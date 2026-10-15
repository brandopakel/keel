package core

import (
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/data_structure"
)

// lrmState is what the LRM checks read of a key: whether it is there, its
// value and its TTL in a form that is the same for the same value, and its
// eviction score, its last modification.
type lrmState struct {
	present bool
	value   string
	score   uint64
}

func lrmStateOf(e *Engine, key string) lrmState {
	// The store that holds it, found as eviction finds a key's score:
	// OwnerOf would remove a key whose TTL has passed.
	var owner data_structure.Keyspace
	e.space.EachKeyspace(func(ks data_structure.Keyspace) {
		if _, ok := ks.ScoreOf(key); ok && owner == nil {
			owner = ks
		}
	})
	if owner == nil {
		return lrmState{}
	}
	at, hasTTL := owner.GetExpiry(key)
	if hasTTL && at <= uint64(time.Now().Add(time.Second).UnixMilli()) {
		// Its TTL has passed or soon will, and DUMP would remove it before
		// the case's own command or the expiry cycle could.
		return lrmState{}
	}
	score, ok := owner.ScoreOf(key)
	if !ok {
		return lrmState{}
	}
	// A hash's and a set's DUMP follow the order the value holds them in,
	// which a read may change; their members, sorted, do not.
	var parts []string
	if h, ok := e.hashStore.Peek(key); ok {
		fields, values := h.Entries()
		for i := range fields {
			parts = append(parts, fields[i]+"\x00"+values[i])
		}
		sort.Strings(parts)
	} else if s, ok := e.setStore.Peek(key); ok {
		parts = append(parts, s.Members()...)
		sort.Strings(parts)
	} else if dump, ok := e.dumpKey(key); ok {
		parts = append(parts, string(dump))
	}
	if hasTTL {
		parts = append(parts, "ttl "+strconv.FormatUint(at, 10))
	}
	return lrmState{present: true, value: strings.Join(parts, "\x01"), score: score}
}

// TestEveryFamilyCountsItsWritesUnderLRM: under allkeys-lrm a command that
// changed a key's value moves the key's score on, as Redis's keyModified
// stamps it, whatever family wrote it, and a read moves none, as Redis's
// lookupKey leaves the stamp alone. Every case of the command corpus runs
// under it, so a write that mutates its value in place and says nothing of
// it fails here, today's commands and those still to come.
func TestEveryFamilyCountsItsWritesUnderLRM(t *testing.T) {
	t.Parallel()
	for _, cs := range commandGoldenCorpus {
		cs := cs
		t.Run(cs.fullName(), func(t *testing.T) {
			t.Parallel()
			chosen := cs.options
			cs.options = func(o *Options) {
				if chosen != nil {
					chosen(o)
				}
				// A case that chose a policy of its own keeps it.
				if o.Eviction == EvictLRU {
					o.Eviction = EvictLRM
				}
			}
			r := startCommandGoldenRun(t, cs, false)
			lrm := r.e.space.EvictionPolicy() == EvictLRM
			for _, s := range cs.steps {
				if s.directive != "" || s.do != nil || r.tx != nil || !lrm {
					r.step(s)
					continue
				}
				cmd := parsedCommand(s.args[0], s.args[1:])
				if IsTransactionCommand(cmd.Cmd) {
					r.step(s)
					continue
				}
				write := writeCommands[cmd.Cmd]
				keys := commandKeys(cmd)
				if write {
					keys = writtenKeys(cmd)
				}
				before := make([]lrmState, len(keys))
				for i, key := range keys {
					before[i] = lrmStateOf(r.e, key)
				}
				r.step(s)
				for i, key := range keys {
					after := lrmStateOf(r.e, key)
					if !before[i].present || !after.present {
						continue
					}
					what := strings.Join(s.args, " ") + ": " + key
					if write && before[i].value != after.value {
						assert.Greater(t, after.score, before[i].score, "%s changed and was not stamped", what)
					}
					if !write {
						assert.Equal(t, before[i].score, after.score, "%s, a read, stamped it", what)
					}
				}
			}
		})
	}
}

// TestLRMEvictsWhatWasOnlyReadFirst is Redis's "LRM: Keys with only read
// operations should be removed first" (tests/unit/maxmemory.tcl): of keys
// written together, those only read since are evicted before any written
// again, however recently they were read.
func TestLRMEvictsWhatWasOnlyReadFirst(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxKeys: 50, Eviction: EvictLRM, EvictionSamples: 64})
	for j := 0; j < 25; j++ {
		runOn(t, e, "SET", "read:"+strconv.Itoa(j), "x")
		runOn(t, e, "SET", "write:"+strconv.Itoa(j), "x")
	}
	for j := 0; j < 25; j++ {
		runOn(t, e, "GET", "read:"+strconv.Itoa(j))
		runOn(t, e, "SET", "write:"+strconv.Itoa(j), "y")
	}
	for j := 0; j < 10; j++ {
		runOn(t, e, "SET", "trigger:"+strconv.Itoa(j), "z")
	}
	readSurvived, writeSurvived := 0, 0
	for j := 0; j < 25; j++ {
		readSurvived += int(runOn(t, e, "EXISTS", "read:"+strconv.Itoa(j)).(int64))
		writeSurvived += int(runOn(t, e, "EXISTS", "write:"+strconv.Itoa(j)).(int64))
	}
	require.Equal(t, 15, readSurvived, "ten of the keys only read made room")
	assert.Equal(t, 25, writeSurvived, "no key written again was evicted")
}
