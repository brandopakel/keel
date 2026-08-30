package core

import (
	"errors"
	"fmt"
	"strings"

	"memkv/internal/config"
	"memkv/internal/constant"
	"memkv/internal/data_structure"
)

// cmdMEMORY implements the MEMORY subcommands.
//
// Only USAGE is supported. It reports what the accounting believes one key
// costs, which is an estimate - see entryBytes - so it is useful for comparing
// keys against each other and for understanding why eviction chose what it did.
func cmdMEMORY(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MEMORY' command"), false)
	}
	switch strings.ToUpper(args[0]) {
	case "USAGE":
		if len(args) != 2 {
			return Encode(errors.New("(error) ERR wrong number of arguments for 'MEMORY USAGE' command"), false)
		}
		bytes, exists := entryBytesAnywhere(args[1])
		if !exists {
			return constant.RespNil
		}
		return Encode(int64(bytes), false)
	default:
		return Encode(errors.New(fmt.Sprintf("ERR unknown MEMORY subcommand '%s'", args[0])), false)
	}
}

// entryBytesAnywhere finds a key in whichever keyspace holds it.
//
// Keys live in a separate map per type here, so a name can be a string in one
// and a sorted set in another; the first match wins, which is the same order
// the command tables resolve in. Looking only in the string dictionary - as
// this did at first - reported nil for every set, sketch and filter.
func entryBytesAnywhere(key string) (uint64, bool) {
	if n, ok := dictStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := zsetStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := setStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := sbStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := cmsStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := morrisStore.EntryBytes(key); ok {
		return n, true
	}
	if n, ok := hllStore.EntryBytes(key); ok {
		return n, true
	}
	return cfStore.EntryBytes(key)
}

// humanBytes renders a byte count the way redis-cli's INFO output does.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

func evictionPolicyName() string {
	switch config.EvictStrategy {
	case config.LRU:
		return "allkeys-lru"
	case config.LFU:
		return "allkeys-lfu"
	default:
		return "allkeys-random"
	}
}

// cmdINFO reports server state, in the section format redis-cli expects.
//
// It exists because eviction is otherwise invisible: without used_memory and
// evicted_keys there is no way to tell a cache that is working from one that is
// thrashing.
func cmdINFO(args []string) []byte {
	if len(args) > 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'INFO' command"), false)
	}
	section := ""
	if len(args) == 1 {
		section = strings.ToLower(args[0])
	}

	var b strings.Builder
	want := func(name string) bool { return section == "" || section == name }

	if want("memory") {
		used := data_structure.TotalMemUsed()
		fmt.Fprintf(&b, "# Memory\r\nused_memory:%d\r\nused_memory_human:%s\r\n",
			used, humanBytes(used))
		fmt.Fprintf(&b, "maxmemory:%d\r\nmaxmemory_human:%s\r\nmaxmemory_policy:%s\r\n\r\n",
			config.MaxMemory, humanBytes(config.MaxMemory), evictionPolicyName())
	}
	if want("stats") {
		fmt.Fprintf(&b, "# Stats\r\nevicted_keys:%d\r\n\r\n", data_structure.Evicted())
	}
	if want("persistence") {
		base, current, rewrites, keys := AOFStats()
		enabled := 0
		if AOFEnabled() {
			enabled = 1
		}
		fmt.Fprintf(&b, "# Persistence\r\naof_enabled:%d\r\naof_base_size:%d\r\naof_current_size:%d\r\n",
			enabled, base, current)
		fmt.Fprintf(&b, "aof_rewrites:%d\r\naof_keys_at_last_rewrite:%d\r\n\r\n", rewrites, keys)
	}
	if want("keyspace") {
		fmt.Fprintf(&b, "# Keyspace\r\ndb0:keys=%d\r\n\r\n", data_structure.TotalKeys())
	}

	return Encode(b.String(), false)
}

// cmdBGREWRITEAOF rewrites the append-only file.
//
// The name is Redis's, and so is what it does, but not how: Redis forks a child
// and keeps serving, and this cannot - a Go runtime does not survive a bare
// fork. So the "BG" is currently a promise the implementation does not keep,
// and the reply says so rather than leaving a caller to discover it from a
// latency graph. Keeping the name is still right: it is what every client,
// script and runbook already calls, and a server that answered only to
// REWRITEAOF would be harder to operate for no gain in honesty that this reply
// does not already provide.
func cmdBGREWRITEAOF(args []string) []byte {
	if len(args) != 0 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'BGREWRITEAOF' command"), false)
	}
	if err := RewriteAOF(); err != nil {
		return Encode(fmt.Errorf("ERR %w", err), false)
	}
	return Encode("Background append only file rewriting started (synchronously: this server does not fork)", true)
}
