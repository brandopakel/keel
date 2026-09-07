package core

import "github.com/brandopakel/keel/internal/data_structure"
import "github.com/brandopakel/keel/internal/config"

// The rule that decides whether a command can be admitted alongside a pending
// append is not "is it a write" but: can its canonical log record and its reply
// be bounded from the arguments and the current keyspace, without running it?
//
// Most commands can. The ones that cannot are those whose record depends on a
// result only execution produces - SPOP and ZPOPMIN stage the members they
// happened to remove, and nothing preflight knows which those are, or how long
// their names are. Those keep the drained barrier, which is not a limitation to
// be worked around: admitting a run and then discovering its transcript is
// larger than the budget is precisely the failure this exists to prevent.
//
// Everything here over-estimates. A bound that is too generous costs a barrier
// that was not strictly needed; a bound that is too tight is a violated budget.

// collectionEntryOverhead is charged per element added to a collection, for the
// slot, header and per-entry bookkeeping the stores keep. It is deliberately
// larger than any of them actually use.
const collectionEntryOverhead = 256
const collectionBaseOverhead = 1024

// replyFraming is charged per element in a reply, for the bulk-string header
// and terminator around it.
const replyFraming = 32

// AppendAdmission bounds the transcript, replies and keyspace growth BEFORE a
// parsed run executes alongside an append. Unmodelled commands use the drained
// barrier path. These conservative bounds cover errors, lazy expiry, SET's
// canonical expiry records and reads of values written earlier in this run.
// No store is touched: Peek and TotalMemUsed do not reap or update access state.
func AppendAdmission(commands []*Command) (logBytes, replyBytes int, ok bool) {
	growth, newKeys, largestWrite := uint64(0), 0, 0
	// Collections accumulate, and preflight runs before any of this executes,
	// so a read of a collection later in the run can return everything written
	// to any collection earlier in it. A string cannot: GET returns one value,
	// which is why largestWrite is enough there and this is needed here.
	collectionWritten := 0
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

		// Collection writes whose log record is the command as it arrived, so
		// the transcript is the arguments and the growth is the elements.
		case "HSET", "HSETNX":
			// HSET key field value [field value ...]
			if len(a) < 3 || (len(a)-1)%2 != 0 {
				return 0, 0, false
			}
			growth += uint64(len(a[0]) + collectionBaseOverhead)
			for _, s := range a[1:] {
				largestWrite = max(largestWrite, len(s))
				collectionWritten += len(s) + replyFraming
				growth += uint64(len(s) + collectionEntryOverhead)
			}
			newKeys++
		case "SADD", "LPUSH", "RPUSH":
			if len(a) < 2 {
				return 0, 0, false
			}
			growth += uint64(len(a[0]) + collectionBaseOverhead)
			if cmd.Cmd == "LPUSH" || cmd.Cmd == "RPUSH" {
				if list, ok := listStore.Peek(a[0]); ok {
					// A single push at capacity can double a large ring. Reserving its
					// current slots plus per-added-element slack also covers a threshold
					// crossed by several pushes later in this unexecuted run.
					growth += list.ReservedSlotBytes()
				}
			}

			for _, s := range a[1:] {
				largestWrite = max(largestWrite, len(s))
				collectionWritten += len(s) + replyFraming
				growth += uint64(len(s) + collectionEntryOverhead)
			}
			newKeys++
		case "ZADD":
			// Flags are rejected rather than modelled: NX/XX/CH change which
			// members land, and GT/LT/INCR change the record. Score-member
			// pairs only, which is the shape that matters here.
			if len(a) < 3 || (len(a)-1)%2 != 0 {
				return 0, 0, false
			}
			growth += uint64(len(a[0]) + collectionBaseOverhead)
			for i := 1; i < len(a); i += 2 {
				if _, err := parseZScore(a[i]); err != nil {
					return 0, 0, false
				}
				largestWrite = max(largestWrite, len(a[i+1]))
				collectionWritten += len(a[i]) + len(a[i+1]) + 2*replyFraming
				growth += uint64(len(a[i+1]) + collectionEntryOverhead)
			}
			newKeys++

		// Removals cannot grow the keyspace, so only the transcript is charged.
		// LPOP and RPOP are absent: with a count they may empty the key, and an
		// emptied collection is deleted, which is a removal record this cannot
		// size without knowing how many elements are actually there.
		case "HDEL", "SREM", "ZREM":
			if len(a) < 2 {
				return 0, 0, false
			}

		// Reads whose reply is a fixed size or a single element.
		case "GET", "MGET", "PING", "EXISTS", "TYPE", "TTL", "PTTL",
			"HLEN", "HEXISTS", "LLEN", "SCARD", "SISMEMBER", "SMISMEMBER",
			"ZCARD", "ZSCORE", "ZRANK":

		// Reads that can return a whole key, so the bound needs the key name.
		// SRANDMEMBER is absent on purpose: a negative count may repeat members
		// and so is not bounded by what the set holds.
		case "HGET", "HMGET", "HGETALL", "HKEYS", "HVALS",
			"SMEMBERS", "LINDEX", "LRANGE", "ZRANGE":
			if len(a) == 0 {
				return 0, 0, false
			}
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
		// An eviction can name an arbitrary old key, and neither how many it
		// removes nor which ones is knowable before the run executes, so its
		// transcript cannot be reserved. Runs that may evict take the barrier.
		// docs/eviction-reservation.md costs the alternatives and says why this
		// is the answer rather than a gap waiting to be filled.
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
				replyBytes += size + replyFraming
			}
		case "HGET", "HMGET":
			replyBytes += hashReadReplyBound(cmd.Args[0], cmd.Args[1:], collectionWritten)
		case "SMISMEMBER":
			replyBytes += max(0, len(cmd.Args)-1) * replyFraming
		case "HGETALL", "HKEYS", "HVALS", "SMEMBERS", "LINDEX", "LRANGE":
			replyBytes += collectionReplyBound(cmd.Args[0], collectionWritten)
		case "ZRANGE":
			// WITHSCORES doubles the elements, and a score is short beside the
			// member it belongs to, so twice the bound covers both forms.
			replyBytes += 2 * collectionReplyBound(cmd.Args[0], collectionWritten)
		case "PING":
			for _, s := range cmd.Args {
				replyBytes += len(s) + replyFraming
			}
		}
		if replyBytes > maxAsyncAppendBytes {
			return 0, 0, false
		}
	}
	return logBytes, replyBytes, true
}

// HMGET may request the same field repeatedly. Bound every requested result,
// including values created by earlier commands in this not-yet-executed run.
func hashReadReplyBound(key string, fields []string, written int) int {
	h, exists := hashStore.Peek(key)
	total := 0
	for _, field := range fields {
		size := written
		if exists {
			value, _ := h.Get(field)
			size = max(size, len(value))
		}
		total += size + replyFraming
		if total > maxAsyncAppendBytes {
			return total
		}
	}
	return total
}

// collectionReplyBound is what a reply reading the whole of one key can come to.
//
// MemUsage already counts the element bytes the store holds, so the reply is
// that plus framing for each element, plus everything written to any collection
// earlier in this run. Charging the run's whole collection growth to every such
// read assumes it all went to this key, which is the safe direction: preflight
// runs before any of it has executed and cannot know where it landed.
func collectionReplyBound(key string, writtenThisRun int) int {
	held, elements := uint64(0), 0
	if h, ok := hashStore.Peek(key); ok {
		held, elements = h.MemUsage(), 2*h.Len()
	} else if s, ok := setStore.Peek(key); ok {
		held, elements = s.MemUsage(), s.Len()
	} else if l, ok := listStore.Peek(key); ok {
		held, elements = l.MemUsage(), l.Len()
	} else if z, ok := zsetStore.Peek(key); ok {
		held, elements = z.MemUsage(), z.Len()
	}
	return int(held) + elements*replyFraming + writtenThisRun + replyFraming
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
