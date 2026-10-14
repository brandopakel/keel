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
func (e *Engine) oomRefuses(cmd string) bool {
	return denyOOMCommands[cmd] && e.replicaOf() == "" && e.space.OverLimit()
}
