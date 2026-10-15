package core

import (
	"fmt"
	"strings"
)

// This file keeps Redis's eviction exceeded time: how long the keyspace has
// been over its bound with nothing its policy may evict, which INFO reports
// as total_eviction_exceeded_time and current_eviction_exceeded_time. It
// sorts after the command handlers, so that its code moves none of them.

// overLimitNoted reports whether the keyspace is over its bound, and keeps
// the eviction exceeded time by it, as Redis's performEvictions does before
// each command: it starts the time when it fails, and adds it to the total
// once it next succeeds (evict.c, update_metrics).
func (e *Engine) overLimitNoted() bool {
	over := e.space.OverLimit()
	e.noteEvictionExceeded(over)
	return over
}

// noteEvictionExceeded starts the time the keyspace is over its bound with
// nothing to evict, or adds it to the total once the keyspace is back inside.
func (e *Engine) noteEvictionExceeded(over bool) {
	switch {
	case over && e.evictionExceededSince == 0:
		e.evictionExceededSince = nanotime()
	case !over && e.evictionExceededSince != 0:
		e.evictionExceededNanos += nanotime() - e.evictionExceededSince
		e.evictionExceededSince = 0
	}
}

// settleEvictionExceeded ends the time over the bound when CONFIG SET has
// chosen a policy that never leaves the keyspace over it: an allkeys policy,
// under which the check that would end it is not made.
func (e *Engine) settleEvictionExceeded() {
	if !e.space.EvictionPolicy().MayStayOver() {
		e.noteEvictionExceeded(false)
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

// evictionExceededInfo writes the two to INFO's Stats section, in
// milliseconds, as Redis divides its microseconds.
func (e *Engine) evictionExceededInfo(b *strings.Builder) {
	current, total := e.evictionExceeded()
	fmt.Fprintf(b, "total_eviction_exceeded_time:%d\r\ncurrent_eviction_exceeded_time:%d\r\n", total/1e6, current/1e6)
}
