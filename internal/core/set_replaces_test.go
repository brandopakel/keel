package core

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// SET replaces a key whatever type held it, as Redis's does. These are the
// rules an application reusing a name across types relies on, and the log
// records that let a build from before this change replay what this one wrote.

func TestSETReplacesEveryOtherType(t *testing.T) {
	for _, holder := range []struct {
		name string
		make func()
	}{
		{"set", func() { run(t, "SADD", "k", "m") }},
		{"hash", func() { run(t, "HSET", "k", "f", "v") }},
		{"list", func() { run(t, "RPUSH", "k", "a", "b") }},
		{"zset", func() { run(t, "ZADD", "k", "1", "m") }},
		{"geo", func() { run(t, "GEOADD", "k", "13.361389", "38.115556", "palermo") }},
		{"bloom", func() { run(t, "BF.MADD", "k", "m") }},
		{"cms", func() { run(t, "CMS.INITBYDIM", "k", "100", "5") }},
		{"morris", func() { run(t, "MORRIS.INITBYDIM", "k", "100", "5") }},
		{"hll", func() { run(t, "PFADD", "k", "m") }},
		{"cuckoo", func() { run(t, "CF.ADD", "k", "m") }},
	} {
		for _, write := range [][]string{
			{"SET", "k", "v"}, {"SET", "k", "v", "XX"}, {"SETEX", "k", "100", "v"},
			{"PSETEX", "k", "100000", "v"}, {"MSET", "k", "v"},
		} {
			ResetStores()
			holder.make()
			assert.Equal(t, "OK", run(t, write[0], write[1:]...), "%v over a %s", write, holder.name)
			assert.Equal(t, "string", run(t, "TYPE", "k"), "%v over a %s", write, holder.name)
			assert.Equal(t, "v", run(t, "GET", "k"))
			assert.Equal(t, 1, data_structure.TotalKeys(), "the %s is gone, not shadowed", holder.name)
			data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
				if ks.KeyspaceName() != "string" {
					assert.Zero(t, ks.MemUsed(), "%s keyspace still charged after %v", ks.KeyspaceName(), write)
				}
			})
		}
	}
}

func TestSETConditionsSeeAKeyOfAnyType(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "f", "v")
	assert.Equal(t, constant.RespNil, rawReply(t, "SET", "h", "s", "NX"), "the hash exists, so NX leaves it")
	assert.EqualValues(t, 0, run(t, "SETNX", "h", "s"))
	assert.Equal(t, "hash", run(t, "TYPE", "h"))

	assert.Contains(t, run(t, "SET", "h", "s", "GET"), "WRONGTYPE",
		"GET cannot return a hash as the old value, so nothing is written")
	assert.Equal(t, "hash", run(t, "TYPE", "h"))

	assert.Equal(t, "OK", run(t, "SET", "h", "s", "XX"), "and XX finds it there")
	assert.Equal(t, "s", run(t, "GET", "h"))
}

func TestSETOverAnotherTypeHandlesItsExpiry(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "f", "v")
	run(t, "EXPIRE", "h", "100")
	assert.Equal(t, "OK", run(t, "SET", "h", "s", "KEEPTTL"))
	assert.EqualValues(t, 100, run(t, "TTL", "h"), "KEEPTTL keeps the key's deadline, whatever held it")

	run(t, "DEL", "h")
	run(t, "HSET", "h", "f", "v")
	run(t, "EXPIRE", "h", "100")
	assert.Equal(t, "OK", run(t, "SET", "h", "s"))
	assert.EqualValues(t, -1, run(t, "TTL", "h"), "a plain SET drops it, as it does over a string")

	// An expired key of another type holds nothing, so NX writes.
	run(t, "DEL", "h")
	run(t, "SADD", "gone", "m")
	run(t, "PEXPIRE", "gone", "1")
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, "OK", run(t, "SET", "gone", "s", "NX"))
}

// The DEL ahead of the SET is what lets the build before this one replay the
// log: there SET over a hash answers WRONGTYPE and a failed replay command
// stops startup.
func TestSETOverAnotherTypeIsLoggedAsDELThenSET(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "HSET", "h", "f", "v")
		run(t, "SET", "h", "s")
		run(t, "SADD", "a", "m")
		run(t, "ZADD", "b", "1", "m")
		run(t, "MSET", "a", "1", "b", "2", "c", "3")
		run(t, "SET", "c", "refused", "NX")
	})
	data, err := os.ReadFile(path)
	assert.NoError(t, err)
	log := string(data)
	del, set := strings.Index(log, "$3\r\nDEL\r\n$1\r\nh\r\n"), strings.Index(log, "$3\r\nSET\r\n$1\r\nh\r\n$1\r\ns\r\n")
	assert.True(t, del >= 0 && set > del, "DEL h must precede SET h:\n%q", log)
	delA, delB, mset := strings.Index(log, "$3\r\nDEL\r\n$1\r\na\r\n"), strings.Index(log, "$3\r\nDEL\r\n$1\r\nb\r\n"), strings.Index(log, "$4\r\nMSET\r\n")
	assert.True(t, delA >= 0 && delB > delA && mset > delB, "DELs for the replaced keys precede the MSET:\n%q", log)
	assert.NotContains(t, log, "refused")

	restart(t, path)
	assert.Equal(t, "s", run(t, "GET", "h"))
	assert.Equal(t, "1", run(t, "GET", "a"))
	assert.Equal(t, "2", run(t, "GET", "b"))
	assert.Equal(t, "3", run(t, "GET", "c"))
	assert.Equal(t, 4, data_structure.TotalKeys())
}
