package redis

import (
    "bufio"
    "bytes"
    "container/vector"
    "fmt"
    "io"
    "io/ioutil"
    "net"
    "os"
    "reflect"
    "strconv"
    "strings"
)

const (
    MaxPoolSize = 5
)

var defaultAddr = "127.0.0.1:7379"

type Client struct {
    Addr     string
    Db       int
    Password string
    //the connection pool
    pool chan net.Conn
}

type RedisError string

func (err RedisError) String() string { return "Redis Error: " + string(err) }

var doesNotExist = RedisError("Key does not exist ")

// reads a bulk reply (i.e $5\r\nhello)
func readBulk(reader *bufio.Reader, head string) ([]byte, os.Error) {
    var err os.Error
    var data []byte

    if head == "" {
        head, err = reader.ReadString('\n')
        if err != nil {
            return nil, err
        }
    }
    switch head[0] {
    case ':':
        data = []byte(strings.TrimSpace(head[1:]))

    case '$':
        size, err := strconv.Atoi(strings.TrimSpace(head[1:]))
        if err != nil {
            return nil, err
        }
        if size == -1 {
            return nil, doesNotExist
        }
        lr := io.LimitReader(reader, int64(size))
        data, err = ioutil.ReadAll(lr)
        if err == nil {
            // read end of line
            _, err = reader.ReadString('\n')
        }
    default:
        return nil, RedisError("Expecting Prefix '$' or ':'")
    }

    return data, err
}

func writeRequest(writer io.Writer, cmd string, args ...string) os.Error {
    b := commandBytes(cmd, args...)
    _, err := writer.Write(b)
    return err
}

func commandBytes(cmd string, args ...string) []byte {
    cmdbuf := bytes.NewBufferString(fmt.Sprintf("*%d\r\n$%d\r\n%s\r\n", len(args)+1, len(cmd), cmd))
    for _, s := range args {
        cmdbuf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))
    }
    return cmdbuf.Bytes()
}

func readResponse(reader *bufio.Reader) (interface{}, os.Error) {

    var line string
    var err os.Error

    //read until the first non-whitespace line
    for {
        line, err = reader.ReadString('\n')
        if len(line) == 0 || err != nil {
            return nil, err
        }
        line = strings.TrimSpace(line)
        if len(line) > 0 {
            break
        }
    }

    if line[0] == '+' {
        return strings.TrimSpace(line[1:]), nil
    }

    if strings.HasPrefix(line, "-ERR ") {
        errmesg := strings.TrimSpace(line[5:])
        return nil, RedisError(errmesg)
    }

    if line[0] == ':' {
        n, err := strconv.Atoi64(strings.TrimSpace(line[1:]))
        if err != nil {
            return nil, RedisError("Int reply is not a number")
        }
        return n, nil
    }

    if line[0] == '*' {
        size, err := strconv.Atoi(strings.TrimSpace(line[1:]))
        if err != nil {
            return nil, RedisError("MultiBulk reply expected a number")
        }
        if size <= 0 {
            return make([][]byte, 0), nil
        }
        res := make([][]byte, size)
        for i := 0; i < size; i++ {
            res[i], err = readBulk(reader, "")
            if err == doesNotExist {
                continue
            }
            if err != nil {
                return nil, err
            }
            // dont read end of line as might not have been bulk
        }
        return res, nil
    }
    return readBulk(reader, line)
}

func (client *Client) rawSend(c net.Conn, cmd []byte) (interface{}, os.Error) {
    _, err := c.Write(cmd)
    if err != nil {
        return nil, err
    }

    reader := bufio.NewReader(c)

    data, err := readResponse(reader)
    if err != nil {
        return nil, err
    }

    return data, nil
}

func (client *Client) openConnection() (c net.Conn, err os.Error) {

    var addr = defaultAddr

    if client.Addr != "" {
        addr = client.Addr
    }
    c, err = net.Dial("tcp", addr)
    if err != nil {
        return
    }

    if client.Db != 0 {
        cmd := fmt.Sprintf("SELECT %d\r\n", client.Db)
        _, err = client.rawSend(c, []byte(cmd))
        if err != nil {
            return
        }
    }
    //TODO: handle authentication here

    return
}


func (client *Client) sendCommand(cmd string, args ...string) (data interface{}, err os.Error) {
    // grab a connection from the pool
    c, err := client.popCon()
    defer client.pushCon(c)

    if err != nil {
        return data, err
    }

    b := commandBytes(cmd, args...)
    data, err = client.rawSend(c, b)
    if err == os.EOF || err == os.EPIPE {
        c, err = client.openConnection()
        if err != nil {
            return data, err
        }

        data, err = client.rawSend(c, b)
    }

    
    return data, err
}

func (client *Client) sendCommands(cmdArgs <-chan []string, data chan<- interface{}) (err os.Error) {

    // grab a connection from the pool
    c, err := client.popCon()
    // Push nil back onto queue
		defer client.pushCon(nil)

    // Close client and synchronization issues are a nightmare to solve.
		defer c.Close()

    if err != nil {
        return err
    }

    reader := bufio.NewReader(c)

    // Ping first to verify connection is open
    err = writeRequest(c, "PING")

    // On first attempt permit a reconnection attempt
    if err == os.EOF {
        // Looks like we have to open a new connection
        c, err = client.openConnection()
        if err != nil {
            return err
        }
        reader = bufio.NewReader(c)
    } else {
        // Read Ping response
        pong, err := readResponse(reader)
        if pong != "PONG" {
            return RedisError("Unexpected response to PING.")
        }
        if err != nil {
            return err
        }
    }

    errs := make(chan os.Error)

    go func() {
        for cmdArg := range cmdArgs {
            err = writeRequest(c, cmdArg[0], cmdArg[1:]...)
            if err != nil {
                errs <- err
                break
            }
        }
        close(errs)
    }()

    go func() {
        for {
            response, err := readResponse(reader)
            if err != nil {
                errs <- err
                break
            }
            data <- response
        }
        close(errs)
    }()

    // Block until errs channel closes
    for e := range errs {
        err = e
    }

    return err
}

func (client *Client) popCon() (net.Conn, os.Error) {
    if client.pool == nil {
        client.pool = make(chan net.Conn, MaxPoolSize)
        for i := 0; i < MaxPoolSize; i++ {
            //add dummy values to the pool
            client.pool <- nil
        }
    }
    // grab a connection from the pool
    c := <-client.pool

    if c == nil {
        return client.openConnection()
    }
    return c, nil
}

func (client *Client) pushCon(c net.Conn) {
    client.pool <- c
}

// General Commands

func (client *Client) Auth(password string) os.Error {
    _, err := client.sendCommand("AUTH", password)
    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Exists(key string) (bool, os.Error) {
    res, err := client.sendCommand("EXISTS", key)
    if err != nil {
        return false, err
    }
    return res.(int64) == 1, nil
}

func (client *Client) Del(key string) (bool, os.Error) {
    res, err := client.sendCommand("DEL", key)

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Type(key string) (string, os.Error) {
    res, err := client.sendCommand("TYPE", key)

    if err != nil {
        return "", err
    }

    return res.(string), nil
}

func (client *Client) Keys(pattern string) ([]string, os.Error) {
    res, err := client.sendCommand("KEYS", pattern)

    if err != nil {
        return nil, err
    }

    var ok bool
    var keydata [][]byte

    if keydata, ok = res.([][]byte); ok {
        // key data is already a double byte array
    } else {
        keydata = bytes.Fields(res.([]byte))
    }
    ret := make([]string, len(keydata))
    for i, k := range keydata {
        ret[i] = string(k)
    }
    return ret, nil
}

func (client *Client) Randomkey() (string, os.Error) {
    res, err := client.sendCommand("RANDOMKEY")
    if err != nil {
        return "", err
    }
    return res.(string), nil
}


func (client *Client) Rename(src string, dst string) os.Error {
    _, err := client.sendCommand("RENAME", src, dst)
    if err != nil {
        return err
    }
    return nil
}

func (client *Client) Renamenx(src string, dst string) (bool, os.Error) {
    res, err := client.sendCommand("RENAMENX", src, dst)
    if err != nil {
        return false, err
    }
    return res.(int64) == 1, nil
}

func (client *Client) Dbsize() (int, os.Error) {
    res, err := client.sendCommand("DBSIZE")
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Expire(key string, time int64) (bool, os.Error) {
    res, err := client.sendCommand("EXPIRE", key, strconv.Itoa64(time))

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Ttl(key string) (int64, os.Error) {
    res, err := client.sendCommand("TTL", key)
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Move(key string, dbnum int) (bool, os.Error) {
    res, err := client.sendCommand("MOVE", key, strconv.Itoa(dbnum))

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Flush(all bool) os.Error {
    var cmd string
    if all {
        cmd = "FLUSHALL"
    } else {
        cmd = "FLUSHDB"
    }
    _, err := client.sendCommand(cmd)
    if err != nil {
        return err
    }
    return nil
}

// String-related commands

func (client *Client) Set(key string, val []byte) os.Error {
    _, err := client.sendCommand("SET", key, string(val))

    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Get(key string) ([]byte, os.Error) {
    res, _ := client.sendCommand("GET", key)
    if res == nil {
        return nil, RedisError("Key `" + key + "` does not exist")
    }

    data := res.([]byte)
    return data, nil
}

func (client *Client) Getset(key string, val []byte) ([]byte, os.Error) {
    res, err := client.sendCommand("GETSET", key, string(val))

    if err != nil {
        return nil, err
    }

    data := res.([]byte)
    return data, nil
}

func (client *Client) Mget(keys ...string) ([][]byte, os.Error) {
    res, err := client.sendCommand("MGET", keys...)
    if err != nil {
        return nil, err
    }

    data := res.([][]byte)
    return data, nil
}

func (client *Client) Setnx(key string, val []byte) (bool, os.Error) {
    res, err := client.sendCommand("SETNX", key, string(val))

    if err != nil {
        return false, err
    }
    if data, ok := res.(int64); ok {
        return data == 1, nil
    }
    return false, RedisError("Unexpected reply to SETNX")
}

func (client *Client) Setex(key string, time int64, val []byte) os.Error {
    _, err := client.sendCommand("SETEX", key, strconv.Itoa64(time), string(val))

    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Mset(mapping map[string][]byte) os.Error {
    args := make([]string, len(mapping)*2)
    i := 0
    for k, v := range mapping {
        args[i] = k
        args[i+1] = string(v)
        i += 2
    }
    _, err := client.sendCommand("MSET", args...)
    if err != nil {
        return err
    }
    return nil
}

func (client *Client) Msetnx(mapping map[string][]byte) (bool, os.Error) {
    args := make([]string, len(mapping)*2)
    i := 0
    for k, v := range mapping {
        args[i] = k
        args[i+1] = string(v)
        i += 2
    }
    res, err := client.sendCommand("MSETNX", args...)
    if err != nil {
        return false, err
    }
    if data, ok := res.(int64); ok {
        return data == 0, nil
    }
    return false, RedisError("Unexpected reply to MSETNX")
}

func (client *Client) Incr(key string) (int64, os.Error) {
    res, err := client.sendCommand("INCR", key)
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Incrby(key string, val int64) (int64, os.Error) {
    res, err := client.sendCommand("INCRBY", key, strconv.Itoa64(val))
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Decr(key string) (int64, os.Error) {
    res, err := client.sendCommand("DECR", key)
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Decrby(key string, val int64) (int64, os.Error) {
    res, err := client.sendCommand("DECRBY", key, strconv.Itoa64(val))
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Append(key string, val []byte) os.Error {
    _, err := client.sendCommand("APPEND", key, string(val))

    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Substr(key string, start int, end int) ([]byte, os.Error) {
    res, _ := client.sendCommand("SUBSTR", key, strconv.Itoa(start), strconv.Itoa(end))

    if res == nil {
        return nil, RedisError("Key `" + key + "` does not exist")
    }

    data := res.([]byte)
    return data, nil
}

// List commands

func (client *Client) Rpush(key string, val []byte) os.Error {
    _, err := client.sendCommand("RPUSH", key, string(val))

    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Lpush(key string, val []byte) os.Error {
    _, err := client.sendCommand("LPUSH", key, string(val))

    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Llen(key string) (int, os.Error) {
    res, err := client.sendCommand("LLEN", key)
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Lrange(key string, start int, end int) ([][]byte, os.Error) {
    res, err := client.sendCommand("LRANGE", key, strconv.Itoa(start), strconv.Itoa(end))
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Ltrim(key string, start int, end int) os.Error {
    _, err := client.sendCommand("LTRIM", key, strconv.Itoa(start), strconv.Itoa(end))
    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Lindex(key string, index int) ([]byte, os.Error) {
    res, err := client.sendCommand("LINDEX", key, strconv.Itoa(index))
    if err != nil {
        return nil, err
    }

    return res.([]byte), nil
}

func (client *Client) Lset(key string, index int, value []byte) os.Error {
    _, err := client.sendCommand("LSET", key, strconv.Itoa(index), string(value))
    if err != nil {
        return err
    }

    return nil
}

func (client *Client) Lrem(key string, index int) (int, os.Error) {
    res, err := client.sendCommand("LREM", key, strconv.Itoa(index))
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Lpop(key string) ([]byte, os.Error) {
    res, err := client.sendCommand("LPOP", key)
    if err != nil {
        return nil, err
    }

    return res.([]byte), nil
}

func (client *Client) Rpop(key string) ([]byte, os.Error) {
    res, err := client.sendCommand("RPOP", key)
    if err != nil {
        return nil, err
    }

    return res.([]byte), nil
}

func (client *Client) Blpop(keys []string, timeoutSecs uint) (*string, []byte, os.Error) {
    return client.bpop("BLPOP", keys, timeoutSecs)
}
func (client *Client) Brpop(keys []string, timeoutSecs uint) (*string, []byte, os.Error) {
    return client.bpop("BRPOP", keys, timeoutSecs)
}

func (client *Client) bpop(cmd string, keys []string, timeoutSecs uint) (*string, []byte, os.Error) {
    args := append(keys, strconv.Uitoa(timeoutSecs))
    res, err := client.sendCommand(cmd, args...)
    if err != nil {
        return nil, nil, err
    }
    kv := res.([][]byte)
    // Check for timeout
    if len(kv) != 2 {
        return nil, nil, nil
    }
    k := string(kv[0])
    v := kv[1]
    return &k, v, nil
}

func (client *Client) Rpoplpush(src string, dst string) ([]byte, os.Error) {
    res, err := client.sendCommand("RPOPLPUSH", src, dst)
    if err != nil {
        return nil, err
    }

    return res.([]byte), nil
}

// Set commands

func (client *Client) Sadd(key string, value []byte) (bool, os.Error) {
    res, err := client.sendCommand("SADD", key, string(value))

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Srem(key string, value []byte) (bool, os.Error) {
    res, err := client.sendCommand("SREM", key, string(value))

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Spop(key string) ([]byte, os.Error) {
    res, err := client.sendCommand("SPOP", key)
    if err != nil {
        return nil, err
    }

    if res == nil {
        return nil, RedisError("Spop failed")
    }

    data := res.([]byte)
    return data, nil
}

func (client *Client) Smove(src string, dst string, val []byte) (bool, os.Error) {
    res, err := client.sendCommand("SMOVE", src, dst, string(val))
    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Scard(key string) (int, os.Error) {
    res, err := client.sendCommand("SCARD", key)
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Sismember(key string, value []byte) (bool, os.Error) {
    res, err := client.sendCommand("SISMEMBER", key, string(value))

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Sinter(keys ...string) ([][]byte, os.Error) {
    res, err := client.sendCommand("SINTER", keys...)
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Sinterstore(dst string, keys ...string) (int, os.Error) {
    args := make([]string, len(keys)+1)
    args[0] = dst
    copy(args[1:], keys)
    res, err := client.sendCommand("SINTERSTORE", args...)
    if err != nil {
        return 0, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Sunion(keys ...string) ([][]byte, os.Error) {
    res, err := client.sendCommand("SUNION", keys...)
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Sunionstore(dst string, keys ...string) (int, os.Error) {
    args := make([]string, len(keys)+1)
    args[0] = dst
    copy(args[1:], keys)
    res, err := client.sendCommand("SUNIONSTORE", args...)
    if err != nil {
        return 0, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Sdiff(key1 string, keys []string) ([][]byte, os.Error) {
    args := make([]string, len(keys)+1)
    args[0] = key1
    copy(args[1:], keys)
    res, err := client.sendCommand("SDIFF", args...)
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Sdiffstore(dst string, key1 string, keys []string) (int, os.Error) {
    args := make([]string, len(keys)+2)
    args[0] = dst
    args[1] = key1
    copy(args[2:], keys)
    res, err := client.sendCommand("SDIFFSTORE", args...)
    if err != nil {
        return 0, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Smembers(key string) ([][]byte, os.Error) {
    res, err := client.sendCommand("SMEMBERS", key)

    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Srandmember(key string) ([]byte, os.Error) {
    res, err := client.sendCommand("SRANDMEMBER", key)
    if err != nil {
        return nil, err
    }

    return res.([]byte), nil
}

// sorted set commands

func (client *Client) Zadd(key string, value []byte, score float64) (bool, os.Error) {
    res, err := client.sendCommand("ZADD", key, strconv.Ftoa64(score, 'f', -1), string(value))
    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Zrem(key string, value []byte) (bool, os.Error) {
    res, err := client.sendCommand("ZREM", key, string(value))
    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Zincrby(key string, value []byte, score float64) (float64, os.Error) {
    res, err := client.sendCommand("ZINCRBY", key, strconv.Ftoa64(score, 'f', -1), string(value))
    if err != nil {
        return 0, err
    }

    data := string(res.([]byte))
    f, _ := strconv.Atof64(data)
    return f, nil
}

func (client *Client) Zrank(key string, value []byte) (int, os.Error) {
    res, err := client.sendCommand("ZRANK", key, string(value))
    if err != nil {
        return 0, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Zrevrank(key string, value []byte) (int, os.Error) {
    res, err := client.sendCommand("ZREVRANK", key, string(value))
    if err != nil {
        return 0, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Zrange(key string, start int, end int) ([][]byte, os.Error) {
    res, err := client.sendCommand("ZRANGE", key, strconv.Itoa(start), strconv.Itoa(end))
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Zrevrange(key string, start int, end int) ([][]byte, os.Error) {
    res, err := client.sendCommand("ZREVRANGE", key, strconv.Itoa(start), strconv.Itoa(end))
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Zrangebyscore(key string, start float64, end float64) ([][]byte, os.Error) {
    res, err := client.sendCommand("ZRANGEBYSCORE", key, strconv.Ftoa64(start, 'f', -1), strconv.Ftoa64(end, 'f', -1))
    if err != nil {
        return nil, err
    }

    return res.([][]byte), nil
}

func (client *Client) Zcard(key string) (int, os.Error) {
    res, err := client.sendCommand("ZCARD", key)
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Zscore(key string, member []byte) (float64, os.Error) {
    res, err := client.sendCommand("ZSCORE", key, string(member))
    if err != nil {
        return 0, err
    }

    data := string(res.([]byte))
    f, _ := strconv.Atof64(data)
    return f, nil
}

func (client *Client) Zremrangebyrank(key string, start int, end int) (int, os.Error) {
    res, err := client.sendCommand("ZREMRANGEBYRANK", key, strconv.Itoa(start), strconv.Itoa(end))
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Zremrangebyscore(key string, start float64, end float64) (int, os.Error) {
    res, err := client.sendCommand("ZREMRANGEBYSCORE", key, strconv.Ftoa64(start, 'f', -1), strconv.Ftoa64(end, 'f', -1))
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

// hash commands

func (client *Client) Hset(key string, field string, val []byte) (bool, os.Error) {
    res, err := client.sendCommand("HSET", key, field, string(val))
    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Hget(key string, field string) ([]byte, os.Error) {
    res, _ := client.sendCommand("HGET", key, field)

    if res == nil {
        return nil, RedisError("Hget failed")
    }

    data := res.([]byte)
    return data, nil
}

//pretty much copy the json code from here.

func valueToString(v reflect.Value) (string, os.Error) {
    if !v.IsValid() {
        return "null", nil
    }

    switch v.Kind() {
    case reflect.Ptr:
        return valueToString(reflect.Indirect(v))
    case reflect.Interface:
        return valueToString(v.Elem())
    case reflect.Bool:
        x := v.Bool()
        if x {
            return "true", nil
        } else {
            return "false", nil
        }

    case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
        return strconv.Itoa64(v.Int()), nil
    case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
        return strconv.Uitoa64(v.Uint()), nil
    case reflect.UnsafePointer:
        return strconv.Uitoa64(uint64(v.Pointer())), nil

    case reflect.Float32, reflect.Float64:
        return strconv.Ftoa64(v.Float(), 'g', -1), nil

    case reflect.String:
        return v.String(), nil

    //This is kind of a rough hack to replace the old []byte
    //detection with reflect.Uint8Type, it doesn't catch
    //zero-length byte slices
    case reflect.Slice:
        typ := v.Type()
        if typ.Elem().Kind() == reflect.Uint || typ.Elem().Kind() == reflect.Uint8 || typ.Elem().Kind() == reflect.Uint16 || typ.Elem().Kind() == reflect.Uint32 || typ.Elem().Kind() == reflect.Uint64 || typ.Elem().Kind() == reflect.Uintptr {
            if v.Len() > 0 {
                if v.Index(1).OverflowUint(257) {
                    return string(v.Interface().([]byte)), nil
                }
            }
        }
    }
    return "", os.NewError("Unsupported type")
}

func containerToString(val reflect.Value, args *vector.StringVector) os.Error {
    switch v := val; v.Kind() {
    case reflect.Ptr:
        return containerToString(reflect.Indirect(v), args)
    case reflect.Interface:
        return containerToString(v.Elem(), args)
    case reflect.Map:
        if v.Type().Key().Kind() != reflect.String {
            return os.NewError("Unsupported type - map key must be a string")
        }
        for _, k := range v.MapKeys() {
            args.Push(k.String())
            s, err := valueToString(v.MapIndex(k))
            if err != nil {
                return err
            }
            args.Push(s)
        }
    case reflect.Struct:
        st := v.Type()
        for i := 0; i < st.NumField(); i++ {
            ft := st.FieldByIndex([]int{i})
            args.Push(ft.Name)
            s, err := valueToString(v.FieldByIndex([]int{i}))
            if err != nil {
                return err
            }
            args.Push(s)
        }
    }
    return nil
}

func (client *Client) Hmset(key string, mapping interface{}) os.Error {
    args := new(vector.StringVector)
    args.Push(key)
    err := containerToString(reflect.ValueOf(mapping), args)
    if err != nil {
        return err
    }
    _, err = client.sendCommand("HMSET", *args...)
    if err != nil {
        return err
    }
    return nil
}

func (client *Client) Hincrby(key string, field string, val int64) (int64, os.Error) {
    res, err := client.sendCommand("HINCRBY", key, field, strconv.Itoa64(val))
    if err != nil {
        return -1, err
    }

    return res.(int64), nil
}

func (client *Client) Hexists(key string, field string) (bool, os.Error) {
    res, err := client.sendCommand("HEXISTS", key, field)
    if err != nil {
        return false, err
    }
    return res.(int64) == 1, nil
}

func (client *Client) Hdel(key string, field string) (bool, os.Error) {
    res, err := client.sendCommand("HDEL", key, field)

    if err != nil {
        return false, err
    }

    return res.(int64) == 1, nil
}

func (client *Client) Hlen(key string) (int, os.Error) {
    res, err := client.sendCommand("HLEN", key)
    if err != nil {
        return -1, err
    }

    return int(res.(int64)), nil
}

func (client *Client) Hkeys(key string) ([]string, os.Error) {
    res, err := client.sendCommand("HKEYS", key)

    if err != nil {
        return nil, err
    }

    data := res.([][]byte)
    ret := make([]string, len(data))
    for i, k := range data {
        ret[i] = string(k)
    }
    return ret, nil
}

func (client *Client) Hvals(key string) ([][]byte, os.Error) {
    res, err := client.sendCommand("HVALS", key)

    if err != nil {
        return nil, err
    }
    return res.([][]byte), nil
}

func writeTo(data []byte, val reflect.Value) os.Error {
    s := string(data)
    switch v := val; v.Kind() {
    // if we're writing to an interace value, just set the byte data
    // TODO: should we support writing to a pointer?
    case reflect.Interface:
        v.Set(reflect.ValueOf(data))
    case reflect.Bool:
        b, err := strconv.Atob(s)
        if err != nil {
            return err
        }
        v.SetBool(b)
    case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
        i, err := strconv.Atoi64(s)
        if err != nil {
            return err
        }
        v.SetInt(i)
    case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
        ui, err := strconv.Atoui64(s)
        if err != nil {
            return err
        }
        v.SetUint(ui)
    case reflect.Float32, reflect.Float64:
        f, err := strconv.Atof64(s)
        if err != nil {
            return err
        }
        v.SetFloat(f)

    case reflect.String:
        v.SetString(s)
    case reflect.Slice:
        typ := v.Type()
        if typ.Elem().Kind() == reflect.Uint || typ.Elem().Kind() == reflect.Uint8 || typ.Elem().Kind() == reflect.Uint16 || typ.Elem().Kind() == reflect.Uint32 || typ.Elem().Kind() == reflect.Uint64 || typ.Elem().Kind() == reflect.Uintptr {
            v.Set(reflect.ValueOf(data))
        }
    }
    return nil
}

func writeToContainer(data [][]byte, val reflect.Value) os.Error {
    switch v := val; v.Kind() {
    case reflect.Ptr:
        return writeToContainer(data, reflect.Indirect(v))
    case reflect.Interface:
        return writeToContainer(data, v.Elem())
    case reflect.Map:
        if v.Type().Key().Kind() != reflect.String {
            return os.NewError("Invalid map type")
        }
        elemtype := v.Type().Elem()
        for i := 0; i < len(data)/2; i++ {
            mk := reflect.ValueOf(string(data[i*2]))
            mv := reflect.New(elemtype).Elem()
            writeTo(data[i*2+1], mv)
            v.SetMapIndex(mk, mv)
        }
    case reflect.Struct:
        for i := 0; i < len(data)/2; i++ {
            name := string(data[i*2])
            field := v.FieldByName(name)
            if !field.IsValid() {
                continue
            }
            writeTo(data[i*2+1], field)
        }
    default:
        return os.NewError("Invalid container type")
    }
    return nil
}


func (client *Client) Hgetall(key string, val interface{}) os.Error {
    res, err := client.sendCommand("HGETALL", key)
    if err != nil {
        return err
    }

    data := res.([][]byte)
    if data == nil || len(data) == 0 {
        return RedisError("Key `" + key + "` does not exist")
    }
    err = writeToContainer(data, reflect.ValueOf(val))
    if err != nil {
        return err
    }

    return nil
}

//Publish/Subscribe

// Container for messages received from publishers on channels that we're subscribed to.
type Message struct {
    ChannelMatched string
    Channel        string
    Message        []byte
}

// Subscribe to redis serve channels, this method will block until one of the sub/unsub channels are closed.
// There are two pairs of channels subscribe/unsubscribe & psubscribe/punsubscribe.
// The former does an exact match on the channel, the later uses glob patterns on the redis channels.
// Closing either of these channels will unblock this method call.
// Messages that are received are sent down the messages channel.
func (client *Client) Subscribe(subscribe <-chan string, unsubscribe <-chan string, psubscribe <-chan string, punsubscribe <-chan string, messages chan<- Message) os.Error {
    cmds := make(chan []string, 0)
    data := make(chan interface{}, 0)

    go func() {
        for {
            var channel string
            var cmd string

            select {
            case channel = <-subscribe:
                cmd = "SUBSCRIBE"
            case channel = <-unsubscribe:
                cmd = "UNSUBSCRIBE"
            case channel = <-psubscribe:
                cmd = "PSUBSCRIBE"
            case channel = <-punsubscribe:
                cmd = "UNPSUBSCRIBE"

            }
            if channel == "" {
                break
            } else {
                cmds <- []string{cmd, channel}
            }
        }
        close(cmds)
        close(data)
    }()

    go func() {
        for response := range data {
            db := response.([][]byte)
            messageType := string(db[0])
            switch messageType {
            case "message":
                channel, message := string(db[1]), db[2]
                messages <- Message{channel, channel, message}
            case "subscribe":
                // Ignore
            case "unsubscribe":
                // Ignore
            case "pmessage":
                channelMatched, channel, message := string(db[1]), string(db[2]), db[3]
                messages <- Message{channelMatched, channel, message}
            case "psubscribe":
                // Ignore
            case "punsubscribe":
                // Ignore

            default:
                // log.Printf("Unknown message '%s'", messageType)
            }
        }
    }()

    err := client.sendCommands(cmds, data)

    return err
}

// Publish a message to a redis server.
func (client *Client) Publish(channel string, val []byte) os.Error {
    _, err := client.sendCommand("PUBLISH", channel, string(val))
    if err != nil {
        return err
    }
    return nil
}

//Server commands

func (client *Client) Save() os.Error {
    _, err := client.sendCommand("SAVE")
    if err != nil {
        return err
    }
    return nil
}

func (client *Client) Bgsave() os.Error {
    _, err := client.sendCommand("BGSAVE")
    if err != nil {
        return err
    }
    return nil
}

func (client *Client) Lastsave() (int64, os.Error) {
    res, err := client.sendCommand("LASTSAVE")
    if err != nil {
        return 0, err
    }

    return res.(int64), nil
}

func (client *Client) Bgrewriteaof() os.Error {
    _, err := client.sendCommand("BGREWRITEAOF")
    if err != nil {
        return err
    }
    return nil
}
