package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/angryfoxsu/angryfoxdb/lockring"
	"github.com/angryfoxsu/levigo"
	"github.com/angryfoxsu/bconv"
)

var KeyMutex = lockring.New(1024)

// Key/Value type identifiers, only append to this list
const (
	MetaKey byte = iota
	StringKey
	HashKey
	ListKey
	SetKey
	ZSetKey
	ZScoreKey
	StringLengthValue
	HashLengthValue
	ListLengthValue
	SetCardValue
	ZCardValue
)

var (
	InvalidKeyTypeError = fmt.Errorf("Operation against a key holding the wrong kind of value")
	InvalidDataError    = fmt.Errorf("Invalid data")
	InvalidIntError     = fmt.Errorf("value is not an integer or out of range")
	SyntaxError         = fmt.Errorf("syntax error")
)

var (
	ReplyOK    = rawReply("+OK\r\n")
	ReplyPONG  = rawReply("+PONG\r\n")
	ReplyNOKEY = rawReply("+NOKEY\r\n")
)

type IOError struct{ error }

// if the number of items is known before the items,
// they do not need to be buffered into memory, and can be streamed over a channel
type cmdReplyStream struct {
	size  int64            // the number of items that will be sent
	items chan interface{} // a multi-bulk reply item, one of nil, []byte, or int
}

// A raw reply will be returned verbatim to the client
type rawReply []byte

// cmdFunc response to Redis protocol conversion:
//
// string - single line reply, automatically prefixed with "+"
// error - error message, automatically prefixed with "-"
// int - integer number, automatically encoded and prefixed with ":"
// []byte - bulk reply, automatically prefixed with the length like "$3\r\n"
// nil, nil []byte - nil response, encoded as "$-1\r\n"
// rawReply - no serialization, returned verbatim
// []interface{} - multi-bulk reply, automatically serialized, members can be nil, []byte, or int
// nil []interface{} - nil multi-bulk reply, serialized as "*-1\r\n"
// map[string]bool - multi-bulk reply (used by SUNION)
// *cmdReplyStream - multi-bulk reply sent over a channel
type cmdFunc func(args [][]byte, wb *levigo.WriteBatch) interface{}

type cmdDesc struct {
	name      string
	function  cmdFunc
	arity     int                     // the number of required arguments, -n means >= n
	writes    bool                    // false if the command doesn't write data (the WriteBatch will not be passed in)
	firstKey  int                     // first argument that is a key (-1 for none)
	lastKey   int                     // last argument that is a key (-1 for unbounded)
	keyStep   int                     // step to get all the keys from first to last. For instance MSET is 2 since the arguments are KEY VAL KEY VAL...
	keyLookup func([][]byte) [][]byte // function that extracts the keys from the args
}

var commandList = []cmdDesc{
	{"del", Del, -1, true, 0, -1, 1, nil},
	{"echo", Echo, 1, false, -1, 0, 0, nil},
	{"exists", Exists, 1, false, 0, 0, 0, nil},
	{"get", Get, 1, false, 0, 0, 0, nil},
	{"hdel", Hdel, -2, true, 0, 0, 0, nil},
	{"hexists", Hexists, 2, false, 0, 0, 0, nil},
	{"hget", Hget, 2, false, 0, 0, 0, nil},
	{"hgetall", Hgetall, 1, false, 0, 0, 0, nil},
	{"hincrby", Hincrby, 3, true, 0, 0, 0, nil},
	{"hincrbyfloat", Hincrbyfloat, 3, true, 0, 0, 0, nil},
	{"hkeys", Hkeys, 1, false, 0, 0, 0, nil},
	{"hlen", Hlen, 1, false, 0, 0, 0, nil},
	{"hmget", Hmget, -2, false, 0, 0, 0, nil},
	{"hmset", Hmset, -3, true, 0, 0, 0, nil},
	{"hset", Hset, 3, true, 0, 0, 0, nil},
	{"hsetnx", Hsetnx, 3, true, 0, 0, 0, nil},
	{"hvals", Hvals, 1, false, 0, 0, 0, nil},
	{"keys", Keys, 1, false, -1, 0, 0, nil},
	{"llen", Llen, 1, false, 0, 0, 0, nil},
	{"lpush", Lpush, -2, true, 0, 0, 0, nil},
	{"lpushx", Lpushx, 2, true, 0, 0, 0, nil},
	{"rpush", Rpush, -2, true, 0, 0, 0, nil},
	{"rpushx", Rpushx, 2, true, 0, 0, 0, nil},
	{"lpop", Lpop, 1, true, 0, 0, 0, nil},
	{"rpop", Rpop, 1, true, 0, 0, 0, nil},
	{"rpoplpush", Rpoplpush, 2, true, 0, 1, 0, nil},
	{"lrange", Lrange, 3, false, 0, 0, 0, nil},
	{"ping", Ping, 0, false, -1, 0, 0, nil},
	{"append", Append, 2, true, 0, 0, 0, nil},
	{"set", Set, 2, true, 0, 0, 0, nil},
	{"sadd", Sadd, -2, true, 0, 0, 0, nil},
	{"scard", Scard, 1, false, 0, 0, 0, nil},
	{"sismember", Sismember, 2, false, 0, 0, 0, nil},
	{"smembers", Smembers, 1, false, 0, 0, 0, nil},
	{"smove", Smove, 3, true, 0, 1, 0, nil},
	{"spop", Spop, 1, true, 0, 0, 0, nil},
	{"srem", Srem, -2, true, 0, 0, 0, nil},
	{"sunion", Sunion, -1, false, 0, -1, 1, nil},
	{"sunionstore", Sunionstore, -2, true, 0, -1, 1, nil},
	{"sinter", Sinter, -1, false, 0, -1, 1, nil},
	{"sinterstore", Sinterstore, -2, true, 0, -1, 1, nil},
	{"sdiff", Sdiff, -1, false, 0, -1, 1, nil},
	{"sdiffstore", Sdiffstore, -2, true, 0, -1, 1, nil},
	{"time", Time, 0, false, -1, 0, 0, nil},
	{"type", Type, 1, false, 0, 0, 0, nil},
	{"zadd", Zadd, -3, true, 0, 0, 0, nil},
	{"zcard", Zcard, 1, false, 0, 0, 0, nil},
	{"zincrby", Zincrby, 3, true, 0, 0, 0, nil},
	{"zrange", Zrange, -3, false, 0, 0, 0, nil},
	{"zrem", Zrem, -2, true, 0, 0, 0, nil},
	{"zrevrange", Zrevrange, -3, false, 0, 0, 0, nil},
	{"zrangebyscore", Zrangebyscore, -3, false, 0, 0, 0, nil},
	{"zrevrangebyscore", Zrevrangebyscore, -3, false, 0, 0, 0, nil},
	{"zremrangebyscore", Zremrangebyscore, 3, true, 0, 0, 0, nil},
	{"zcount", Zcount, 3, false, 0, 0, 0, nil},
	{"zscore", Zscore, 2, false, 0, 0, 0, nil},
	{"zrank", Zrank, 2, false, 0, 0, 0, nil},
	{"zrevrank", Zrevrank, 2, false, 0, 0, 0, nil},
	{"zunionstore", Zunionstore, -3, true, 0, 0, 0, ZunionInterKeys},
	{"zinterstore", Zinterstore, -3, true, 0, 0, 0, ZunionInterKeys},
	{"restore", Restore, 3, true, 0, 0, 0, nil},
	{"dump", Dump, 1, false, 0, 0, 0, nil},
	{"migrate", Migrate, 5, true, 2, 2, 0, nil},
	{"select", Select, 1, false, 0, 0, 0, nil},
}

// extract the keys from the command args
func (c *cmdDesc) getKeys(args [][]byte) [][]byte {
	// if a key lookup function was specified, use it
	if c.keyLookup != nil {
		return c.keyLookup(args)
	}
	// if no keys are expected, or the argument with the first key doesn't exist
	if c.firstKey < 0 || len(args) <= c.firstKey {
		return nil
	}
	// shortcut: if the keystep is 0 or 1, we can slice the array
	if c.keyStep <= 1 {
		return args[c.firstKey : c.lastKey+1]
	}
	keys := make([][]byte, 0, 1)
keyloop:
	for i := c.firstKey; i < len(args) && (c.lastKey == -1 || i <= c.lastKey); i += c.keyStep {
		for _, k := range keys {
			// skip keys that are already in the array
			if bytes.Equal(k, args[i]) {
				continue keyloop
			}
		}
		keys = append(keys, args[i])
	}
	return keys
}

// acquires a read or write lock for the keys in arguments using the cmdDesc
func (c *cmdDesc) lockKeys(args [][]byte) {
	if !c.writes {
		return
	}
	for _, k := range c.getKeys(args) {
		KeyMutex.Lock(k)
	}
}

func (c *cmdDesc) unlockKeys(args [][]byte) {
	if !c.writes {
		return
	}
	for _, k := range c.getKeys(args) {
		KeyMutex.Unlock(k)
	}
}

var commands = make(map[string]cmdDesc, len(commandList))

func Ping(args [][]byte, wb *levigo.WriteBatch) interface{} {
	return ReplyPONG
}

func Echo(args [][]byte, wb *levigo.WriteBatch) interface{} {
	return args[0]
}

func Time(args [][]byte, wb *levigo.WriteBatch) interface{} {
	now := time.Now()
	secs := strconv.AppendInt(nil, now.Unix(), 10)
	micros := strconv.AppendInt(nil, int64(now.Nanosecond()/1000), 10)
	return []interface{}{secs, micros}
}

func Exists(args [][]byte, wb *levigo.WriteBatch) interface{} {
	res, err := DB.Get(DefaultReadOptions, metaKey(args[0]))
	if err != nil {
		return err
	}
	if res == nil {
		return 0
	}
	return 1
}

func Type(args [][]byte, wb *levigo.WriteBatch) interface{} {
	res, err := DB.Get(DefaultReadOptions, metaKey(args[0]))
	if err != nil {
		return err
	}
	if res == nil {
		return "none"
	}
	if len(res) == 0 {
		return InvalidDataError
	}
	switch res[0] {
	case StringLengthValue:
		return "string"
	case HashLengthValue:
		return "hash"
	case ListLengthValue:
		return "list"
	case SetCardValue:
		return "set"
	case ZCardValue:
		return "zset"
	}
	panic("unknown type")
}

func Keys(args [][]byte, wb *levigo.WriteBatch) interface{} {
	it := DB.NewIterator(ReadWithoutCacheFill)
	defer it.Close()
	keys := []interface{}{}
	pattern := string(args[0])

	for it.Seek([]byte{MetaKey}); it.Valid(); it.Next() {
		k := it.Key()
		// if the first byte isn't MetaKey, we've reached the end
		if len(k) < 2 || k[0] != MetaKey {
			break
		}
		// filepatch.Match() implements the same pattern syntax as we want
		matched, err := filepath.Match(pattern, string(k[1:]))
		if err != nil {
			return fmt.Errorf("invalid pattern for 'keys' command")
		}
		if matched {
			keys = append(keys, k[1:])
		}
	}
	return keys
}

// No-op for now
func Select(args [][]byte, wb *levigo.WriteBatch) interface{} {
	return ReplyOK
}

func Del(args [][]byte, wb *levigo.WriteBatch) interface{} {
	deleted := 0
	k := make([]byte, 1, len(args[0])) // make a reusable slice with room for the first metakey

	for _, key := range args {
		k = bufMetaKey(k, key)
		d, err := delKey(k, wb)
		if err != nil {
			return err
		}
		if d {
			deleted++
		}
	}
	return deleted
}

func delKey(key []byte, wb *levigo.WriteBatch) (deleted bool, err error) {
	res, err := DB.Get(ReadWithoutCacheFill, key)
	if err != nil {
		return
	}
	if res == nil {
		return
	}
	if len(res) < 1 {
		return false, InvalidDataError
	}
	del(key[1:], res[0], wb)
	wb.Delete(key)
	return true, nil
}

func del(key []byte, t byte, wb *levigo.WriteBatch) {
	switch t {
	case StringLengthValue:
		DelString(key, wb)
	case HashLengthValue:
		DelHash(key, wb)
	case SetCardValue:
		DelSet(key, wb)
	case ZCardValue:
		DelZset(key, wb)
	default:
		panic("unknown key type")
	}
}

// set buf to the metaKey for key
func bufMetaKey(buf []byte, key []byte) []byte {
	buf[0] = MetaKey
	return append(buf[:1], key...)
}

func metaKey(k []byte) []byte {
	key := make([]byte, 1+len(k))
	key[0] = MetaKey
	copy(key[1:], k)
	return key
}

func parseRange(args [][]byte, length int64) (int64, int64, error) {
	start, err := bconv.ParseInt(args[0], 10, 64)
	end, err2 := bconv.ParseInt(args[1], 10, 64)
	if err != nil || err2 != nil {
		return 0, 0, InvalidIntError
	}

	// if the index is negative, it is counting from the end,
	// so add it to the length to get the absolute index
	if start < 0 {
		start += length
	}
	if end < 0 {
		end += length
	}

	if end > length { // limit the end to the last member
		end = length - 1
	}

	return start, end, nil
}

func init() {
	for _, c := range commandList {
		commands[c.name] = c
	}
}

// Keys
// EXPIRE
// EXPIREAT
// MOVE
// OBJECT?
// PERSIST
// PEXPIRE
// PEXPIREAT
// PTTL
// RANDOMKEY
// RENAME
// RENAMENX
// SORT
// TTL
// TYPE
//
// Pub/Sub
// PSUBSCRIBE
// PUNSUBSCRIBE
// UNSUBSCRIBE
// PUBLISH
// SUBSCRIBE
//
// Transactions
// DISCARD
// MULTI
// EXEC
// WATCH
// UNWATCH
//
// Scripting
// EVAL
// EVALSHA
// SCRIPT EXISTS
// SCRIPT KILL
// SCRIPT FLUSH
// SCRIPT LOAD
//
// Connection
// AUTH
// SELECT
// QUIT
//
// Server
// FLUSHALL
// FLUSHDB
// SYNC
// CONFIG RESETSTAT
// INFO
// DBSIZE
// CLIENT LIST
// CLIENT KILL
// MONITOR
// CONFIG GET
// CONFIG SET
// SLAVEOF
// SHUTDOWN
// SAVE
// SLOWLOG
