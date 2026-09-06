package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCounterRejectsNoncanonicalIntegersWithoutMutation(t *testing.T) {
	for _, value := range []string{"", "007", "+1", "-0", " 1", "1 ", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			ResetStores()
			run(t, "HSET", "h", "field", value)
			if result := string(rawReply(t, "HINCRBY", "h", "field", "2")); !strings.HasPrefix(result, "-ERR") {
				t.Fatalf("invalid hash field incremented: %q", result)
			}
			if run(t, "HGET", "h", "field") != value {
				t.Fatal("refused increment changed the hash field")
			}
			for _, command := range []string{"INCRBY", "DECRBY", "HINCRBY"} {
				args := []string{"absent", value}
				if command == "HINCRBY" {
					args = []string{"absent", "field", value}
				}
				if result := string(rawReply(t, command, args...)); !strings.HasPrefix(result, "-ERR") {
					t.Fatalf("invalid increment accepted by %s", command)
				}
				if run(t, "EXISTS", "absent") != int64(0) {
					t.Fatal("refused increment created a key")
				}
			}
		})
	}
}

func TestCounterLegacyAOFAcceptsHistoricalIntegerSpellings(t *testing.T) {
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
		restart(t, path)
		if run(t, "HGET", "h", "field") != "2" || run(t, "GET", "string") != "6" {
			t.Fatal("historical accepted counter operations changed on replay")
		}
		if err := CloseAOF(); err != nil {
			t.Fatal(err)
		}
	}
}
