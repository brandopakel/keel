package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCounterRejectsNoncanonicalIntegersWithoutMutation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, value := range []string{"", "007", "+1", "-0", " 1", "1 ", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			e.resetStores()
			runOn(t, e, "HSET", "h", "field", value)
			if result := string(rawReplyOn(t, e, "HINCRBY", "h", "field", "2")); !strings.HasPrefix(result, "-ERR") {
				t.Fatalf("invalid hash field incremented: %q", result)
			}
			if runOn(t, e, "HGET", "h", "field") != value {
				t.Fatal("refused increment changed the hash field")
			}
			for _, command := range []string{"INCRBY", "DECRBY", "HINCRBY"} {
				args := []string{"absent", value}
				if command == "HINCRBY" {
					args = []string{"absent", "field", value}
				}
				if result := string(rawReplyOn(t, e, command, args...)); !strings.HasPrefix(result, "-ERR") {
					t.Fatalf("invalid increment accepted by %s", command)
				}
				if runOn(t, e, "EXISTS", "absent") != int64(0) {
					t.Fatal("refused increment created a key")
				}
			}
		})
	}
}

func TestCounterLegacyAOFAcceptsHistoricalIntegerSpellings(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "legacy.aof")
	var history []byte
	for _, command := range [][]string{
		{"HSET", "h", "field", "-0"}, {"HINCRBY", "h", "field", "+2"},
		{"INCRBY", "string", "007"}, {"DECRBY", "string", "+1"},
	} {
		history = appendCommand(history, command...)
	}
	if err := os.WriteFile(path, history, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		if runOn(t, e, "HGET", "h", "field") != "2" || runOn(t, e, "GET", "string") != "6" {
			t.Fatal("historical accepted counter operations changed on replay")
		}
		if err := e.CloseAOF(); err != nil {
			t.Fatal(err)
		}
	}
}
