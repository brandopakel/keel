package core

import (
	"context"
	"fmt"
)

// Typed calls: how the public package runs a typed operation for a Go caller
// (docs/embedding-plan.md, phase 5).
//
// A typed call is a command without its words: it runs an operation in the
// scope a command of the same name runs in - the instance's lock, the
// refusals Do makes (a closed instance, a failed log), a replica's refusal of
// a write, the log's staging, the type check of the keys it names against the
// command's descriptor, eviction held off around it - and returns once what
// it wrote, or could have read, is as durable as a reply's, as Do does. The
// operation stages every record of what it changes, as every converted
// command's does, so a typed call is logged exactly as the command it stands
// for would have been, and never as anything it was not.
//
// Nothing is parsed and nothing is encoded: the public package hands Go
// values in and takes Go values and errors out. The errors are the typed
// operations' sentinels, whose text is Redis's wire error; ErrorCategory
// sorts them into the public package's categories.

// call runs op on e in the scope of the command name, for the keys it names.
// It returns ctx's error if ctx is done before it runs, a refusal (ErrClosed,
// ErrPersistence, a replica's READONLY) without running it, the type check's
// refusal, or op's error; and once op has run, it waits as Do waits.
//
// name must be a command e has: a typed call is always one, and a name it
// does not have is a mistake in this package, reported as one.
func (e *Engine) call(ctx context.Context, name string, keys []string, op func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry := commands[name]
	if entry.run == nil {
		return fmt.Errorf("core: typed call of %s, which this engine does not have", name)
	}
	e.mu.Lock()
	if err := e.callRefusal(name); err != nil {
		e.mu.Unlock()
		return err
	}
	if err := e.replicaCommandError(name); err != nil {
		e.noteError(Encode(err, false))
		e.mu.Unlock()
		return err
	}
	e.aofBegin(name)
	result := e.typeCheckOf(name, entry, keys)
	if result == nil {
		suspended := e.space.SuspendEviction
		e.space.SuspendEviction = true
		result = op()
		e.totals.commands++
		if result != nil {
			e.noteError(Encode(result, false))
		}
		e.aofCommitCall(name, keys)
		e.space.SuspendEviction = suspended
		e.space.EnforceLimits()
	} else {
		// Redis finds the wrong type inside the command, so this counts as a
		// command that ran and failed, as EvalAndResponse counts it.
		e.totals.commands++
		e.noteError(Encode(result, false))
		e.aofCommitCall(name, keys)
	}
	e.aofEnd()
	if failure := e.persistenceFailure(); failure != nil {
		e.mu.Unlock()
		return failure
	}
	end := e.AppendOffset()
	if end <= e.appendCompleted || e.logRetrying() {
		e.mu.Unlock()
		return result
	}
	return e.waitPublished(ctx, end, result)
}

// typeCheckOf is checkKeyTypes for keys a typed call names: the same check,
// against the same descriptor, counting the same lookups, of keys handed in
// rather than found among a command's arguments. A typed call's arguments
// are Go values its caller has already given the right types, so nothing
// Redis would read ahead of the key (argumentsBeforeType) can be malformed.
func (e *Engine) typeCheckOf(name string, entry commandEntry, keys []string) error {
	space := entry.typed
	if space == "" {
		return nil
	}
	for i, key := range keys {
		owner, held := e.space.OwnerOf(key)
		if entry.reads {
			e.noteLookup(held)
		}
		if held && owner.KeyspaceName() != space {
			if entry.reads && entry.keys == keyFirstTwo {
				for _, rest := range keys[i+1:] {
					_, held := e.space.OwnerOf(rest)
					e.noteLookup(held)
				}
			}
			if entry.readsLate {
				e.noteLookup(true)
			}
			return e.typeError(name, owner)
		}
	}
	return nil
}

// aofCommitCall is aofCommit for a typed call, which records itself: the
// keys it names are the ones the rewrite and the replication stream are told
// it may have written, and the log takes exactly what its operation staged.
func (e *Engine) aofCommitCall(name string, keys []string) {
	if e.aof.file == nil || e.aof.replaying {
		e.clearStaged()
		return
	}
	if e.rewrite.active {
		for _, key := range keys {
			e.noteRewriteDirty(key)
		}
	}
	if writeCommands[name] {
		for _, key := range keys {
			e.noteReplicationDirty(key)
		}
	}
	if len(e.aof.staged) > 0 {
		e.appendStaged()
	}
	e.clearStaged()
}
