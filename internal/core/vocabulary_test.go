package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commandInfo runs COMMAND INFO name and returns its one entry.
func commandInfo(t *testing.T, e *Engine, name string) []interface{} {
	t.Helper()
	reply, ok := runOn(t, e, "COMMAND", "INFO", name).([]interface{})
	require.True(t, ok)
	require.Len(t, reply, 1)
	entry, ok := reply[0].([]interface{})
	require.True(t, ok, "%s: %v", name, reply[0])
	return entry
}

// TestCommandInfoIsRedissDerivation: the flags, first and last key and step,
// and categories COMMAND INFO reports are those Redis derives from each
// command's spec.
func TestCommandInfoIsRedissDerivation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, tc := range []struct {
		name              string
		arity             int64
		flags             []interface{}
		first, last, step int64
		categories        []interface{}
	}{
		{"get", 2, []interface{}{"readonly", "fast"}, 1, 1, 1, []interface{}{"@read", "@string", "@fast"}},
		{"mset", -3, []interface{}{"write", "denyoom"}, 1, -1, 2, []interface{}{"@write", "@string", "@slow"}},
		{"lcs", -3, []interface{}{"readonly"}, 1, 2, 1, []interface{}{"@read", "@string", "@slow"}},
		{"set", -3, []interface{}{"write", "denyoom"}, 1, 1, 1, []interface{}{"@write", "@string", "@slow"}},
		{"ping", -1, []interface{}{"fast"}, 0, 0, 0, []interface{}{"@fast", "@connection"}},
		{"config|get", -3, []interface{}{"admin", "noscript", "loading", "stale"}, 0, 0, 0,
			[]interface{}{"@admin", "@slow", "@dangerous"}},
	} {
		info := commandInfo(t, e, tc.name)
		require.Len(t, info, 10, tc.name)
		assert.Equal(t, tc.name, info[0])
		assert.Equal(t, tc.arity, info[1], tc.name)
		assert.Equal(t, tc.flags, info[2], tc.name)
		assert.Equal(t, []interface{}{tc.first, tc.last, tc.step}, info[3:6], tc.name)
		assert.Equal(t, tc.categories, info[6], tc.name)
	}
	assert.Equal(t, "*2\r\n$-1\r\n$-1\r\n", string(rawReplyOn(t, e, "COMMAND", "INFO", "watch", "config|nosuch")))
	assert.Equal(t, "*1\r\n_\r\n", string(rawReplyAsOn(t, e, true, "COMMAND", "INFO", "nosuch")))
}

// TestLegacyKeysAsRedisReadsSeveralSpecs: consecutive plain ranges join, and
// anything else leaves the keys' positions to the arguments (movablekeys).
func TestLegacyKeysAsRedisReadsSeveralSpecs(t *testing.T) {
	t.Parallel()
	rng := func(pos, last, step int) keySpec {
		return keySpec{begin: keyBegin{kind: "index", pos: pos}, find: keyFind{kind: "range", lastKey: last, step: step}}
	}
	keyword := keySpec{begin: keyBegin{kind: "keyword", keyword: "STORE", startFrom: 6}, find: keyFind{kind: "range", step: 1}}
	for _, tc := range []struct {
		keys              []keySpec
		first, last, step int
		movable           bool
	}{
		{nil, 0, 0, 0, false},
		{[]keySpec{rng(1, 0, 1), rng(2, 0, 1)}, 1, 2, 1, false},
		{[]keySpec{rng(1, 0, 1), rng(2, -1, 1)}, 1, -1, 1, false},
		{[]keySpec{rng(1, 0, 1), rng(3, 0, 1)}, 1, 1, 1, true},
		{[]keySpec{rng(1, 0, 1), keyword}, 1, 1, 1, true},
		{[]keySpec{keyword}, 0, 0, 0, true},
		{[]keySpec{{flags: []string{"INCOMPLETE"}, begin: keyBegin{kind: "index", pos: 1}, find: keyFind{kind: "range", step: 1}}}, 1, 1, 1, true},
	} {
		first, last, step, movable := commandSpec{keys: tc.keys}.legacyKeys()
		assert.Equal(t, []int{tc.first, tc.last, tc.step}, []int{first, last, step}, "%+v", tc.keys)
		assert.Equal(t, tc.movable, movable, "%+v", tc.keys)
	}
}

// TestCommandGetKeysAsRedisFindsThem: the key specs where they say all there
// is, SET's and PFMERGE's own functions where they do not, and Redis's errors.
func TestCommandGetKeysAsRedisFindsThem(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for args, want := range map[string]interface{}{
		"GETKEYS set k v":             []interface{}{"k"},
		"GETKEYSANDFLAGS set k v":     []interface{}{[]interface{}{"k", []interface{}{"OW", "update"}}},
		"GETKEYSANDFLAGS set k v GET": []interface{}{[]interface{}{"k", []interface{}{"RW", "access", "update"}}},
		"GETKEYS mset a 1 b 2":        []interface{}{"a", "b"},
		"GETKEYSANDFLAGS lcs a b":     []interface{}{[]interface{}{"a", []interface{}{"RO", "access"}}, []interface{}{"b", []interface{}{"RO", "access"}}},
		"GETKEYS pfmerge d s1 s2":     []interface{}{"d", "s1", "s2"},
		"GETKEYS memory usage k":      []interface{}{"k"},
		"GETKEYS del a b c":           []interface{}{"a", "b", "c"},
		"GETKEYS mset a 1 b":          []interface{}{"a", "b"},
		"GETKEYS mset a":              "ERR Invalid number of arguments specified for command",
		"GETKEYS ping":                "ERR The command has no key arguments",
		"GETKEYS config get x":        "ERR The command has no key arguments",
		"GETKEYS config":              "ERR The command has no key arguments",
		"GETKEYS nosuch x":            "ERR Invalid command specified",
		"GETKEYS memory|usage k":      "ERR Invalid command specified",
		"GETKEYS memory nosuch k":     "ERR Invalid command specified",
		"GETKEYS get":                 "ERR Invalid number of arguments specified for command",
		"GETKEYS get a b":             "ERR Invalid number of arguments specified for command",
		"GETKEYS scan 0":              "ERR The command has no key arguments",
	} {
		assert.Equal(t, want, runOn(t, e, "COMMAND", strings.Fields(args)...), args)
	}
}

// TestEveryKeyFunctionIsKeels: each function Redis finds a command's keys
// with that a command Keel has names has its counterpart here.
func TestEveryKeyFunctionIsKeels(t *testing.T) {
	t.Parallel()
	for _, f := range redisCommandFacts {
		if f.getKeys != "" {
			assert.NotNil(t, getKeysProc(f.getKeys), "%s finds its keys with %s, which Keel lacks", f.name, f.getKeys)
		}
	}
}

// TestCommandListCountAndDocs: LIST names every command and subcommand Keel
// has, or those a filter keeps; COUNT counts the commands; DOCS reports the
// facts and none of Redis's text.
func TestCommandListCountAndDocs(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	all := runOn(t, e, "COMMAND", "LIST").([]interface{})
	assert.Contains(t, all, "get")
	assert.Contains(t, all, "config|get")
	assert.Contains(t, all, "command|getkeys")
	assert.Equal(t, int64(len(commandArity)), runOn(t, e, "COMMAND", "COUNT"))
	assert.Len(t, runOn(t, e, "COMMAND").([]interface{}), len(commandArity))
	assert.Equal(t, []interface{}{"config|get"}, runOn(t, e, "COMMAND", "LIST", "FILTERBY", "PATTERN", "config|g*"))
	assert.Equal(t, []interface{}{}, runOn(t, e, "COMMAND", "LIST", "FILTERBY", "ACLCAT", "nosuch"))
	hll := runOn(t, e, "COMMAND", "LIST", "FILTERBY", "aclcat", "HYPERLOGLOG").([]interface{})
	assert.ElementsMatch(t, []interface{}{"pfadd", "pfcount", "pfmerge"}, hll)
	bf := runOn(t, e, "COMMAND", "LIST", "FILTERBY", "MODULE", "bf").([]interface{})
	assert.Contains(t, bf, "bf.add")
	assert.Contains(t, bf, "cms.query")
	assert.Equal(t, "ERR syntax error", runOn(t, e, "COMMAND", "LIST", "FILTERBY", "x", "y"))
	assert.Equal(t, "ERR syntax error", runOn(t, e, "COMMAND", "LIST", "FILTERBY", "PATTERN"))

	assert.Equal(t, []interface{}{"get", []interface{}{"since", "1.0.0", "group", "string", "arguments", []interface{}{
		[]interface{}{"name", "key", "type", "key", "display_text", "key", "key_spec_index", int64(0)}}}},
		runOn(t, e, "COMMAND", "DOCS", "get", "nosuch"))
	docs := runOn(t, e, "COMMAND", "DOCS", "bf.add").([]interface{})
	assert.Equal(t, []interface{}{"bf.add", []interface{}{"group", "module", "module", "bf"}}, docs)
}
