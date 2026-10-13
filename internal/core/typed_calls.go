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
//
// A transaction (Exec) runs typed calls too: each through its typed method,
// with a context that tells call it runs inside the block.

// call runs op on e in the scope of the command name, for the keys it names.
// It returns ctx's error if ctx is done before it runs, a refusal (ErrClosed,
// ErrPersistence, a replica's READONLY) without running it, the type check's
// refusal, or op's error; and once op has run, it waits as Do waits.
//
// name must be a command e has: a typed call is always one, and a name it
// does not have is a mistake in this package, reported as one. With the
// context Exec hands its calls, it runs op at once, in Exec's block.
func (e *Engine) call(ctx context.Context, name string, keys []string, op func() error) error {
	// Inside Exec's block the lock is held and the block was admitted whole.
	x, inBlock := ctx.(*execContext)
	if inBlock && x.e == e {
		if err := x.admit(name); err != nil {
			return err
		}
	} else if err := ctx.Err(); err != nil {
		return err
	} else {
		x = nil
	}
	entry := commands[name]
	if entry.run == nil {
		return fmt.Errorf("core: typed call of %s, which this engine does not have", name)
	}
	if x == nil {
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
	if x != nil {
		// Exec waits once, for the whole block.
		return result
	}
	return e.callReturn(ctx, result)
}

// callReturn is how a call returns once what it ran has run: with the log's
// failure, if that latched; at once, when nothing it wrote or could have read
// is still buffered, or while a failed write or sync is retried; and
// otherwise once the flush that covers it is done, as Do returns. The caller
// holds e's lock, which callReturn releases.
func (e *Engine) callReturn(ctx context.Context, result error) error {
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

// TxCall is a command a transaction runs: Name, the command it runs as, and
// Run, which runs it through the typed method that stands for it, with the
// context it is handed, and keeps what the method answers. Run makes at most
// one typed call, of Name: none when the method refuses an argument before
// it reaches the engine.
type TxCall struct {
	Name string
	Run  func(ctx context.Context)
}

// Exec runs calls as one transaction, as EXEC runs a MULTI block, through the
// same primitive (transactionBlock): under one hold of e's lock, so that
// nothing else runs between them; each in its own command's scope, in
// order; framed MULTI ... EXEC in the log and the protocol 2 stream when any
// of them writes; and eviction held off until the last has run. A call that
// fails fails alone: it neither stops the calls after it nor undoes those
// before, for there is no rollback, as in Redis. Each call's typed method
// answers it as it runs, with the context Exec hands its Run, which runs the
// method's typed call at once, in the block.
//
// The block is refused whole, before any of it runs, as Redis refuses EXEC:
// with ctx's error if ctx is done; ErrClosed, or ErrPersistence for a log
// whose failure latched; ErrPersistence while a failed write or sync is
// retried, if any call writes; or a replica's refusal of any call. Once the
// block has run, Exec waits as Do waits, and its error is the wait's: ctx's,
// or a failure of the log that latched; the calls ran.
//
// The transaction counts as the commands a client sends for it, MULTI and
// EXEC among them, as the server counts them (Transact); a replica's refusal
// answers EXECABORT, as EXEC's does, and is counted as that error.
func (e *Engine) Exec(ctx context.Context, calls []TxCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, c := range calls {
		if commands[c.Name].run == nil {
			return fmt.Errorf("core: typed call of %s, which this engine does not have", c.Name)
		}
	}
	e.mu.Lock()
	if err := e.callRefusal("EXEC"); err != nil {
		e.mu.Unlock()
		return err
	}
	for _, c := range calls {
		if err := e.callRefusal(c.Name); err != nil {
			e.mu.Unlock()
			return err
		}
	}
	for _, c := range calls {
		if err := e.replicaCommandError(c.Name); err != nil {
			e.totals.commands += 2
			e.noteError(execAbort(err))
			e.mu.Unlock()
			return err
		}
	}
	x := &execContext{Context: ctx, e: e}
	e.transactionBlock(len(calls), func(i int) {
		x.name, x.ran = calls[i].Name, false
		calls[i].Run(x)
	})
	e.totals.commands += 2
	return e.callReturn(ctx, nil)
}

// execContext is the context Exec hands the calls of its block: a typed call
// made with it runs at once, in the block, under the lock Exec holds, as
// EXEC runs a queued command - no refusal, since Exec has checked every call
// of the block, and no wait, since Exec waits once for the whole of it.
type execContext struct {
	context.Context
	e *Engine
	// name is the command the call running now was queued as, and ran
	// whether it has made its typed call.
	name string
	ran  bool
}

// admit lets the call running now in x's block make its typed call: the
// one it was queued as, once. Any other is a mistake in this package,
// reported as one, and runs nothing: Exec admitted the block by the names it
// was given.
func (x *execContext) admit(name string) error {
	if x.ran || name != x.name {
		return fmt.Errorf("core: a transaction's call queued as %s made a typed call of %s", x.name, name)
	}
	x.ran = true
	return nil
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
