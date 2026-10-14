package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The command specs, held to Redis's source and to this package's own
// tables. command_specs.go is generated from Redis 8.10.1's src/commands by
// scripts/generate-command-specs.py; TestRedisCommandSpecsMatchRedisSource
// reads those files itself, when KEEL_REDIS_SRC names the unpacked tree,
// and compares, so that the generator and the file cannot drift from the
// specs. TestCommandSpecsAgreeWithTheTables reads the specs against
// commandArity, writeCommands and the key rules on every run, so that the
// tables the specs will replace (plan step 4.2) and the specs agree.

// specNames is every name the tables declare: commandArity's, and each
// container's subcommands as container|sub.
func specNames() []string {
	var names []string
	for name := range commandArity {
		names = append(names, name)
		for _, sub := range subcommandsOf(name) {
			names = append(names, name+"|"+strings.ToUpper(sub.name))
		}
	}
	slices.Sort(names)
	return names
}

// redisSpec is a command's spec as src/commands/<name>.json holds it, the
// fields the table keeps.
type redisSpec struct {
	Container string   `json:"container"`
	Arity     int      `json:"arity"`
	Group     string   `json:"group"`
	Flags     []string `json:"command_flags"`
	ACL       []string `json:"acl_categories"`
	KeySpecs  []struct {
		Flags       []string `json:"flags"`
		BeginSearch struct {
			Index   *struct{ Pos int } `json:"index"`
			Keyword *struct {
				Keyword   string
				StartFrom int `json:"startfrom"`
			} `json:"keyword"`
		} `json:"begin_search"`
		FindKeys struct {
			Range *struct {
				LastKey     int `json:"lastkey"`
				Step, Limit int
			} `json:"range"`
			Keynum *struct {
				KeynumIdx int `json:"keynumidx"`
				FirstKey  int `json:"firstkey"`
				Step      int
			} `json:"keynum"`
		} `json:"find_keys"`
	} `json:"key_specs"`
}

// specFromRedis is the table's form of a spec read from Redis's source.
func specFromRedis(s redisSpec) commandSpec {
	out := commandSpec{arity: s.Arity, group: s.Group, flags: s.Flags, acl: s.ACL}
	for _, ks := range s.KeySpecs {
		k := keySpec{flags: ks.Flags, begin: keyBegin{kind: "unknown"}, find: keyFind{kind: "unknown"}}
		switch {
		case ks.BeginSearch.Index != nil:
			k.begin = keyBegin{kind: "index", pos: ks.BeginSearch.Index.Pos}
		case ks.BeginSearch.Keyword != nil:
			k.begin = keyBegin{kind: "keyword", keyword: ks.BeginSearch.Keyword.Keyword, startFrom: ks.BeginSearch.Keyword.StartFrom}
		}
		switch {
		case ks.FindKeys.Range != nil:
			k.find = keyFind{kind: "range", lastKey: ks.FindKeys.Range.LastKey, step: ks.FindKeys.Range.Step, limit: ks.FindKeys.Range.Limit}
		case ks.FindKeys.Keynum != nil:
			k.find = keyFind{kind: "keynum", keynumIdx: ks.FindKeys.Keynum.KeynumIdx, firstKey: ks.FindKeys.Keynum.FirstKey, step: ks.FindKeys.Keynum.Step}
		}
		if k.flags == nil {
			k.flags = []string{}
		}
		out.keys = append(out.keys, k)
	}
	return out
}

// TestRedisCommandSpecsMatchRedisSource: the generated table is what Redis
// 8.10.1's src/commands declares for every command Keel has, and holds
// every command Keel has that Redis declares. It runs where KEEL_REDIS_SRC
// names the unpacked Redis source tree, which CI's differential job has.
func TestRedisCommandSpecsMatchRedisSource(t *testing.T) {
	t.Parallel()
	src := os.Getenv("KEEL_REDIS_SRC")
	if src == "" {
		t.Skip("set KEEL_REDIS_SRC to the unpacked redis-8.10.1 tree")
	}
	commands := filepath.Join(src, "src", "commands")
	seen := map[string]bool{}
	for _, name := range specNames() {
		container, sub, isSub := strings.Cut(name, "|")
		file := strings.ToLower(container) + ".json"
		if isSub {
			file = strings.ToLower(container) + "-" + strings.ToLower(sub) + ".json"
		}
		body, err := os.ReadFile(filepath.Join(commands, file))
		if os.IsNotExist(err) {
			_, generated := redisCommandSpecs[name]
			require.False(t, generated, "%s has no spec in Redis's source and is in the generated table", name)
			continue
		}
		require.NoError(t, err)
		var entries map[string]redisSpec
		require.NoError(t, json.Unmarshal(body, &entries))
		key := container
		if isSub {
			key = sub
		}
		entry, ok := entries[key]
		require.True(t, ok, "%s: no entry %s", file, key)
		if isSub {
			require.Equal(t, container, entry.Container, file)
		}
		want := specFromRedis(entry)
		got, ok := redisCommandSpecs[name]
		require.True(t, ok, "%s is declared by Redis and missing from the generated table: run scripts/generate-command-specs.py", name)
		require.Equal(t, want, normalizedSpec(got), "%s: the generated table differs from %s", name, file)
		seen[name] = true
	}
	for name := range redisCommandSpecs {
		require.True(t, seen[name], "%s is in the generated table and not in the tables", name)
	}
}

// normalizedSpec is s with nil slices as empty ones where the JSON form has
// them, so that the two compare.
func normalizedSpec(s commandSpec) commandSpec {
	for i := range s.keys {
		if s.keys[i].flags == nil {
			s.keys[i].flags = []string{}
		}
	}
	return s
}

// keyRuleExceptions are the commands whose key spec says every argument is a
// key, where the type check reads only the first, with the reason: each is
// handled on its own and constrains no key to a type, as keytype.go says.
var keyRuleExceptions = map[string]string{
	"DEL":    "removes keys whatever type holds them; writtenKeys names every argument itself",
	"UNLINK": "DEL by another name",
	"EXISTS": "answers about a name whatever type holds it",
	"MGET":   "answers nil for a key of another type, Redis's rule",
}

// keyRuleOfSpecs is the key rule keys amount to: the first argument, the
// first two, every argument, or every step-th from the first; false for key
// specs no rule describes. Keys found from the first argument by range are
// read directly, and PFMERGE's two specs - the destination, then every
// source - together name every argument.
func keyRuleOfSpecs(keys []keySpec) (rule keyRule, stride int, ok bool) {
	for _, k := range keys {
		if k.begin.kind != "index" || k.find.kind != "range" {
			return 0, 0, false
		}
	}
	first := keys[0]
	if first.begin.pos != 1 {
		return 0, 0, false
	}
	if len(keys) == 2 {
		second := keys[1]
		if first.find.lastKey == 0 && second.begin.pos == 2 && second.find.lastKey == -1 && second.find.step == 1 {
			return keyEvery, 0, true
		}
		return 0, 0, false
	}
	if len(keys) != 1 {
		return 0, 0, false
	}
	switch {
	case first.find.lastKey == 0:
		return keyFirst, 0, true
	case first.find.lastKey == 1 && first.find.step == 1:
		return keyFirstTwo, 0, true
	case first.find.lastKey == -1 && first.find.step == 1:
		return keyEvery, 0, true
	case first.find.lastKey == -1:
		return keyStride, first.find.step, true
	}
	return 0, 0, false
}

// TestCommandSpecsAgreeWithTheTables: every command the tables declare has
// one spec, with the tables' arity; WRITE is writeCommands; and the key
// specs are the key rules.
func TestCommandSpecsAgreeWithTheTables(t *testing.T) {
	t.Parallel()
	names := specNames()
	for _, name := range names {
		_, redis := redisCommandSpecs[name]
		_, keel := keelCommandSpecs[name]
		require.True(t, redis != keel, "%s: declared by Redis %v and by hand %v; one table declares a command", name, redis, keel)
		spec, _ := commandSpecOf(name)
		container, sub, isSub := strings.Cut(name, "|")
		if isSub {
			for _, s := range subcommandsOf(container) {
				if strings.EqualFold(s.name, sub) {
					require.Equal(t, s.arity, spec.arity, "%s: arity", name)
				}
			}
			continue
		}
		require.Equal(t, commandArity[name], spec.arity, "%s: arity", name)
		require.Equal(t, writeCommands[name], spec.hasFlag("WRITE"), "%s: WRITE is the log's writeCommands", name)
		if len(spec.keys) == 0 || subcommandsOf(name) != nil {
			continue
		}
		if _, excepted := keyRuleExceptions[name]; excepted {
			continue
		}
		want, stride, ok := keyRuleOfSpecs(spec.keys)
		require.True(t, ok, "%s: the key specs %+v name keys in a form the type check has no rule for", name, spec.keys)
		if want == keyStride {
			require.Equal(t, stride, strideKeyCommands[name], "%s: the stride", name)
		}
		require.Equal(t, want, keyRuleOf(name), "%s: the key rule the type check applies is the specs'", name)
	}
	for name := range redisCommandSpecs {
		require.Contains(t, names, name, "%s is in the generated table and not in the tables", name)
	}
	for name := range keelCommandSpecs {
		require.Contains(t, names, name, "%s is declared by hand and not in the tables", name)
	}
	for name := range keyRuleExceptions {
		spec, ok := commandSpecOf(name)
		require.True(t, ok, name)
		require.Equal(t, -1, spec.keys[0].find.lastKey, "%s is excepted and its spec does not name every argument", name)
	}
}

// TestRedisCommandSpecTableIsInNameOrder: the generated table holds each name
// once, in order, so that indexing it by name loses none.
func TestRedisCommandSpecTableIsInNameOrder(t *testing.T) {
	t.Parallel()
	for i := 1; i < len(redisCommandSpecTable); i++ {
		require.Less(t, redisCommandSpecTable[i-1].name, redisCommandSpecTable[i].name)
	}
	require.Len(t, redisCommandSpecs, len(redisCommandSpecTable))
	for _, entry := range redisCommandSpecTable {
		require.Equal(t, entry.spec, redisCommandSpecs[entry.name], entry.name)
	}
}
