package core

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Calls: how an embedded caller runs a command on an engine Open made, and
// when the call may return (plan phase 3).
//
// A call takes the engine's lock, runs the command in the scope
// EvalAndResponse gives it, notes where the log ends, and lets go of the lock.
// It returns once the published offset, appendCompleted, covers that end:
// written under everysec and no, synced under always - the gate the server
// holds its replies to. If its records are still buffered, it wakes the
// maintenance goroutine, whose next flush covers every call that buffered
// records since the last one, and waits for that flush outside the lock: one
// write, and under always one sync, for all of them.
//
// A read waits too, when anything is buffered: it may have seen a write that
// is not on disk yet, and nobody the server answers is told what a crash could
// still lose. With no log nothing is ever buffered, and a call is the lock,
// the command and the unlock.
//
// A failed write or sync of the log does what Redis 8.10.1's does
// (docs/embedding-plan.md, "Log failures, as Redis's"). Under everysec and no
// it is retried: while it is, write commands and PING are refused with
// ErrPersistence, Redis's MISCONF error, reads are served, a command that ran
// before the failure was known keeps its reply and has its record written on
// recovery, and a write or sync that succeeds clears it. Under always, where
// Redis exits, it latches: every later call, read or write, returns
// ErrPersistence without running, as does a call waiting for the failed write
// or sync, until the engine is reopened. The server does neither in phase 3;
// it stops at the first failure.

// ErrPersistence is what a call refused for a failed log returns: Redis's
// wire error for a failed AOF write, followed by the failure itself.
var ErrPersistence = errors.New("MISCONF Errors writing to the AOF file")

// errNotDriven refuses a call on an engine nothing but its maker drives: the
// maintenance goroutine is what flushes for a call, and only Open starts one.
var errNotDriven = errors.New("calls need an engine Open made: nothing flushes the log for them otherwise")

// Do runs cmd on e for an embedded caller, writing its reply to w, and
// returns once everything the call wrote, or could have read, is as durable
// as e's fsync policy makes a reply's.
//
// The error is ErrClosed after Close; ErrPersistence, wrapping the failure,
// once e's log has failed; the context's, if ctx is done before the call runs
// or while it waits; or what EvalAndResponse returns, a command this server
// does not have. A call whose context is done while it waits has run, and its
// records will be written, as a Redis client that times out has had its
// command run.
//
// It unlocks at each return rather than with defer: a deferred method is
// wrapped in a closure and called through it (plan step 2.3). A panic in a
// command is not recovered, as on the server, which it ends; it leaves e
// locked.
func (e *Engine) Do(ctx context.Context, cmd *Command, w io.ReadWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if err := e.callRefusal(cmd); err != nil {
		e.mu.Unlock()
		return err
	}
	result := e.EvalAndResponse(cmd, w)
	// A command can fail the log itself, draining a large record: then what
	// it did is not in the log, and it did not succeed.
	if failure := e.persistenceFailure(); failure != nil {
		e.mu.Unlock()
		return failure
	}
	// While a failed write or sync is retried, the command's reply stands, as
	// Redis's does, and its record is written when the log recovers.
	end := e.AppendOffset()
	if end <= e.appendCompleted || e.logRetrying() {
		e.mu.Unlock()
		return result
	}
	return e.waitPublished(ctx, end, result)
}

// callRefusal is why cmd may not run on e, or nil: e is closed, or nothing
// flushes its log for it, or its log has failed - every call, where the
// failure latches, and while it is retried, write commands and PING, as
// Redis's processCommand refuses them (server.c, writeCommandsDeniedByDiskError).
// The caller holds e's lock.
func (e *Engine) callRefusal(cmd *Command) error {
	switch {
	case e.closed:
		return ErrClosed
	case e.driver == nil:
		return errNotDriven
	}
	if failure := e.persistenceFailure(); failure != nil {
		return failure
	}
	if failure := e.retriedFailure(); failure != nil && (writeCommands[cmd.Cmd] || cmd.Cmd == "PING") {
		return failure
	}
	return nil
}

// logRetrying says e's log has a failed write or sync it is retrying (driver.go).
func (e *Engine) logRetrying() bool {
	d := e.driver
	return d != nil && (d.writeFailure != nil || d.syncFailure != nil)
}

// retriedFailure is the failed write or sync e's log is retrying, as
// ErrPersistence: Redis's "MISCONF Errors writing to the AOF file: <cause>".
// nil while there is none. The caller holds e's lock.
func (e *Engine) retriedFailure() error {
	d := e.driver
	switch {
	case d.writeFailure != nil:
		return fmt.Errorf("%w: %w", ErrPersistence, d.writeFailure)
	case d.syncFailure != nil:
		return fmt.Errorf("%w: %w", ErrPersistence, d.syncFailure)
	}
	return nil
}

// persistenceFailure latches a failure of e's log that is not retried, and
// returns it as ErrPersistence; nil while there is none. The caller holds e's
// lock.
func (e *Engine) persistenceFailure() error {
	d := e.driver
	if d.failed == nil {
		d.failed = e.aof.failed
	}
	if d.failed == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPersistence, d.failed)
}

// waitPublished returns result once e's published offset covers end, or once
// e's log has failed in a way it retries, when the reply stands as Redis's
// does; the failure if e's log fails in a way that latches; or ctx's error if
// ctx is done first. The caller holds e's lock, which waitPublished releases.
func (e *Engine) waitPublished(ctx context.Context, end uint64, result error) error {
	d := e.driver
	d.waiters.Add(1)
	for {
		if e.appendCompleted >= end || e.logRetrying() {
			d.waiters.Add(-1)
			e.mu.Unlock()
			return result
		}
		if failure := e.persistenceFailure(); failure != nil {
			d.waiters.Add(-1)
			e.mu.Unlock()
			return failure
		}
		// The next publication, by a flush that takes the lock after this
		// call let go of it, covers everything the call buffered.
		published := d.published
		e.mu.Unlock()
		d.wake()
		select {
		case <-published:
		case <-ctx.Done():
			// Without the lock: a flush may hold it as long as the disk
			// takes, and the caller asked not to wait that long.
			d.waiters.Add(-1)
			return ctx.Err()
		}
		e.mu.Lock()
	}
}
