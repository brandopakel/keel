package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Opaque updates replace each dirty key's exact state. Size the complete set of
// records before allocating: deciding to fall back after emitKey constructed a
// huge image already paid the allocation and serialization cost. Commands and
// these two passes share the event-loop owner; no key changes between them.
func opaqueReplicationBody() ([]byte, bool) {
	size := 0
	for key := range replication.dirty {
		plan, present := planDump(key, replicationCommandBytes)
		var ok bool
		size, ok = replicationRecordSize(size, len("DEL"), len(key))
		if !ok {
			return nil, false
		}
		if !present {
			continue
		}
		if plan.size > replicationCommandBytes-9 {
			return nil, false
		}
		size, ok = replicationRecordSize(size, len("KEEL.RESTORE"), len(key), plan.size+9)
		if !ok {
			return nil, false
		}
		if replicationKeyExpiry(key) > 0 {
			// Twenty digits safely cover any persisted uint64 deadline.
			size, ok = replicationRecordSize(size, len("PEXPIREAT"), len(key), 20)
			if !ok {
				return nil, false
			}
		}
	}
	if size == 0 {
		return nil, true
	}
	body := make([]byte, 0, size)
	for key := range replication.dirty {
		body = appendCommand(body, "DEL", key)
		plan, present := planDump(key, replicationCommandBytes)
		if !present {
			continue
		}
		body = appendArrayHeader(body, 3)
		body = appendBulkString(body, "KEEL.RESTORE")
		body = appendBulkString(body, key)
		body = append(body, '$')
		body = strconv.AppendInt(body, int64(plan.size+9), 10)
		body = append(body, '\r', '\n')
		body = appendDump(body, plan)
		body = append(body, '\r', '\n')
		if expiry := replicationKeyExpiry(key); expiry > 0 {
			body = appendCommand(body, "PEXPIREAT", key, strconv.FormatUint(expiry, 10))
		}
	}
	return body, true
}

func replicationRecordSize(size int, lengths ...int) (int, bool) {
	if size > replicationCommandBytes-decimalDigits(len(lengths))-3 {
		return size, false
	}
	size += decimalDigits(len(lengths)) + 3
	for _, length := range lengths {
		framing := decimalDigits(length) + 5
		if length > replicationCommandBytes-size-framing {
			return size, false
		}
		size += length + framing
	}
	return size, true
}

func replicationKeyExpiry(key string) uint64 {
	var expiry uint64
	data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
		if at, present := ks.GetExpiry(key); present {
			expiry = at
		}
	})
	return expiry
}
