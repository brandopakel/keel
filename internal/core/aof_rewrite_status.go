package core

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// What a failed rewrite leaves behind, and when the next one may start.
//
// A rewrite writes the new log beside the old one and gives it the log's name
// only once it is complete and on disk. Until that rename the log being
// appended to is the old one, and every write has gone into it throughout, so a
// rewrite that fails before the rename has lost nothing. The server keeps
// serving and keeps appending to the old log, which is what Redis does
// (backgroundRewriteDoneHandler in aof.c): it logs "Background AOF rewrite
// terminated with error", removes the temporary file, reports
// aof_last_bgrewrite_status:err and counts aof_rewrites_consecutive_failures in
// INFO persistence, and carries on.
//
// Every step up to and including the rename fails that way: a snapshot write
// or its sync on the worker, a dirty-tail write, the final sync, closing the
// new file, opening it for appending, the rename, and the budgets that abandon
// a rewrite that would not finish. So does a refusal to start, which Redis
// reports the same way without counting it as a consecutive failure.
//
// # After the rename
//
// Once the rename has happened the new file is the log and there is no going
// back: the old file no longer has a name, and anything appended to it would
// vanish at the next restart. The one step left that can fail, syncing the
// directory so the rename itself is durable, is therefore not a failed rewrite.
// The new file stays the log, and the directory sync is tried again before the
// log's next sync, which under appendfsync always is before anything appended
// to the new file is acknowledged. If it fails again, that is a failed sync of
// the log, and the server stops as it does for any failed sync of the log.
// Until then a crash leaves either file under the name, and each holds every
// write acknowledged before the rename.
//
// Redis keeps its log in several files named by a manifest, and the file it
// appends to does not change at the swap, so its equivalent failure - the
// manifest's directory sync - is safe to report as a failed rewrite. Keel's log
// is one file whose name moves, so it is not.
//
// # Retrying
//
// An automatic rewrite that failed is tried again as Redis tries one again
// (aofRewriteLimited in aof.c). The first two failures in a row are retried at
// the next check; Redis checks ten times a second, and so a failed attempt here
// is followed by the next one no sooner than 100 ms later. From the third
// failure in a row each attempt waits, a minute at first and twice as long each
// time after, up to an hour. A full disk therefore costs three attempts and
// then one an hour at most, never a loop. A BGREWRITEAOF that can start is not
// held back by any of this, in Redis or here: it starts at once. One that is
// only scheduled, inside EXEC, waits for the limit like an automatic rewrite,
// as Redis's serverCron starts aof_rewrite_scheduled only when
// !aofRewriteLimited(). Protocol 2 snapshot rewrites share the failure count,
// so that wait can reach the hour too.
//
// A rewrite abandoned on its duration or dirty-key budget is a failure too, but
// the next automatic attempt also waits the minute it always has, since the
// load that outran one rewrite would outrun the next. A rewrite that a
// protocol 2 replica needs for its snapshot is paced by the same limit, so a
// replica waiting for one does not turn a full disk into a loop either.

const (
	// rewriteRetryThreshold and rewriteRetryMaxDelay are Redis's
	// AOF_REWRITE_LIMITE_THRESHOLD and AOF_REWRITE_LIMITE_MAX_MINUTES.
	rewriteRetryThreshold = 3
	rewriteRetryMaxDelay  = time.Hour
	// rewriteRetryTick is Redis's serverCron period at its default hz of 10,
	// the soonest Redis looks again after a failure.
	rewriteRetryTick = 100 * time.Millisecond
)

var rewriteOutcome struct {
	// lastErr is why the last rewrite failed or could not start, and nil once
	// one has finished: aof_last_bgrewrite_status.
	lastErr error
	// failures counts rewrites in a row that started and did not finish:
	// aof_rewrites_consecutive_failures.
	failures int64
	// scheduled is a BGREWRITEAOF waiting to start: aof_rewrite_scheduled.
	scheduled bool
	// lastSeconds is how long the last rewrite to end took, finished or not,
	// counted in whole seconds as Redis counts it, and -1 until one has ended.
	lastSeconds int64
	// delay and limitedUntil are aofRewriteLimited's next_delay_minutes and
	// next_rewrite_time.
	delay        time.Duration
	limitedUntil time.Time
}

// unsyncedLogDir names the directory whose entry for the log is not yet known
// to be durable, because the sync after a rewrite's rename failed.
var unsyncedLogDir string

// The steps of the handoff, injectable so tests can fail each one.
var (
	rewriteRename  = os.Rename
	rewriteSyncDir = syncDir
	// rewriteOpenLog opens the finished file for appending before it takes
	// the log's name, so that nothing after the rename can fail to reach it.
	rewriteOpenLog = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	}
)

var (
	errRewriteInProgress = errors.New("ERR Background append only file rewriting already in progress")
	errRewriteCantStart  = errors.New("ERR Can't execute an AOF background rewriting. Please check the server logs for more information.")
)

// rewriteStartError is a rewrite that could not start, because its file could
// not be created, as a Redis fork can fail, or because of Keel's key ceiling,
// which Redis does not have. Both are reported as Redis reports a failed start:
// a failed rewrite that is not counted as a consecutive failure, and
// BGREWRITEAOF's generic refusal.
type rewriteStartError struct{ err error }

func (e *rewriteStartError) Error() string { return e.err.Error() }
func (e *rewriteStartError) Unwrap() error { return e.err }

// refuseRewriteStart records a start that failed and returns it.
func refuseRewriteStart(err error) error {
	rewriteOutcome.lastErr = err
	aofLog("Can't rewrite append only file in background: %v", err)
	return &rewriteStartError{err}
}

func resetRewriteOutcome() {
	rewriteOutcome.lastErr = nil
	rewriteOutcome.failures = 0
	rewriteOutcome.scheduled = false
	rewriteOutcome.lastSeconds = -1
	rewriteOutcome.delay = 0
	rewriteOutcome.limitedUntil = time.Time{}
	unsyncedLogDir = ""
	snapshotRetryAt = time.Time{}
}

func init() { rewriteOutcome.lastSeconds = -1 }

// noteRewriteFailed records a rewrite that ended without replacing the log.
func noteRewriteFailed(cause error, started time.Time, path string) {
	rewriteOutcome.lastErr = cause
	rewriteOutcome.failures++
	rewriteOutcome.lastSeconds = time.Now().Unix() - started.Unix()
	aofLog("Background AOF rewrite terminated with error: %v; %s is still the log and keeps every write", cause, path)
}

// noteRewriteFinished records a rewrite whose file is now the log.
func noteRewriteFinished(started time.Time) {
	rewriteOutcome.lastErr = nil
	rewriteOutcome.failures = 0
	rewriteOutcome.lastSeconds = time.Now().Unix() - started.Unix()
	aofLog("Background AOF rewrite finished successfully")
}

// rewriteLimited is Redis's aofRewriteLimited. It is asked only when a rewrite
// would otherwise start now, and the caller starts one when it answers false.
func rewriteLimited(now time.Time) bool {
	o := &rewriteOutcome
	if o.failures < rewriteRetryThreshold {
		o.delay, o.limitedUntil = 0, time.Time{}
		return false
	}
	if !o.limitedUntil.IsZero() {
		if now.Before(o.limitedUntil) {
			return true
		}
		o.limitedUntil = time.Time{}
		return false
	}
	if o.delay == 0 {
		o.delay = time.Minute
	} else {
		o.delay = min(2*o.delay, rewriteRetryMaxDelay)
	}
	o.limitedUntil = now.Add(o.delay)
	aofLog("Background AOF rewrite has repeatedly failed and triggered the limit, will retry in %d minutes", int(o.delay/time.Minute))
	return true
}

// snapshotRetryAt holds back the rewrite a protocol 2 pull would start after
// one finished but its snapshot could not be opened.
var snapshotRetryAt time.Time

// snapshotRewriteAllowed says whether a protocol 2 pull may start the rewrite
// its snapshot needs now.
func snapshotRewriteAllowed() bool {
	now := time.Now()
	return !now.Before(snapshotRetryAt) && !rewriteLimited(now)
}

// startSnapshotRewrite starts the rewrite a protocol 2 pull needs. A replica
// pulls several times a second, so one that cannot start for a reason that
// will not pass by itself is not tried again for a minute, as an automatic one
// is not; a pull meanwhile is told to wait. A transient refusal, such as a
// pending append, is retried by the next pull.
func startSnapshotRewrite() error {
	err := StartRewrite()
	var refused *rewriteStartError
	if errors.As(err, &refused) {
		snapshotRetryAt = time.Now().Add(time.Minute)
	}
	return err
}

// logStartFailure logs a rewrite the server started itself that could not
// start, unless refuseRewriteStart has logged it already.
func logStartFailure(what string, err error) {
	var refused *rewriteStartError
	if !errors.As(err, &refused) {
		aofLog("%s failed to start: %v", what, err)
	}
}

// startScheduledRewrite starts a BGREWRITEAOF that could not start when it
// was asked for. Redis keeps one scheduled through a failed start and tries
// again ten times a second; here it is dropped after a failed start, which is
// logged and reported, rather than retried in a loop.
func startScheduledRewrite(now time.Time) {
	if ready, _ := pollRewriteIO(false); !ready || AppendPending() || len(aof.buf) > 0 {
		return // the worker that finishes that wakes the loop again
	}
	if rewriteLimited(now) {
		return
	}
	rewriteOutcome.scheduled = false
	if err := StartRewrite(); err != nil {
		logStartFailure("scheduled rewrite", err)
	}
}

// bgRewriteAOF answers BGREWRITEAOF as Redis 8.10.1's bgrewriteaofCommand
// does: an error if one is running, scheduled inside a transaction, started
// otherwise - after a failed one too, and whatever the retry limit says,
// though a scheduled one waits for that limit before it starts - and
// Redis's generic refusal when it cannot start. Keel's own refusals, a log
// that is off or a rewrite waiting for a pending append, keep their reasons.
func bgRewriteAOF() []byte {
	if aof.file != nil && rewrite.active {
		return Encode(errRewriteInProgress, false)
	}
	if aof.file != nil && aof.transaction {
		// Redis starts nothing in the middle of EXEC. The rewrite starts once
		// the transaction is over, and a BGREWRITEAOF asked for this way
		// clears the failures holding automatic ones back, as in Redis.
		rewriteOutcome.scheduled = true
		rewriteOutcome.failures = 0
		return Encode("Background append only file rewriting scheduled", true)
	}
	if err := StartRewrite(); err != nil {
		var refused *rewriteStartError
		if errors.As(err, &refused) {
			return Encode(errRewriteCantStart, false)
		}
		return Encode(fmt.Errorf("ERR %w", err), false)
	}
	return Encode("Background append only file rewriting started", true)
}

// syncPendingLogDir finishes a rename whose directory sync failed. It runs
// before the log's own sync, so nothing appended to the new file is reported
// synced, or acknowledged under appendfsync always, before its name is durable.
func syncPendingLogDir() error {
	if unsyncedLogDir == "" {
		return nil
	}
	if err := rewriteSyncDir(unsyncedLogDir); err != nil {
		return fmt.Errorf("syncing %s after the rewrite's rename: %w", unsyncedLogDir, err)
	}
	aofLog("synced %s: the rewritten log's name is durable", unsyncedLogDir)
	unsyncedLogDir = ""
	return nil
}

// renamedAnyway reports whether path already names next, the file a rename
// that reported an error was moving there. A network filesystem can apply a
// rename and lose its reply, and appending to the old file then would write
// to a file with no name.
func renamedAnyway(next *os.File, path string) bool {
	want, err := next.Stat()
	if err != nil {
		return false
	}
	got, err := os.Stat(path)
	return err == nil && os.SameFile(want, got)
}

// rewriteStatusInfo writes the fields Redis's INFO persistence reports about
// rewrites beyond the ones Keel already had. aof_rewrites keeps Keel's meaning,
// rewrites finished since the log was opened, where Redis counts the ones
// started.
func rewriteStatusInfo(b *strings.Builder) {
	scheduled := 0
	if rewriteOutcome.scheduled {
		scheduled = 1
	}
	current := int64(-1)
	if rewrite.active {
		current = time.Now().Unix() - rewrite.started.Unix()
	}
	status := "ok"
	if rewriteOutcome.lastErr != nil {
		status = "err"
	}
	fmt.Fprintf(b, "aof_rewrite_scheduled:%d\r\naof_pending_rewrite:%d\r\naof_last_rewrite_time_sec:%d\r\n", scheduled, scheduled, rewriteOutcome.lastSeconds)
	fmt.Fprintf(b, "aof_current_rewrite_time_sec:%d\r\naof_last_bgrewrite_status:%s\r\naof_rewrites_consecutive_failures:%d\r\n", current, status, rewriteOutcome.failures)
}
