package core

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RESP3 replies, and the RESP2 ones they must leave alone.
//
// A RESP3 client picks how to decode a reply by its type, so the shape of every
// reply on a RESP3 connection is part of the contract: the one Redis 8.10.1
// sends. scripts/differential.py --protocol 3 compares the two servers over a
// long random run; these pin the bytes, so a change to either protocol shows up
// here first.

// rawReplyAs runs a command the way a connection that negotiated the given
// protocol would, and returns the reply as written.
func rawReplyAs(t *testing.T, resp3 bool, name string, args ...string) []byte {
	t.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: name, Args: args, RESP3: resp3}, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w.b
}

// replyScript is one session, run in order from an empty keyspace, with the
// exact reply each protocol gives. The RESP2 column was captured before RESP3
// existed here, and RESP2 connections must still get it byte for byte - apart
// from CF.MEXISTS, BF.INFO and CF.INFO, whose RESP2 replies were Keel's own
// and are now RedisBloom's (see redisbloom.go). An empty RESP3 column means
// the reply is the same in both. {n} stands for a number that is not about
// framing - a size in an INFO reply, a SCAN cursor - and matches any.
var replyScript = []struct {
	args         []string
	resp2, resp3 string
}{
	{[]string{"PING"}, "+PONG\r\n", ""},
	{[]string{"PING", "hi"}, "$2\r\nhi\r\n", ""},
	{[]string{"ECHO", "x"}, "$1\r\nx\r\n", ""},
	{[]string{"SELECT", "0"}, "+OK\r\n", ""},
	{[]string{"SET", "s", "v"}, "+OK\r\n", ""},
	{[]string{"SET", "s", "v", "NX"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"SET", "s", "v2", "GET"}, "$1\r\nv\r\n", ""},
	{[]string{"SET", "nx", "v", "XX", "GET"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"SET", "nx", "v", "XX"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"GET", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"GET", "s"}, "$2\r\nv2\r\n", ""},
	{[]string{"SETNX", "s", "x"}, ":0\r\n", ""},
	{[]string{"SETNX", "n", "x"}, ":1\r\n", ""},
	{[]string{"MGET", "s", "missing", "n"}, "*3\r\n$2\r\nv2\r\n$-1\r\n$1\r\nx\r\n",
		"*3\r\n$2\r\nv2\r\n_\r\n$1\r\nx\r\n"},
	{[]string{"MSET", "m1", "1", "m2", "2"}, "+OK\r\n", ""},
	{[]string{"INCR", "m1"}, ":2\r\n", ""},
	{[]string{"DECRBY", "m2", "5"}, ":-3\r\n", ""},
	{[]string{"SETEX", "e", "100", "v"}, "+OK\r\n", ""},
	{[]string{"PERSIST", "e"}, ":1\r\n", ""},
	{[]string{"TTL", "e"}, ":-1\r\n", ""},
	{[]string{"PTTL", "missing"}, ":-2\r\n", ""},
	{[]string{"EXPIRE", "missing", "10"}, ":0\r\n", ""},
	{[]string{"TYPE", "s"}, "+string\r\n", ""},
	{[]string{"TYPE", "missing"}, "+none\r\n", ""},
	{[]string{"EXISTS", "s", "missing"}, ":1\r\n", ""},
	{[]string{"DEL", "n"}, ":1\r\n", ""},
	{[]string{"UNLINK", "missing"}, ":0\r\n", ""},
	{[]string{"SET", "a1", "ohmytext"}, "+OK\r\n", ""},
	{[]string{"SET", "b1", "mynewtext"}, "+OK\r\n", ""},
	{[]string{"LCS", "a1", "b1"}, "$6\r\nmytext\r\n", ""},
	{[]string{"LCS", "a1", "b1", "LEN"}, ":6\r\n", ""},
	{[]string{"LCS", "a1", "b1", "IDX"}, "*4\r\n$7\r\nmatches\r\n*2\r\n*2\r\n*2\r\n:4\r\n:7\r\n*2\r\n:5\r\n:8\r\n*2\r\n*2\r\n:2\r\n:3\r\n*2\r\n:0\r\n:1\r\n$3\r\nlen\r\n:6\r\n",
		"%2\r\n$7\r\nmatches\r\n*2\r\n*2\r\n*2\r\n:4\r\n:7\r\n*2\r\n:5\r\n:8\r\n*2\r\n*2\r\n:2\r\n:3\r\n*2\r\n:0\r\n:1\r\n$3\r\nlen\r\n:6\r\n"},
	{[]string{"LCS", "a1", "b1", "IDX", "WITHMATCHLEN", "MINMATCHLEN", "4"}, "*4\r\n$7\r\nmatches\r\n*1\r\n*3\r\n*2\r\n:4\r\n:7\r\n*2\r\n:5\r\n:8\r\n:4\r\n$3\r\nlen\r\n:6\r\n",
		"%2\r\n$7\r\nmatches\r\n*1\r\n*3\r\n*2\r\n:4\r\n:7\r\n*2\r\n:5\r\n:8\r\n:4\r\n$3\r\nlen\r\n:6\r\n"},
	{[]string{"HSET", "h", "f", "v"}, ":1\r\n", ""},
	{[]string{"HSETNX", "h", "f", "w"}, ":0\r\n", ""},
	{[]string{"HGETALL", "h"}, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n",
		"%1\r\n$1\r\nf\r\n$1\r\nv\r\n"},
	{[]string{"HGETALL", "missing"}, "*0\r\n",
		"%0\r\n"},
	{[]string{"HKEYS", "h"}, "*1\r\n$1\r\nf\r\n", ""},
	{[]string{"HVALS", "missing"}, "*0\r\n", ""},
	{[]string{"HGET", "h", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"HGET", "h", "f"}, "$1\r\nv\r\n", ""},
	{[]string{"HMGET", "h", "f", "missing"}, "*2\r\n$1\r\nv\r\n$-1\r\n",
		"*2\r\n$1\r\nv\r\n_\r\n"},
	{[]string{"HEXISTS", "h", "f"}, ":1\r\n", ""},
	{[]string{"HLEN", "h"}, ":1\r\n", ""},
	{[]string{"HINCRBY", "h", "c", "3"}, ":3\r\n", ""},
	{[]string{"HDEL", "h", "c"}, ":1\r\n", ""},
	{[]string{"RPUSH", "l", "a", "b", "c"}, ":3\r\n", ""},
	{[]string{"LPUSH", "l", "z"}, ":4\r\n", ""},
	{[]string{"LPOP", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"LPOP", "missing", "2"}, "*-1\r\n",
		"_\r\n"},
	{[]string{"LPOP", "l", "0"}, "*0\r\n", ""},
	{[]string{"LPOP", "l"}, "$1\r\nz\r\n", ""},
	{[]string{"RPOP", "l", "1"}, "*1\r\n$1\r\nc\r\n", ""},
	{[]string{"LINDEX", "l", "9"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"LINDEX", "l", "0"}, "$1\r\na\r\n", ""},
	{[]string{"LRANGE", "l", "0", "-1"}, "*2\r\n$1\r\na\r\n$1\r\nb\r\n", ""},
	{[]string{"LRANGE", "missing", "0", "-1"}, "*0\r\n", ""},
	{[]string{"LSET", "l", "0", "q"}, "+OK\r\n", ""},
	{[]string{"LTRIM", "l", "0", "0"}, "+OK\r\n", ""},
	{[]string{"LLEN", "l"}, ":1\r\n", ""},
	{[]string{"SADD", "st", "a"}, ":1\r\n", ""},
	{[]string{"SMEMBERS", "st"}, "*1\r\n$1\r\na\r\n",
		"~1\r\n$1\r\na\r\n"},
	{[]string{"SMEMBERS", "missing"}, "*0\r\n",
		"~0\r\n"},
	{[]string{"SISMEMBER", "st", "a"}, ":1\r\n", ""},
	{[]string{"SMISMEMBER", "st", "a", "z"}, "*2\r\n:1\r\n:0\r\n", ""},
	{[]string{"SCARD", "st"}, ":1\r\n", ""},
	{[]string{"SPOP", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"SPOP", "missing", "2"}, "*0\r\n",
		"~0\r\n"},
	{[]string{"SPOP", "st", "0"}, "*0\r\n",
		"~0\r\n"},
	{[]string{"SRANDMEMBER", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"SRANDMEMBER", "missing", "2"}, "*0\r\n", ""},
	{[]string{"SRANDMEMBER", "st", "5"}, "*1\r\n$1\r\na\r\n", ""},
	{[]string{"SRANDMEMBER", "st"}, "$1\r\na\r\n", ""},
	{[]string{"SRANDMEMBER", "st", "-2"}, "*2\r\n$1\r\na\r\n$1\r\na\r\n", ""},
	{[]string{"SPOP", "st", "5"}, "*1\r\n$1\r\na\r\n",
		"~1\r\n$1\r\na\r\n"},
	{[]string{"SADD", "st", "b"}, ":1\r\n", ""},
	{[]string{"SPOP", "st"}, "$1\r\nb\r\n", ""},
	{[]string{"SREM", "st", "x"}, ":0\r\n", ""},
	{[]string{"ZADD", "z", "1", "a", "2.5", "b"}, ":2\r\n", ""},
	{[]string{"ZSCORE", "z", "b"}, "$3\r\n2.5\r\n",
		",2.5\r\n"},
	{[]string{"ZSCORE", "z", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"ZINCRBY", "z", "1", "a"}, "$1\r\n2\r\n",
		",2\r\n"},
	{[]string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, "*4\r\n$1\r\na\r\n$1\r\n2\r\n$1\r\nb\r\n$3\r\n2.5\r\n",
		"*2\r\n*2\r\n$1\r\na\r\n,2\r\n*2\r\n$1\r\nb\r\n,2.5\r\n"},
	{[]string{"ZRANGE", "z", "0", "-1"}, "*2\r\n$1\r\na\r\n$1\r\nb\r\n", ""},
	{[]string{"ZRANGE", "missing", "0", "-1", "WITHSCORES"}, "*0\r\n", ""},
	{[]string{"ZRANGEBYSCORE", "z", "-inf", "+inf", "WITHSCORES"}, "*4\r\n$1\r\na\r\n$1\r\n2\r\n$1\r\nb\r\n$3\r\n2.5\r\n",
		"*2\r\n*2\r\n$1\r\na\r\n,2\r\n*2\r\n$1\r\nb\r\n,2.5\r\n"},
	{[]string{"ZREVRANGEBYSCORE", "z", "+inf", "-inf", "WITHSCORES", "LIMIT", "0", "1"}, "*2\r\n$1\r\nb\r\n$3\r\n2.5\r\n",
		"*1\r\n*2\r\n$1\r\nb\r\n,2.5\r\n"},
	{[]string{"ZRANK", "z", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"ZRANK", "z", "b"}, ":1\r\n", ""},
	{[]string{"ZCOUNT", "z", "-inf", "+inf"}, ":2\r\n", ""},
	{[]string{"ZCARD", "z"}, ":2\r\n", ""},
	{[]string{"ZPOPMIN", "z"}, "*2\r\n$1\r\na\r\n$1\r\n2\r\n",
		"*2\r\n$1\r\na\r\n,2\r\n"},
	{[]string{"ZADD", "z", "1", "a", "inf", "c"}, ":2\r\n", ""},
	{[]string{"ZPOPMAX", "z", "1"}, "*2\r\n$1\r\nc\r\n$3\r\ninf\r\n",
		"*1\r\n*2\r\n$1\r\nc\r\n,inf\r\n"},
	{[]string{"ZPOPMIN", "z", "5"}, "*4\r\n$1\r\na\r\n$1\r\n1\r\n$1\r\nb\r\n$3\r\n2.5\r\n",
		"*2\r\n*2\r\n$1\r\na\r\n,1\r\n*2\r\n$1\r\nb\r\n,2.5\r\n"},
	{[]string{"ZPOPMIN", "missing"}, "*0\r\n", ""},
	{[]string{"ZPOPMIN", "missing", "2"}, "*0\r\n", ""},
	{[]string{"ZADD", "z", "-inf", "n", "1e300", "big"}, ":2\r\n", ""},
	{[]string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, "*4\r\n$1\r\nn\r\n$4\r\n-inf\r\n$3\r\nbig\r\n$6\r\n1e+300\r\n",
		"*2\r\n*2\r\n$1\r\nn\r\n,-inf\r\n*2\r\n$3\r\nbig\r\n,1e+300\r\n"},
	{[]string{"ZREM", "z", "n", "big"}, ":2\r\n", ""},
	{[]string{"GEOADD", "g", "13.361389", "38.115556", "Palermo", "15.087269", "37.502669", "Catania"}, ":2\r\n", ""},
	{[]string{"GEODIST", "g", "Palermo", "Catania"}, "$11\r\n166274.1516\r\n", ""},
	{[]string{"GEODIST", "g", "Palermo", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"GEOPOS", "g", "Palermo", "missing"}, "*2\r\n*2\r\n$18\r\n13.361389338970184\r\n$16\r\n38.1155563954963\r\n*-1\r\n",
		"*2\r\n*2\r\n,13.361389338970184\r\n,38.1155563954963\r\n_\r\n"},
	{[]string{"GEOPOS", "missing", "Palermo"}, "*1\r\n*-1\r\n",
		"*1\r\n_\r\n"},
	{[]string{"GEOHASH", "g", "Palermo", "missing"}, "*2\r\n$11\r\nsqc8b49rny0\r\n$-1\r\n",
		"*2\r\n$11\r\nsqc8b49rny0\r\n_\r\n"},
	{[]string{"GEOSEARCH", "g", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ASC"}, "*2\r\n$7\r\nCatania\r\n$7\r\nPalermo\r\n", ""},
	{[]string{"GEOSEARCH", "g", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ASC", "WITHCOORD", "WITHDIST", "WITHHASH"}, "*2\r\n*4\r\n$7\r\nCatania\r\n$7\r\n56.4413\r\n:3479447370796909\r\n*2\r\n$18\r\n15.087267458438873\r\n$17\r\n37.50266842333162\r\n*4\r\n$7\r\nPalermo\r\n$8\r\n190.4424\r\n:3479099956230698\r\n*2\r\n$18\r\n13.361389338970184\r\n$16\r\n38.1155563954963\r\n",
		"*2\r\n*4\r\n$7\r\nCatania\r\n$7\r\n56.4413\r\n:3479447370796909\r\n*2\r\n,15.087267458438873\r\n,37.50266842333162\r\n*4\r\n$7\r\nPalermo\r\n$8\r\n190.4424\r\n:3479099956230698\r\n*2\r\n,13.361389338970184\r\n,38.1155563954963\r\n"},
	{[]string{"GEOSEARCH", "g", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ASC", "WITHCOORD"}, "*2\r\n*2\r\n$7\r\nCatania\r\n*2\r\n$18\r\n15.087267458438873\r\n$17\r\n37.50266842333162\r\n*2\r\n$7\r\nPalermo\r\n*2\r\n$18\r\n13.361389338970184\r\n$16\r\n38.1155563954963\r\n",
		"*2\r\n*2\r\n$7\r\nCatania\r\n*2\r\n,15.087267458438873\r\n,37.50266842333162\r\n*2\r\n$7\r\nPalermo\r\n*2\r\n,13.361389338970184\r\n,38.1155563954963\r\n"},
	{[]string{"GEOSEARCH", "missing", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km"}, "*0\r\n", ""},
	{[]string{"PFADD", "p", "a", "b"}, ":1\r\n", ""},
	{[]string{"PFCOUNT", "p"}, ":2\r\n", ""},
	{[]string{"PFMERGE", "p2", "p"}, "+OK\r\n", ""},
	{[]string{"BF.RESERVE", "bf", "0.01", "100"}, "+OK\r\n", ""},
	{[]string{"BF.ADD", "bf", "a"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"BF.ADD", "bf", "a"}, ":0\r\n",
		"#f\r\n"},
	{[]string{"BF.MADD", "bf", "a", "b"}, "*2\r\n:0\r\n:1\r\n",
		"*2\r\n#f\r\n#t\r\n"},
	{[]string{"BF.EXISTS", "bf", "a"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"BF.EXISTS", "missing", "a"}, ":0\r\n",
		"#f\r\n"},
	{[]string{"BF.MEXISTS", "bf", "a", "z"}, "*2\r\n:1\r\n:0\r\n",
		"*2\r\n#t\r\n#f\r\n"},
	{[]string{"BF.INFO", "bf"}, "*10\r\n+Capacity\r\n:100\r\n+Size\r\n:{n}\r\n+Number of filters\r\n:1\r\n+Number of items inserted\r\n:2\r\n+Expansion rate\r\n:2\r\n",
		"%5\r\n+Capacity\r\n:100\r\n+Size\r\n:{n}\r\n+Number of filters\r\n:1\r\n+Number of items inserted\r\n:2\r\n+Expansion rate\r\n:2\r\n"},
	{[]string{"CF.RESERVE", "cf", "100"}, "+OK\r\n", ""},
	{[]string{"CF.ADD", "cf", "a"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"CF.ADDNX", "cf", "a"}, ":0\r\n",
		"#f\r\n"},
	{[]string{"CF.ADDNX", "cf", "b"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"CF.EXISTS", "cf", "a"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"CF.EXISTS", "missing", "a"}, ":0\r\n",
		"#f\r\n"},
	{[]string{"CF.MEXISTS", "cf", "a", "z"}, "*2\r\n:1\r\n:0\r\n",
		"*2\r\n#t\r\n#f\r\n"},
	{[]string{"CF.DEL", "cf", "a"}, ":1\r\n",
		"#t\r\n"},
	{[]string{"CF.DEL", "cf", "zz"}, ":0\r\n",
		"#f\r\n"},
	{[]string{"CF.COUNT", "cf", "b"}, ":1\r\n", ""},
	{[]string{"CF.INFO", "cf"}, "*16\r\n+Size\r\n:{n}\r\n+Number of buckets\r\n:32\r\n+Number of filters\r\n:1\r\n+Number of items inserted\r\n:1\r\n+Number of items deleted\r\n:1\r\n+Bucket size\r\n:4\r\n+Expansion rate\r\n:0\r\n+Max iterations\r\n:500\r\n",
		"%8\r\n+Size\r\n:{n}\r\n+Number of buckets\r\n:32\r\n+Number of filters\r\n:1\r\n+Number of items inserted\r\n:1\r\n+Number of items deleted\r\n:1\r\n+Bucket size\r\n:4\r\n+Expansion rate\r\n:0\r\n+Max iterations\r\n:500\r\n"},
	{[]string{"CMS.INITBYDIM", "cms", "10", "5"}, "+OK\r\n", ""},
	{[]string{"CMS.INCRBY", "cms", "a", "3", "b", "2"}, "*2\r\n:3\r\n:2\r\n", ""},
	{[]string{"CMS.QUERY", "cms", "a", "z"}, "*2\r\n:3\r\n:0\r\n", ""},
	{[]string{"MORRIS.INITBYDIM", "mo", "10", "5"}, "+OK\r\n", ""},
	{[]string{"MORRIS.INCRBY", "mo", "a", "3"}, "*1\r\n$1\r\n3\r\n", ""},
	{[]string{"MORRIS.QUERY", "mo", "a", "z"}, "*2\r\n$1\r\n3\r\n$1\r\n0\r\n", ""},
	{[]string{"MORRIS.INFO", "mo"}, "*14\r\n$5\r\nWidth\r\n$2\r\n10\r\n$5\r\nDepth\r\n$1\r\n5\r\n$4\r\nSize\r\n${n}\r\n{n}\r\n$25\r\nCount-Min equivalent size\r\n$3\r\n264\r\n$22\r\nCounter relative error\r\n$6\r\n0.2000\r\n$9\r\nMax count\r\n$10\r\n4168383430\r\n$11\r\nTotal count\r\n$1\r\n3\r\n",
		"%7\r\n$5\r\nWidth\r\n$2\r\n10\r\n$5\r\nDepth\r\n$1\r\n5\r\n$4\r\nSize\r\n${n}\r\n{n}\r\n$25\r\nCount-Min equivalent size\r\n$3\r\n264\r\n$22\r\nCounter relative error\r\n$6\r\n0.2000\r\n$9\r\nMax count\r\n$10\r\n4168383430\r\n$11\r\nTotal count\r\n$1\r\n3\r\n"},
	{[]string{"MEMORY", "USAGE", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"KEEL.DUMP", "missing"}, "$-1\r\n",
		"_\r\n"},
	{[]string{"DBSIZE"}, ":15\r\n", ""},
	{[]string{"SCAN", "0", "MATCH", "s", "COUNT", "1000"}, "*2\r\n${n}\r\n{n}\r\n*1\r\n$1\r\ns\r\n", ""},
	{[]string{"KEYS", "s"}, "*1\r\n$1\r\ns\r\n", ""},
	{[]string{"FLUSHDB"}, "+OK\r\n", ""},
	{[]string{"DBSIZE"}, ":0\r\n", ""},
}

// replyPattern turns an expected reply into a pattern in which {n} matches any
// number and everything else matches itself.
func replyPattern(want string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(want), `\{n\}`, `[0-9]+`) + "$")
}

func TestRESP2RepliesAreUnchanged(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	for _, step := range replyScript {
		got := string(rawReplyAs(t, false, step.args[0], step.args[1:]...))
		assert.Regexp(t, replyPattern(step.resp2), got, "%q", step.args)
	}
}

func TestRESP3ReplyShapes(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	for _, step := range replyScript {
		want := step.resp3
		if want == "" {
			want = step.resp2
		}
		got := string(rawReplyAs(t, true, step.args[0], step.args[1:]...))
		assert.Regexp(t, replyPattern(want), got, "%q", step.args)
	}
}

// TestRESP3INFOReportsTheConnectionsProtocol: INFO is a verbatim string to a
// RESP3 connection, as Redis sends it, and resp_version says which protocol
// the connection asking speaks - the same as HELLO's proto.
func TestRESP3INFOReportsTheConnectionsProtocol(t *testing.T) {
	ResetStores()
	resp2 := string(rawReplyAs(t, false, "INFO", "server"))
	assert.True(t, strings.HasPrefix(resp2, "$"), resp2)
	assert.Contains(t, resp2, "resp_version:2\r\n")

	resp3 := string(rawReplyAs(t, true, "INFO", "server"))
	require.True(t, strings.HasPrefix(resp3, "="), resp3)
	header, body, _ := strings.Cut(resp3, "\r\n")
	assert.Equal(t, "="+strconv.Itoa(len(body)-2), header, "the length counts the format and its colon")
	assert.True(t, strings.HasPrefix(body, "txt:# Server\r\n"), body)
	assert.Contains(t, body, "resp_version:3\r\n")
	assert.True(t, strings.HasSuffix(body, "\r\n\r\n"))
}

func TestRESP3MEMORYSTATSIsAMap(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v")
	resp2 := rawReplyAs(t, false, "MEMORY", "STATS")
	resp3 := rawReplyAs(t, true, "MEMORY", "STATS")
	pairs := 3
	data_structure.EachKeyspace(func(data_structure.Keyspace) { pairs++ })
	assert.True(t, bytes.HasPrefix(resp2, []byte("*"+strconv.Itoa(2*pairs)+"\r\n")), "%q", resp2)
	assert.True(t, bytes.HasPrefix(resp3, []byte("%"+strconv.Itoa(pairs)+"\r\n")), "%q", resp3)
	assert.Equal(t, resp2[bytes.IndexByte(resp2, '\n'):], resp3[bytes.IndexByte(resp3, '\n'):],
		"the same keys and values, under a map header")
}

// TestRESP3NeverReachesTheLogOrADump: what is logged, replicated and dumped is
// not a reply. A session run over RESP3 writes the same log, byte for byte, as
// the same session over RESP2, and the same KEEL.DUMP images.
func TestRESP3NeverReachesTheLogOrADump(t *testing.T) {
	session := func(resp3 bool) (string, []string) {
		var dumps []string
		path := withAOF(t, func() {
			for _, step := range replyScript {
				if step.args[0] == "FLUSHDB" {
					for _, key := range []string{"s", "h", "l", "z", "g", "p", "bf", "cf", "cms", "mo"} {
						dumps = append(dumps, string(rawReplyAs(t, resp3, "KEEL.DUMP", key)))
					}
				}
				rawReplyAs(t, resp3, step.args[0], step.args[1:]...)
			}
		})
		log, err := os.ReadFile(path)
		require.NoError(t, err)
		// SETEX is logged as the instant it expires, which the two sessions
		// reach a few milliseconds apart.
		instant := regexp.MustCompile(`PEXPIREAT\r\n\$1\r\ne\r\n\$13\r\n[0-9]{13}\r\n`)
		return instant.ReplaceAllString(string(log), "PEXPIREAT e <instant>"), dumps
	}
	log2, dumps2 := session(false)
	log3, dumps3 := session(true)
	assert.Contains(t, log2, "PEXPIREAT e <instant>")
	assert.Equal(t, log2, log3, "the log does not depend on the protocol a command arrived in")
	for i := range dumps2 {
		if strings.HasPrefix(dumps2[i], "$-1") {
			assert.Equal(t, "_\r\n", dumps3[i], "a missing key is RESP3's null")
			continue
		}
		assert.Equal(t, dumps2[i], dumps3[i], "a dump image is a bulk string in both protocols")
	}
}

// TestReplayAndReplicaApplyAnswerInRESP2: neither answers a client, and what
// each does with a reply - look for an error - must not depend on how the
// command first arrived.
func TestReplayAndReplicaApplyAnswerInRESP2(t *testing.T) {
	ResetStores()
	t.Cleanup(func() { replicaApplying, aof.replaying = false, false })
	for _, flag := range []*bool{&replicaApplying, &aof.replaying} {
		*flag = true
		assert.Equal(t, "$-1\r\n", string(rawReplyAs(t, true, "GET", "missing")))
		*flag = false
	}
	assert.Equal(t, "_\r\n", string(rawReplyAs(t, true, "GET", "missing")))
}

// TestRESP3IsScopedToOneCommand: the protocol belongs to the command that
// carried it, and nothing after it inherits it - including a command that
// fails, or one this server does not have.
func TestRESP3IsScopedToOneCommand(t *testing.T) {
	ResetStores()
	rawReplyAs(t, true, "GET", "missing")
	assert.Equal(t, "$-1\r\n", string(rawReplyAs(t, false, "GET", "missing")))
	rawReplyAs(t, true, "GET")
	assert.Equal(t, "$-1\r\n", string(rawReplyAs(t, false, "GET", "missing")))
	var w replyWriter
	assert.Error(t, EvalAndResponse(&Command{Cmd: "NOSUCH", RESP3: true}, &w))
	assert.False(t, replyRESP3)
	assert.Equal(t, "$-1\r\n", string(Encode(nil, false)), "Encode outside a command is RESP2")
	assert.Equal(t, "_\r\n", string(EncodeAs(nil, false, true)))
	assert.False(t, replyRESP3, "EncodeAs restores what it found")
}

// TestRESP3RepliesAreSizedExactly: the collection replies count their framing
// before allocating, and RESP3's framing is not RESP2's - a map header counts
// pairs, a double has no length line, a null is shorter, and nested pairs
// have headers of their own. An exact count fills the buffer to the byte, and
// the reservation is for that size.
func TestRESP3RepliesAreSizedExactly(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old })
	for i := 0; i < 12; i++ {
		v := strings.Repeat("v", i*13)
		run(t, "HSET", "h", "field"+strconv.Itoa(i), v)
		run(t, "RPUSH", "l", v)
		run(t, "SADD", "s", "member"+v)
		run(t, "ZADD", "z", []string{"1", "2.5", "-0.125", "inf", "1e300", "-7"}[i%6], "member"+strconv.Itoa(i))
		run(t, "GEOADD", "g", strconv.Itoa(i), strconv.Itoa(i), "place"+strconv.Itoa(i))
	}
	// build runs a handler directly in the given protocol, restoring the
	// protocol whatever happens, so a failure here cannot leave the rest of
	// the package's tests encoding RESP3.
	build := func(args []string, resp3 bool) []byte {
		saved := replyRESP3
		replyRESP3 = resp3
		defer func() { replyRESP3 = saved }()
		return commandTable[args[0]](args[1:])
	}
	for _, args := range [][]string{
		{"HGETALL", "h"}, {"HKEYS", "h"}, {"HMGET", "h", "field1", "missing", "field2"}, {"MGET", "missing", "missing"},
		{"LRANGE", "l", "0", "-1"}, {"LPOP", "l", "2"}, {"SMEMBERS", "s"}, {"SPOP", "s", "2"}, {"SRANDMEMBER", "s", "3"},
		{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, {"ZRANGEBYSCORE", "z", "-inf", "+inf", "WITHSCORES"},
		{"ZPOPMIN", "z"}, {"ZPOPMAX", "z", "3"},
		{"GEOSEARCH", "g", "FROMLONLAT", "0", "0", "BYRADIUS", "5000", "km", "ASC", "WITHCOORD", "WITHDIST", "WITHHASH"},
		{"GEOSEARCH", "g", "FROMLONLAT", "0", "0", "BYRADIUS", "5000", "km", "WITHCOORD"},
		{"SCAN", "0", "COUNT", "100"},
	} {
		for _, resp3 := range []bool{false, true} {
			CommandAllocations = &CommandAllocationBudget{Limit: 64 << 20}
			out := build(args, resp3)
			require.NotEqual(t, byte('-'), out[0], "%v: %q", args, out)
			assert.Equal(t, len(out), cap(out), "%v resp3=%v is sized to the byte", args, resp3)
			assert.Equal(t, 3*((len(out)+4095)&^4095), CommandAllocations.ReplyReserved,
				"%v resp3=%v reserves what it sends", args, resp3)
		}
	}
	CommandAllocations = old
}

// TestRESP3OutputLimitCountsRESP3Framing: the 64 MiB output limit is applied
// to the reply as the connection's protocol frames it. RESP2 sends a member
// and its score as two bulk strings; RESP3 nests them as an array of a bulk
// string and a double, a byte longer for a three-character score. Members
// sized so the RESP2 reply is ten bytes under the limit put the RESP3 one 55
// over, and each is answered by its own size.
func TestRESP3OutputLimitCountsRESP3Framing(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	const pairs = 65
	target := MaxReplyBytes - 10
	header, score := len("*130\r\n"), len("$3\r\n0.5\r\n")
	bulk := (target-header)/pairs - score
	length := bulk - len("$1048576\r\n\r\n")
	rest := target - header - pairs*(bulk+score)
	z := data_structure.CreateZSet()
	for i := 0; i < pairs; i++ {
		n := length
		if i == pairs-1 {
			n += rest
		}
		z.Add(0.5, strconv.Itoa(100+i)+strings.Repeat("x", n-3), 0)
	}
	zsetStore.Put("z", z)

	saved := replyRESP3
	defer func() { replyRESP3 = saved }()
	replyRESP3 = false
	resp2 := cmdZRANGE([]string{"z", "0", "-1", "WITHSCORES"})
	require.Equal(t, byte('*'), resp2[0])
	assert.Equal(t, target, len(resp2))
	resp2 = nil

	replyRESP3 = true
	assert.Equal(t, replyTooLarge, cmdZRANGE([]string{"z", "0", "-1", "WITHSCORES"}))
	assert.Equal(t, replyTooLarge, cmdZPOPMIN([]string{"z", "65"}))
	assert.Equal(t, pairs, z.Len(), "a refused pop removes nothing")
}
