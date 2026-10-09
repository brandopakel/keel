package core

import (
	"time"
	"unicode"
	"unicode/utf8"
)

// Log failures, as Redis 8.10.1's: what a failed write or sync of the log
// does under everysec and no, for the server and for an engine Open makes
// alike (docs/embedding-plan.md, "Log failures, as Redis's").
//
// A failed write leaves what it did not write buffered and puts the log's
// write status in error, as flushAppendOnlyFile sets aof_last_write_status
// (aof.c 1539-1552); a failed background sync puts its sync status in error,
// as bio.c's fsync job sets aof_bio_fsync_status (bio.c 320-329). While
// either is in error, write commands and PING are refused with MISCONF and
// reads are served, as processCommand refuses them (server.c 4675-4697); the
// flush retries every cycle, and a write or sync that succeeds clears its
// status. Under always, where Redis exits, a failure latches instead
// (aof.failed), and so it does for a replica, which Redis stops ("Replica
// was unable to write command to disk", server.c 4680-4681), and with the
// server's asynchronous append, whose batch is not retried.

// logFailures is e's log's write and sync statuses while they are in error,
// nil when they are not. failing is set while either is, so that a command's
// path asks one field. writeLogged is when a failed write was last logged, at
// most once in writeLogEvery, as Redis logs it.
type logFailures struct {
	write, sync error
	failing     bool
	writeLogged time.Time
}

// writeLogEvery is how often a write of the log that keeps failing is
// logged: Redis's AOF_WRITE_LOG_ERROR_RATE.
const writeLogEvery = 30 * time.Second

// RetriesLogFailures says a failed write or sync of e's log is retried rather
// than latched: under everysec and no, unless e is a replica or appends on
// the server's worker. The caller holds e's lock.
func (e *Engine) RetriesLogFailures() bool { return e.retriesLogFailures() }

func (e *Engine) retriesLogFailures() bool {
	return e.settings.fsync != FsyncAlways && !e.settings.asyncAppend && e.replicaOf() == ""
}

// writeFailed puts the log's write status in error, as Redis's
// flushAppendOnlyFile does when a write under everysec or no fails.
func (e *Engine) writeFailed(err error) {
	f := &e.logFailure
	f.write, f.failing = err, true
	if now := time.Now(); now.Sub(f.writeLogged) > writeLogEvery {
		f.writeLogged = now
		aofLog("error writing to the log: %v", err)
	}
}

// writeSolved clears the log's write status once a write succeeds, as Redis
// clears aof_last_write_status and logs that it can write again.
func (e *Engine) writeSolved() {
	f := &e.logFailure
	f.write, f.failing = nil, f.sync != nil
	aofLog("write error looks solved; writes are accepted again")
}

// syncFailed puts the log's sync status in error, as Redis's background fsync
// job does, logging it when it was not already.
func (e *Engine) syncFailed(err error) {
	f := &e.logFailure
	if f.sync == nil {
		aofLog("failed to sync the log: %v", err)
	}
	f.sync, f.failing = err, true
}

// syncSolved clears the log's sync status once a sync succeeds.
func (e *Engine) syncSolved() {
	f := &e.logFailure
	f.sync, f.failing = nil, f.write != nil
}

// logRetrying says e's log has a failed write or sync it is retrying.
func (e *Engine) logRetrying() bool { return e.logFailure.failing }

// LogRetrying says e's log has a failed write or sync it is retrying, as
// Redis's writeCommandsDeniedByDiskError does, so that a flush's error is not
// the server's to stop on. The caller holds e's lock.
func (e *Engine) LogRetrying() bool { return e.logFailure.failing }

// writeRetrying says e's log has a failed write it is retrying, so that what
// is appended stays buffered for that retry (appendAOFFragment).
func (e *Engine) writeRetrying() bool { return e.logFailure.write != nil }

// retriedFailure is the failed write or sync e's log is retrying, as
// ErrPersistence: Redis's "MISCONF Errors writing to the AOF file: <cause>".
// nil while there is none.
func (e *Engine) retriedFailure() error {
	switch f := &e.logFailure; {
	case f.write != nil:
		return persistenceError{f.write}
	case f.sync != nil:
		return persistenceError{f.sync}
	}
	return nil
}

// diskErrorRefusal is why cmd may not run while e's log retries a failed
// write or sync: write commands and PING are refused, as Redis refuses them,
// so that a health check sees the failure too (server.c 4675-4697).
func (e *Engine) diskErrorRefusal(cmd string) error {
	if writeCommands[cmd] || cmd == "PING" {
		return e.retriedFailure()
	}
	return nil
}

// persistenceError is a refusal for a failed log: ErrPersistence, followed by
// its cause in the words Redis gives it.
type persistenceError struct{ cause error }

func (p persistenceError) Error() string {
	return ErrPersistence.Error() + ": " + causeText(p.cause)
}

func (p persistenceError) Unwrap() []error { return []error{ErrPersistence, p.cause} }

// causeText is a failure of the log in the words Redis's MISCONF reply gives
// it, strerror(aof_last_write_errno): a short write is ENOSPC, as Redis
// records it (aof.c 1527), and an errno is its strerror text, which is Go's
// text for it with its first letter capitalised, on Linux and macOS alike
// ("No space left on device"). Any other failure, which no errno names, is
// Go's own text.
func causeText(err error) string {
	text, ok := errnoText(err)
	if !ok {
		return err.Error()
	}
	first, size := utf8.DecodeRuneInString(text)
	return string(unicode.ToUpper(first)) + text[size:]
}
