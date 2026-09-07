package core

import (
	"os"
	"strings"
	"testing"
)

func TestExpiringSetAliasesPreserveCanonicalPersistence(t *testing.T) {
	path := withAOF(t, func() {
		if run(t, "SETEX", "seconds", "600", "value") != "OK" || run(t, "PSETEX", "milliseconds", "600000", "value") != "OK" {
			t.Fatal("expiring SET did not succeed")
		}
		for _, key := range []string{"seconds", "milliseconds"} {
			if ttl := run(t, "PTTL", key).(int64); ttl <= 590000 || ttl > 600000 {
				t.Fatalf("incorrect TTL for %s: %d", key, ttl)
			}
		}
	})
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "SETEX") || !strings.Contains(string(data), "PEXPIREAT") {
		t.Fatal("aliases must persist as backward-readable SET and absolute expiry")
	}
	restart(t, path)
	defer CloseAOF()
	for _, key := range []string{"seconds", "milliseconds"} {
		if run(t, "GET", key) != "value" || run(t, "PTTL", key).(int64) <= 0 {
			t.Fatal("value or expiry lost after replay")
		}
	}
}

func TestExpiringSetAliasesValidateBeforeMutation(t *testing.T) {
	for _, command := range []string{"SETEX", "PSETEX"} {
		ResetStores()
		run(t, "SET", "key", "original")
		for _, ttl := range []string{"0", "-1", "invalid", "9223372036854775807"} {
			if result := string(rawReply(t, command, "key", ttl, "replacement")); !strings.HasPrefix(result, "-ERR") {
				t.Fatalf("%s accepted invalid TTL %q", command, ttl)
			}
			if run(t, "GET", "key") != "original" {
				t.Fatal("failed command changed value")
			}
		}
		run(t, "HSET", "hash", "field", "value")
		if !strings.HasPrefix(string(rawReply(t, command, "hash", "100", "replacement")), "-WRONGTYPE") {
			t.Fatal("alias did not preserve SET's documented type boundary")
		}
	}
}
