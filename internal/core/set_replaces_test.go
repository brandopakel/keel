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
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, holder := range []struct {
		name string
		make func()
	}{
		{"set", func() { runOn(t, e, "SADD", "k", "m") }},
		{"hash", func() { runOn(t, e, "HSET", "k", "f", "v") }},
		{"list", func() { runOn(t, e, "RPUSH", "k", "a", "b") }},
		{"zset", func() { runOn(t, e, "ZADD", "k", "1", "m") }},
		{"geo", func() { runOn(t, e, "GEOADD", "k", "13.361389", "38.115556", "palermo") }},
		{"bloom", func() { runOn(t, e, "BF.MADD", "k", "m") }},
		{"cms", func() { runOn(t, e, "CMS.INITBYDIM", "k", "100", "5") }},
		{"morris", func() { runOn(t, e, "MORRIS.INITBYDIM", "k", "100", "5") }},
		{"hll", func() { runOn(t, e, "PFADD", "k", "m") }},
		{"cuckoo", func() { runOn(t, e, "CF.ADD", "k", "m") }},
	} {
		for _, write := range [][]string{
			{"SET", "k", "v"}, {"SET", "k", "v", "XX"}, {"SETEX", "k", "100", "v"},
			{"PSETEX", "k", "100000", "v"}, {"MSET", "k", "v"},
		} {
			e.resetStores()
			holder.make()
			assert.Equal(t, "OK", runOn(t, e, write[0], write[1:]...), "%v over a %s", write, holder.name)
			assert.Equal(t, "string", runOn(t, e, "TYPE", "k"), "%v over a %s", write, holder.name)
			assert.Equal(t, "v", runOn(t, e, "GET", "k"))
			assert.Equal(t, 1, e.space.TotalKeys(), "the %s is gone, not shadowed", holder.name)
			e.space.EachKeyspace(func(ks data_structure.Keyspace) {
				if ks.KeyspaceName() != "string" {
					assert.Zero(t, ks.MemUsed(), "%s keyspace still charged after %v", ks.KeyspaceName(), write)
				}
			})
		}
	}
}

func TestSETConditionsSeeAKeyOfAnyType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "f", "v")
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "SET", "h", "s", "NX"), "the hash exists, so NX leaves it")
	assert.EqualValues(t, 0, runOn(t, e, "SETNX", "h", "s"))
	assert.Equal(t, "hash", runOn(t, e, "TYPE", "h"))

	assert.Contains(t, runOn(t, e, "SET", "h", "s", "GET"), "WRONGTYPE",
		"GET cannot return a hash as the old value, so nothing is written")
	assert.Equal(t, "hash", runOn(t, e, "TYPE", "h"))

	assert.Equal(t, "OK", runOn(t, e, "SET", "h", "s", "XX"), "and XX finds it there")
	assert.Equal(t, "s", runOn(t, e, "GET", "h"))
}

func TestSETOverAnotherTypeHandlesItsExpiry(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "f", "v")
	runOn(t, e, "EXPIRE", "h", "100")
	assert.Equal(t, "OK", runOn(t, e, "SET", "h", "s", "KEEPTTL"))
	assert.EqualValues(t, 100, runOn(t, e, "TTL", "h"), "KEEPTTL keeps the key's deadline, whatever held it")

	runOn(t, e, "DEL", "h")
	runOn(t, e, "HSET", "h", "f", "v")
	runOn(t, e, "EXPIRE", "h", "100")
	assert.Equal(t, "OK", runOn(t, e, "SET", "h", "s"))
	assert.EqualValues(t, -1, runOn(t, e, "TTL", "h"), "a plain SET drops it, as it does over a string")

	// An expired key of another type holds nothing, so NX writes.
	runOn(t, e, "DEL", "h")
	runOn(t, e, "SADD", "gone", "m")
	runOn(t, e, "PEXPIRE", "gone", "1")
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, "OK", runOn(t, e, "SET", "gone", "s", "NX"))
}

// The DEL ahead of the SET is what lets the build before this one replay the
// log: there SET over a hash answers WRONGTYPE and a failed replay command
// stops startup.
func TestSETOverAnotherTypeIsLoggedAsDELThenSET(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "HSET", "h", "f", "v")
		runOn(t, e, "SET", "h", "s")
		runOn(t, e, "SADD", "a", "m")
		runOn(t, e, "ZADD", "b", "1", "m")
		runOn(t, e, "MSET", "a", "1", "b", "2", "c", "3")
		runOn(t, e, "SET", "c", "refused", "NX")
	})
	data, err := os.ReadFile(path)
	assert.NoError(t, err)
	log := string(data)
	del, set := strings.Index(log, "$3\r\nDEL\r\n$1\r\nh\r\n"), strings.Index(log, "$3\r\nSET\r\n$1\r\nh\r\n$1\r\ns\r\n")
	assert.True(t, del >= 0 && set > del, "DEL h must precede SET h:\n%q", log)
	delA, delB, mset := strings.Index(log, "$3\r\nDEL\r\n$1\r\na\r\n"), strings.Index(log, "$3\r\nDEL\r\n$1\r\nb\r\n"), strings.Index(log, "$4\r\nMSET\r\n")
	assert.True(t, delA >= 0 && delB > delA && mset > delB, "DELs for the replaced keys precede the MSET:\n%q", log)
	assert.NotContains(t, log, "refused")

	restartOn(t, e, path)
	assert.Equal(t, "s", runOn(t, e, "GET", "h"))
	assert.Equal(t, "1", runOn(t, e, "GET", "a"))
	assert.Equal(t, "2", runOn(t, e, "GET", "b"))
	assert.Equal(t, "3", runOn(t, e, "GET", "c"))
	assert.Equal(t, 4, e.space.TotalKeys())
}
