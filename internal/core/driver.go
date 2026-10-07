package core

import (
	"errors"
	"sync/atomic"
	"time"
)

// The maintenance goroutine: the driver of an engine Open makes, as the
// event loop is the server's engine's (plan phase 3).
//
// It does the work the loop does every cycle, in the loop's order and under
// the engine's lock: the expiry cycle, then the flush - FlushAOF, which writes
// what is buffered, syncs it as the fsync policy says, and advances a rewrite
// by a slice - and memory maintenance, once a second. A flush publishes what it
// wrote to the calls waiting for it, and a failed one latches (calls.go). It
// runs a cycle every maintenanceInterval; whenever it is poked, which a
// waiting call does, and a disk worker or a rewrite's I/O when it finishes,
// since it installs its wake as the engine's rewrite waker; and at once again
// while a rewrite has slices left,
// as the loop wakes itself. It takes the lock afresh for every cycle, so
// whoever else holds the lock runs between cycles, and between a rewrite's
// slices. Close stops it, and waits for it, before it closes the log.

// maintenanceInterval is how often the maintenance goroutine runs a cycle
// with nothing to prompt it: the server's default -cron-interval-ms, and
// Redis's default hz of 10, for the same reason - an engine nobody is calling
// still has keys to expire and a log to sync.
const maintenanceInterval = 100 * time.Millisecond

// memoryMaintenanceInterval is how often a cycle compacts the stores, as the
// loop does.
const memoryMaintenanceInterval = time.Second

// driver is the maintenance goroutine's state: how to poke it, stop it, and
// tell that it has stopped, and what the calls waiting on its flushes share
// with it (calls.go).
type driver struct {
	// poke wakes the goroutine for a cycle. It holds one token, so pokes
	// that arrive while a cycle is pending or running fold into one more.
	poke chan struct{}
	// stop is closed by Close, and done by the goroutine as it returns.
	stop, done chan struct{}

	// waiters is how many calls wait for a flush. A call whose context is
	// done stops waiting without the engine's lock, which a flush may hold
	// for as long as the disk takes, so it is counted atomically.
	waiters atomic.Int64

	// The rest is the engine's, read and written under its lock.
	//
	// published is closed, and replaced, once a cycle has flushed while
	// calls wait for it, and once Close has.
	published chan struct{}
	// failed is the log's failure that latches: a failed write or sync
	// under always, where Redis exits, or any failure of a log that does not
	// retry. Every later call returns it, as ErrPersistence.
	failed error

	// retries says a failed write or sync of the log is retried, as Redis
	// retries it under everysec and no: set when the engine's policy is not
	// always. writeFailure and syncFailure are then the log's write and sync
	// statuses while they are in error - Redis's aof_last_write_status and
	// aof_bio_fsync_status - nil when they are not; while either is, write
	// commands are refused (calls.go). writeLogged is when a failed write was
	// last logged, at most once in writeLogEvery, as Redis logs it.
	retries                   bool
	writeFailure, syncFailure error
	writeLogged               time.Time
}

// writeLogEvery is how often a write of the log that keeps failing is
// logged: Redis's AOF_WRITE_LOG_ERROR_RATE.
const writeLogEvery = 30 * time.Second

// writeFailed puts the log's write status in error, as Redis's
// flushAppendOnlyFile does when a write under everysec or no fails. The caller
// holds the engine's lock.
func (d *driver) writeFailed(err error) {
	d.writeFailure = err
	if now := time.Now(); now.Sub(d.writeLogged) > writeLogEvery {
		d.writeLogged = now
		aofLog("error writing to the log: %v", err)
	}
}

// writeSolved clears the log's write status once a write succeeds, as Redis
// clears aof_last_write_status. The caller holds the engine's lock.
func (d *driver) writeSolved() {
	d.writeFailure = nil
	aofLog("write error looks solved; writes are accepted again")
}

// syncFailed puts the log's sync status in error, as Redis's background fsync
// job does, logging it when it was not already. The caller holds the engine's
// lock.
func (d *driver) syncFailed(err error) {
	if d.syncFailure == nil {
		aofLog("failed to sync the log: %v", err)
	}
	d.syncFailure = err
}

// publish wakes every call waiting on d's flushes, to look again at what has
// been published. The caller holds the engine's lock.
func (d *driver) publish() {
	close(d.published)
	d.published = make(chan struct{})
}

// wake pokes d without waiting. A worker calls it, from its own goroutine,
// when it finishes.
func (d *driver) wake() {
	select {
	case d.poke <- struct{}{}:
	default:
	}
}

// Open drives neither of these. AsyncAppend's batch is held back by the
// server's loop, which runs no command while one is out; the maintenance
// goroutine already keeps the log's I/O off callers' goroutines. And a
// replication role needs the server's transport: replication stays the
// server's (docs/embedding-plan.md, "API").
var (
	errOpenAsyncAppend = errors.New("AsyncAppend is the server's: an engine Open makes appends from its own maintenance goroutine")
	errOpenReplication = errors.New("replication is the server's: an engine Open makes has no ReplicaOf or ReplicationFeed")
)

// startDriver gives e its maintenance goroutine. e's maker calls it, while it
// is still e's only user.
func (e *Engine) startDriver() {
	d := &driver{poke: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		published: make(chan struct{}), retries: e.settings.fsync != FsyncAlways}
	e.driver = d
	e.SetRewriteWaker(d.wake)
	go e.drive(d)
}

// drive is the maintenance goroutine: a cycle at every tick and every poke,
// until it is stopped.
func (e *Engine) drive(d *driver) {
	defer close(d.done)
	tick := time.NewTicker(maintenanceInterval)
	defer tick.Stop()
	nextMemory := time.Now().Add(memoryMaintenanceInterval)
	for {
		select {
		case <-d.stop:
			return
		case <-tick.C:
		case <-d.poke:
		}
		e.mu.Lock()
		e.maintain(d, &nextMemory)
		e.mu.Unlock()
	}
}

// maintain runs one cycle of the loop's own work on e, whose lock the caller
// holds.
func (e *Engine) maintain(d *driver, nextMemory *time.Time) {
	// Reap idle keys before flushing their removal records, as the loop does.
	e.ExpireCycle()
	// A failed flush is a state, not an error to act on here: one the log
	// retries leaves its write or sync status in error until a later flush
	// succeeds, and one it does not latches (aof.failed), where the server
	// would stop, so that every later call is refused (calls.go).
	_ = e.FlushAOF()
	if e.aof.failed != nil && d.failed == nil {
		d.failed = e.aof.failed
	}
	if d.waiters.Load() > 0 {
		d.publish()
	}
	if now := time.Now(); !now.Before(*nextMemory) {
		*nextMemory = now.Add(memoryMaintenanceInterval)
		e.MaintainMemory()
	}
	// A rewrite advances one slice a cycle, so one with slices left needs the
	// next cycle now, not at the next tick.
	if e.RewriteNeedsCycle() {
		d.wake()
	}
}

// stopDriver stops e's maintenance goroutine, if it has one, and waits for it
// to return. The caller must not hold e's lock, which the goroutine may be
// waiting for.
func (e *Engine) stopDriver() {
	if d := e.driver; d != nil {
		close(d.stop)
		<-d.done
	}
}
