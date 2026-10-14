package core

import "errors"

// errOOM is Redis's refusal of a command that would grow the data set while
// used_memory is over maxmemory (shared.oomerr, server.c).
var errOOM = errors.New("OOM command not allowed when used memory > 'maxmemory'.")

// denyOOMCommands are the commands whose spec carries DENYOOM, which Redis
// refuses while memory is over its bound.
var denyOOMCommands = func() map[string]bool {
	deny := make(map[string]bool)
	for name, spec := range redisCommandSpecs {
		if spec.hasFlag("DENYOOM") {
			deny[name] = true
		}
	}
	for name, spec := range keelCommandSpecs {
		if spec.hasFlag("DENYOOM") {
			deny[name] = true
		}
	}
	return deny
}()

// oomRefuses reports whether cmd is refused for memory: a DENYOOM command,
// while the keyspace is over its bound and eviction cannot bring it back, as
// Redis's processCommand refuses it when performEvictions fails (server.c,
// evict.c). Under noeviction nothing is evicted, and under a volatile policy
// only keys that carry a TTL are, so a keyspace may stay over. An allkeys
// policy always brings it back, since every write evicts until the keyspace
// is inside its bounds; its caller tests the policy first
// (EvictionPolicy.MayStayOver), so those policies pay one test of a bit and no
// call. A replica ignores maxmemory, as Redis's does by default
// (replica-ignore-maxmemory), and refuses nothing for it.
//
// It keeps Redis's eviction exceeded time as it looks, as performEvictions
// does before each command (update_metrics).
func (e *Engine) oomRefuses(cmd string) bool {
	if e.replicaOf() != "" {
		// Redis's replica returns before it measures anything.
		return false
	}
	over := e.space.OverLimit()
	e.noteEvictionExceeded(over)
	return over && denyOOMCommands[cmd]
}

// noteEvictionExceeded starts the time the keyspace is over its bound with
// nothing to evict, or adds it to the total once the keyspace is back inside,
// as Redis's performEvictions does when it fails and when it next succeeds
// (evict.c, update_metrics).
func (e *Engine) noteEvictionExceeded(over bool) {
	switch {
	case over && e.evictionExceededSince == 0:
		e.evictionExceededSince = nanotime()
	case !over && e.evictionExceededSince != 0:
		e.evictionExceededNanos += nanotime() - e.evictionExceededSince
		e.evictionExceededSince = 0
	}
}

// evictionExceeded is INFO's current_eviction_exceeded_time and
// total_eviction_exceeded_time, in nanoseconds: how long the keyspace has now
// been over its bound with nothing to evict, and that with the time it was so
// before.
func (e *Engine) evictionExceeded() (current, total int64) {
	if e.evictionExceededSince != 0 {
		current = nanotime() - e.evictionExceededSince
	}
	return current, e.evictionExceededNanos + current
}
