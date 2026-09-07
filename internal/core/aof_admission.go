package core

import "github.com/brandopakel/keel/internal/data_structure"
import "github.com/brandopakel/keel/internal/config"

// AppendAdmission bounds the transcript, replies and keyspace growth BEFORE a
// parsed run executes alongside an append. Unmodelled commands use the drained
// barrier path. These conservative bounds cover errors, lazy expiry, SET's
// canonical expiry records and reads of values written earlier in this run.
// No store is touched: Peek and TotalMemUsed do not reap or update access state.
func AppendAdmission(commands []*Command) (logBytes, replyBytes int, ok bool) {
	growth, newKeys, largestWrite := uint64(0), 0, 0
	for _, cmd := range commands {
		a := cmd.Args
		switch cmd.Cmd {
		case "SET", "SETEX", "PSETEX":
			if len(a) < 2 {
				return 0, 0, false
			}
			for _, s := range a {
				largestWrite = max(largestWrite, len(s))
			}
			growth += uint64(len(a[0]) + largestWrite + 256)
			newKeys++
		case "MSET":
			if len(a)%2 != 0 {
				return 0, 0, false
			}
			for i := 0; i < len(a); i += 2 {
				largestWrite = max(largestWrite, len(a[i+1]))
				growth += uint64(len(a[i]) + len(a[i+1]) + 256)
				newKeys++
			}
		case "INCR", "DECR", "INCRBY", "DECRBY":
			if len(a) == 0 {
				return 0, 0, false
			}
			growth += uint64(len(a[0]) + 256)
			largestWrite = max(largestWrite, 20)
			newKeys++
		case "GET", "MGET", "PING", "EXISTS", "TYPE", "TTL", "PTTL":
		default:
			return 0, 0, false
		}
		// Each argument can appear in the input record and in canonical SET /
		// PEXPIREAT or lazy DEL records. The fixed allowance covers framing.
		logBytes += 512
		for _, s := range a {
			logBytes += 3 * (len(s) + 32)
		}
		if logBytes > maxAsyncAppendBytes {
			return 0, 0, false
		}
	}
	if data_structure.TotalKeys()+newKeys > config.KeyNumberLimit ||
		(config.MaxMemory > 0 && data_structure.TotalMemUsed()+growth > config.MaxMemory) {
		// An eviction can name an arbitrary old key. Until its transcript has
		// its own reservation, runs that may evict wait for the barrier.
		return 0, 0, false
	}
	for _, cmd := range commands {
		replyBytes += 256
		switch cmd.Cmd {
		case "GET", "MGET", "SET":
			keys := cmd.Args
			if cmd.Cmd == "SET" && len(keys) > 0 {
				keys = keys[:1]
			}
			for _, key := range keys {
				size := largestWrite
				if obj := dictStore.Peek(key); obj != nil {
					size = max(size, len(obj.Value))
				}
				replyBytes += size + 32
			}
		case "PING":
			for _, s := range cmd.Args {
				replyBytes += len(s) + 32
			}
		}
		if replyBytes > maxAsyncAppendBytes {
			return 0, 0, false
		}
	}
	return logBytes, replyBytes, true
}

// AppendOffset is a logical encoded prefix, independent of rewrite file sizes.
// Reads inherit this position so they cannot expose an unacknowledged mutation.
func AppendOffset() uint64      { return appendStarted + uint64(len(aof.buf)) }
func AppendReadyOffset() uint64 { return appendCompleted }
func AppendBufferedBytes() int  { return len(aof.buf) }
func AppendRetainedBytes() int  { return appendRetained + cap(aof.buf) }
func AppendHasRoom(reserve int) bool {
	// Twice the encoded length also reserves slice growth/allocator slack.
	return reserve >= 0 && appendRetained+2*(len(aof.buf)+reserve) <= maxAsyncAppendBytes
}

// AOFPositions are logical positions since open; rewrites never reset them.
// Synced is a conservative prefix: writes racing an everysec Sync remain dirty.
func AOFPositions() (encoded, written, synced, ready uint64) {
	return AppendOffset(), appendWritten, appendSynced, appendCompleted
}
