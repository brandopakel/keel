package core

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func resetCFStore() { ResetStores() }

func TestCmdCFReserveAndAdd(t *testing.T) {
	resetCFStore()

	res, err := Decode(defaultEngine.cmdCFRESERVE([]string{"cf", "1000"}))
	assert.Nil(t, err)
	assert.EqualValues(t, "OK", res)

	res, _ = Decode(defaultEngine.cmdCFRESERVE([]string{"cf", "1000"}))
	assert.Equal(t, "ERR item exists", res)

	res, _ = Decode(defaultEngine.cmdCFADD([]string{"cf", "a"}))
	assert.EqualValues(t, 1, res)
	res, _ = Decode(defaultEngine.cmdCFEXISTS([]string{"cf", "a"}))
	assert.EqualValues(t, 1, res)
	res, _ = Decode(defaultEngine.cmdCFEXISTS([]string{"cf", "b"}))
	assert.EqualValues(t, 0, res)
}

// TestCmdCFAddCreatesMissingKey matches RedisBloom: adding to a key that does
// not exist creates a default-sized filter rather than erroring.
func TestCmdCFAddCreatesMissingKey(t *testing.T) {
	resetCFStore()
	res, _ := Decode(defaultEngine.cmdCFADD([]string{"fresh", "x"}))
	assert.EqualValues(t, 1, res)
	assert.True(t, defaultEngine.cfStore.Exists("fresh"))
}

// TestCmdCFAddAllowsDuplicates is the difference from CF.ADDNX, and what makes
// CF.COUNT meaningful.
func TestCmdCFAddAllowsDuplicates(t *testing.T) {
	resetCFStore()
	for i := 0; i < 3; i++ {
		res, _ := Decode(defaultEngine.cmdCFADD([]string{"cf", "dup"}))
		assert.EqualValues(t, 1, res)
	}
	res, _ := Decode(defaultEngine.cmdCFCOUNT([]string{"cf", "dup"}))
	assert.EqualValues(t, 3, res)
}

func TestCmdCFAddNX(t *testing.T) {
	resetCFStore()
	res, _ := Decode(defaultEngine.cmdCFADDNX([]string{"cf", "x"}))
	assert.EqualValues(t, 1, res, "first add should succeed")

	res, _ = Decode(defaultEngine.cmdCFADDNX([]string{"cf", "x"}))
	assert.EqualValues(t, 0, res, "second add should be refused")

	res, _ = Decode(defaultEngine.cmdCFCOUNT([]string{"cf", "x"}))
	assert.EqualValues(t, 1, res, "ADDNX must not have stored a second copy")
}

func TestCmdCFDel(t *testing.T) {
	resetCFStore()
	defaultEngine.cmdCFADD([]string{"cf", "gone"})

	res, _ := Decode(defaultEngine.cmdCFDEL([]string{"cf", "gone"}))
	assert.EqualValues(t, 1, res)

	res, _ = Decode(defaultEngine.cmdCFEXISTS([]string{"cf", "gone"}))
	assert.EqualValues(t, 0, res)

	res, _ = Decode(defaultEngine.cmdCFDEL([]string{"cf", "gone"}))
	assert.EqualValues(t, 0, res, "deleting what is not there reports nothing removed")

	res, _ = Decode(defaultEngine.cmdCFDEL([]string{"nosuchkey", "x"}))
	assert.Equal(t, "Not found", res, "RedisBloom's answer for a key with no filter")
}

func TestCmdCFMExists(t *testing.T) {
	resetCFStore()
	defaultEngine.cmdCFADD([]string{"cf", "a"})
	defaultEngine.cmdCFADD([]string{"cf", "c"})

	assert.Equal(t, "*3\r\n:1\r\n:0\r\n:1\r\n", string(defaultEngine.cmdCFMEXISTS([]string{"cf", "a", "b", "c"})),
		"integers in RESP2, as RedisBloom sends them")
	assert.Equal(t, "*2\r\n:0\r\n:0\r\n", string(defaultEngine.cmdCFMEXISTS([]string{"nosuchkey", "a", "b"})))
}

func TestCmdCFCountAndExistsOnMissingKey(t *testing.T) {
	resetCFStore()
	res, _ := Decode(defaultEngine.cmdCFCOUNT([]string{"nope", "x"}))
	assert.EqualValues(t, 0, res)
	res, _ = Decode(defaultEngine.cmdCFEXISTS([]string{"nope", "x"}))
	assert.EqualValues(t, 0, res)
}

func TestCmdCFInfo(t *testing.T) {
	resetCFStore()
	defaultEngine.cmdCFRESERVE([]string{"cf", "1000"})
	defaultEngine.cmdCFADD([]string{"cf", "a"})
	defaultEngine.cmdCFADD([]string{"cf", "b"})
	defaultEngine.cmdCFDEL([]string{"cf", "a"})

	// RedisBloom's fields, in its order, as simple strings and integers in
	// both protocols. "Number of items inserted" is what the filter holds now.
	reply := string(defaultEngine.cmdCFINFO([]string{"cf"}))
	assert.Regexp(t, `^\*16\r\n\+Size\r\n:[0-9]+\r\n\+Number of buckets\r\n:512\r\n\+Number of filters\r\n:1\r\n`+
		`\+Number of items inserted\r\n:1\r\n\+Number of items deleted\r\n:1\r\n\+Bucket size\r\n:4\r\n`+
		`\+Expansion rate\r\n:0\r\n\+Max iterations\r\n:500\r\n$`, reply)

	res, _ := Decode(defaultEngine.cmdCFINFO([]string{"missing"}))
	assert.Equal(t, "ERR not found", res)
}

// TestCmdCFFullFilterReportsAnError covers what a cuckoo filter does that a
// Bloom filter cannot: refuse. A Bloom filter accepts every insert and quietly
// grows less accurate; this one has a hard capacity and says so.
func TestCmdCFFullFilterReportsAnError(t *testing.T) {
	resetCFStore()
	defaultEngine.cmdCFRESERVE([]string{"small", "8"})

	var lastErr interface{}
	for i := 0; i < 100000; i++ {
		res, _ := Decode(defaultEngine.cmdCFADD([]string{"small", "item:" + strconv.Itoa(i)}))
		if s, ok := res.(string); ok {
			lastErr = s
			break
		}
	}
	assert.Equal(t, "Filter is full", lastErr)
}

func TestCmdCFWrongArity(t *testing.T) {
	resetCFStore()
	for _, c := range []struct {
		name string
		fn   func([]string) []byte
		args []string
	}{
		{"CF.RESERVE", defaultEngine.cmdCFRESERVE, []string{"k"}},
		{"CF.ADD", defaultEngine.cmdCFADD, []string{"k"}},
		{"CF.ADDNX", defaultEngine.cmdCFADDNX, []string{"k"}},
		{"CF.EXISTS", defaultEngine.cmdCFEXISTS, []string{"k"}},
		{"CF.MEXISTS", defaultEngine.cmdCFMEXISTS, []string{"k"}},
		{"CF.DEL", defaultEngine.cmdCFDEL, []string{"k"}},
		{"CF.COUNT", defaultEngine.cmdCFCOUNT, []string{"k"}},
		{"CF.INFO", defaultEngine.cmdCFINFO, []string{}},
	} {
		res, _ := Decode(c.fn(c.args))
		assert.Contains(t, res, "wrong number of arguments", "%s should reject bad arity", c.name)
	}
}

// TestCFRESERVEOptions: RedisBloom's options, read and refused as RedisBloom
// reads and refuses them. This server's filters have one geometry, so a
// value other than it is refused, after every check RedisBloom makes.
func TestCFRESERVEOptions(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"abc"}, "-Bad capacity"},
		{[]string{"007"}, "-Bad capacity"},
		{[]string{"3"}, "-Capacity must be in the range [2 * BUCKETSIZE, 1073741824]"},
		{[]string{"1073741825"}, "-Capacity must be in the range [2 * BUCKETSIZE, 1073741824]"},
		{[]string{"7", "BUCKETSIZE", "4"}, "-Capacity must be in the range [2 * BUCKETSIZE, 1073741824]"},
		{[]string{"100", "BUCKETSIZE", "x"}, "-Couldn't parse BUCKETSIZE"},
		{[]string{"100", "BUCKETSIZE", "0"}, "-BUCKETSIZE: value must be in the range [1, 255]"},
		{[]string{"100", "MAXITERATIONS", "0"}, "-MAXITERATIONS: value must be in the range [1, 65535]"},
		{[]string{"100", "EXPANSION", "-1"}, "-EXPANSION: value must be in the range [0, 32768]"},
		{[]string{"100", "BUCKETSIZE", "0", "MAXITERATIONS", "0"}, "-MAXITERATIONS: value must be in the range [1, 65535]"},
		{[]string{"100", "BUCKETSIZE", "2"}, "-ERR this server's cuckoo filters have BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 only"},
		{[]string{"100", "EXPANSION", "1"}, "-ERR this server's cuckoo filters have BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 only"},
	} {
		assert.Equal(t, c.want+"\r\n", string(rawReply(t, "CF.RESERVE", append([]string{"cf"}, c.args...)...)), "%q", c.args)
	}
	assert.EqualValues(t, 0, run(t, "EXISTS", "cf"))
	assert.Equal(t, "OK", run(t, "CF.RESERVE", "cf", "8", "BUCKETSIZE", "4", "MAXITERATIONS", "500", "EXPANSION", "0"))
	assert.Equal(t, "OK", run(t, "CF.RESERVE", "partial", "100", "expansion", "0", "FOO", "BAR"), "an unknown argument is ignored")
	assert.Equal(t, "ERR item exists", run(t, "CF.RESERVE", "cf", "100"))
	assert.Equal(t, "Bad capacity", run(t, "CF.RESERVE", "str", "abc"), "parameters come before the key")
	assert.Contains(t, run(t, "CF.RESERVE", "str", "100"), "WRONGTYPE")
	assert.Equal(t, "BUCKETSIZE: value must be in the range [1, 255]", run(t, "CF.RESERVE", "bucketsize", "1000"),
		"the options are looked for among every argument, the key included")
	assert.Contains(t, run(t, "CF.RESERVE", "cf", "100", "BUCKETSIZE"), "wrong number of arguments")
}

// TestCFReadsAnswerNoForAnotherType: RedisBloom's CF.EXISTS, CF.MEXISTS and
// CF.COUNT answer no, and 0, for a key that is not a cuckoo filter, and CF.DEL
// answers "Not found"; its writes and CF.INFO answer WRONGTYPE.
func TestCFReadsAnswerNoForAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	run(t, "BF.ADD", "bf", "a")
	for _, key := range []string{"str", "bf"} {
		assert.EqualValues(t, 0, run(t, "CF.EXISTS", key, "a"), key)
		assert.Equal(t, "*2\r\n:0\r\n:0\r\n", string(rawReply(t, "CF.MEXISTS", key, "a", "b")), key)
		assert.EqualValues(t, 0, run(t, "CF.COUNT", key, "a"), key)
		assert.Equal(t, "-Not found\r\n", string(rawReply(t, "CF.DEL", key, "a")), key)
		for _, args := range [][]string{{"CF.ADD", key, "a"}, {"CF.ADDNX", key, "a"}, {"CF.INFO", key}, {"CF.RESERVE", key, "100"}} {
			assert.Equal(t, "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n", string(rawReply(t, args[0], args[1:]...)), "%q", args)
		}
	}
}

// TestCFDELOfAMissingKeyIsNotLoggedOrReplicated: RedisBloom refuses it, so it
// changes nothing and is neither logged nor sent to a replica - where the
// build before answered 0 and logged it. Its log still replays: see
// TestBloomCuckooLegacyLogReplays.
func TestCFDELOfAMissingKeyIsNotLoggedOrReplicated(t *testing.T) {
	setupReplicationV2(t)
	frames := snapshotV2(t)
	offset, epoch := frames[len(frames)-1].To, frames[0].Epoch
	before, err := os.ReadFile(defaultEngine.aof.path)
	assert.NoError(t, err)
	assert.Equal(t, "-Not found\r\n", string(rawReply(t, "CF.DEL", "missing", "x")))
	assert.NoError(t, FlushAOF())
	after, err := os.ReadFile(defaultEngine.aof.path)
	assert.NoError(t, err)
	assert.Equal(t, before, after, "nothing logged")
	assert.Empty(t, pullV2(t, epoch, offset, "", 0).Body, "nothing replicated")
}
