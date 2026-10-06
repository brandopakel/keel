package core

import (
	"os"
	"strings"
	"testing"
)

func TestExpiringSetAliasesPreserveCanonicalPersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		if runOn(t, e, "SETEX", "seconds", "600", "value") != "OK" || runOn(t, e, "PSETEX", "milliseconds", "600000", "value") != "OK" {
			t.Fatal("expiring SET did not succeed")
		}
		for _, key := range []string{"seconds", "milliseconds"} {
			if ttl := runOn(t, e, "PTTL", key).(int64); ttl <= 590000 || ttl > 600000 {
				t.Fatalf("incorrect TTL for %s: %d", key, ttl)
			}
		}
	})
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "SETEX") || !strings.Contains(string(data), "PEXPIREAT") {
		t.Fatal("aliases must persist as backward-readable SET and absolute expiry")
	}
	restartOn(t, e, path)
	defer e.CloseAOF()
	for _, key := range []string{"seconds", "milliseconds"} {
		if runOn(t, e, "GET", key) != "value" || runOn(t, e, "PTTL", key).(int64) <= 0 {
			t.Fatal("value or expiry lost after replay")
		}
	}
}

func TestExpiringSetAliasesValidateBeforeMutation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, command := range []string{"SETEX", "PSETEX"} {
		e.resetStores()
		runOn(t, e, "SET", "key", "original")
		for _, ttl := range []string{"0", "-1", "invalid", "9223372036854775807"} {
			if result := string(rawReplyOn(t, e, command, "key", ttl, "replacement")); !strings.HasPrefix(result, "-ERR") {
				t.Fatalf("%s accepted invalid TTL %q", command, ttl)
			}
			if runOn(t, e, "GET", "key") != "original" {
				t.Fatal("failed command changed value")
			}
		}
		runOn(t, e, "HSET", "hash", "field", "value")
		if !strings.HasPrefix(string(rawReplyOn(t, e, command, "hash", "0", "replacement")), "-ERR") {
			t.Fatalf("%s accepted an invalid TTL over a hash", command)
		}
		if runOn(t, e, "TYPE", "hash") != "hash" {
			t.Fatal("a refused command replaced the hash before validating")
		}
		if string(rawReplyOn(t, e, command, "hash", "100", "replacement")) != "+OK\r\n" || runOn(t, e, "GET", "hash") != "replacement" {
			t.Fatalf("%s did not replace a hash, as SET does", command)
		}
	}
}
