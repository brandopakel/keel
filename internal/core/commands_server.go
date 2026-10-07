package core

import (
	"fmt"
	"strings"

	"github.com/brandopakel/keel/internal/data_structure"
)

// cmdMEMORY implements the MEMORY subcommands.
//
// USAGE reports one key and STATS reports estimated memory by type. It reports what the accounting believes one key
// costs, which is an estimate - see entryBytes - so it is useful for comparing
// keys against each other and for understanding why eviction chose what it did.
//
// The subcommand and its count have been checked against the command table by
// the time this runs; see containerCommands.
func (e *Engine) cmdMEMORY(args []string) []byte {
	if err := CommandError(&Command{Cmd: "MEMORY", Args: args}); err != nil {
		return e.encode(err, false)
	}
	switch strings.ToUpper(args[0]) {
	case "STATS":
		out := ReplyMap{"keyspace.bytes", int64(e.space.TotalMemUsed()), "keys.count", int64(e.space.TotalKeys()), "keys.expires", int64(e.KeysWithExpiry())}
		e.space.EachKeyspace(func(ks data_structure.Keyspace) { out = append(out, ks.KeyspaceName()+".bytes", int64(ks.MemUsed())) })
		return e.encode(out, false)
	case "USAGE":
		// SAMPLES is Redis's, and read as Redis reads it; the estimate here
		// is not sampled, so its count changes nothing.
		for i := 2; i < len(args); i += 2 {
			if !strings.EqualFold(args[i], "SAMPLES") || i+1 == len(args) {
				return e.encode(errSyntax, false)
			}
			samples, valid := e.counterInteger(args[i+1])
			if !valid {
				return e.encode(errNotAnInteger, false)
			}
			if samples < 0 {
				return e.encode(errSyntax, false)
			}
		}
		bytes, exists := e.entryBytesAnywhere(args[1])
		if !exists {
			return e.nullReply()
		}
		return e.encode(int64(bytes), false)
	}
	return memoryHelp
}

// memoryHelp is Redis's MEMORY HELP, for the subcommands this server has.
var memoryHelp = HelpReply("MEMORY",
	"STATS",
	"    Return information about the memory usage of the server.",
	"USAGE <key> [SAMPLES <count>]",
	"    Return memory in bytes used by <key> and its value, as this server",
	"    estimates it. SAMPLES is accepted, and the estimate is not sampled.")

// entryBytesAnywhere finds a key in whichever keyspace holds it.
//
// Keys live in a separate map per type here, so a name can be a string in one
// and a sorted set in another; the first match wins, which is the same order
// the command tables resolve in. Looking only in the string dictionary - as
// this did at first - reported nil for every set, sketch and filter.
func (e *Engine) entryBytesAnywhere(key string) (uint64, bool) {
	if owner, ok := e.space.OwnerOf(key); ok {
		return owner.EntryBytes(key)
	}
	return 0, false
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

// evictionPolicyName is Redis's name for an eviction policy, as INFO reports
// it.
func evictionPolicyName(policy EvictionPolicy) string {
	switch policy {
	case EvictLRU:
		return "allkeys-lru"
	case EvictLFU:
		return "allkeys-lfu"
	default:
		return "allkeys-random"
	}
}

// ClientBufferStats describes retained event-loop buffers, not process RSS or
// kernel socket buffers. The event loop owns both collection and observation.
type ClientBufferStats struct {
	Connected, InputBytes, ReplyBytes, TotalBytes int
	RequestAllocationPeak                         int64
	RequestAllocationRefusals                     uint64
	// Connections the event loop closed for holding work past its timeout:
	// slow readers and half-sent requests; requests the server parsed and
	// never answered; and requests it never read at all, the socket still
	// holding their bytes. RunsUnreplied counts connections closed because a
	// run executed commands and produced no reply. All but the first should
	// stay at zero.
	ClosedSlow, ClosedUnanswered, ClosedUnread uint64
	RunsUnreplied                              uint64
	// ConnectionsReceived counts every connection accepted since startup, and
	// ConnectionsRejected every one closed at once because the server already
	// held its most clients (Redis's rejected_connections).
	ConnectionsReceived, ConnectionsRejected uint64
}

// SetClientBuffers installs on e the hook INFO reads the connections of the
// transport driving e through, or removes it when f is nil. The event loop
// installs it before accepting clients and removes it after they close;
// core-only and alternate transports install none.
func (e *Engine) SetClientBuffers(f func() ClientBufferStats) { e.clientBuffers = f }

// RedisCompatibleVersion is the Redis release whose command forms this server's
// subset follows - EXPIRE NX/XX/GT/LT, SET GET/EXAT/PXAT, LCS - and the version
// clients are told in INFO and HELLO. Libraries gate features on it: Rails'
// cache store reads redis_version before choosing EXPIRE NX, and fails without
// one. It is not a claim to implement all of Redis 7.0; the README's integration
// contract is that.
const RedisCompatibleVersion = "7.0.0"

// cmdBGREWRITEAOF starts rewriting the append-only file and returns at once.
//
// It only starts it. The walk then advances a slice per event-loop cycle, so
// the reply arrives long before the work is finished and the server keeps
// answering throughout - which is what the "BG" in the name has always meant to
// every client and runbook that calls it.
//
// Redis gets that by forking; a Go runtime does not survive a bare fork, so
// here it comes from slicing the walk instead. INFO persistence reports
// aof_rewrites, which is how a caller waits for one to finish, and
// aof_last_bgrewrite_status, which says whether the last one did. A rewrite
// that fails leaves the log as it was and the server serving; see
// aof_rewrite_status.go, which also has the replies.
func (e *Engine) cmdBGREWRITEAOF(args []string) []byte {
	if len(args) != 0 {
		return e.encode(wrongArguments("BGREWRITEAOF"), false)
	}
	return e.bgRewriteAOF()
}
