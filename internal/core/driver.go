package core

import (
	"errors"
	"time"
)

// The maintenance goroutine: the driver of an engine Open makes, as the
// event loop is the server's engine's (plan phase 3).
//
// It does the work the loop does every cycle, in the loop's order and under
// the engine's lock: the expiry cycle, then the flush - FlushAOF, which writes
// what is buffered, syncs it as the fsync policy says, and advances a rewrite
// by a slice - and memory maintenance, once a second. It runs a cycle every
// maintenanceInterval; whenever it is poked, which a disk worker or a
// rewrite's I/O does when it finishes, since it installs its wake as the
// engine's rewrite waker; and at once again while a rewrite has slices left,
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
// tell that it has stopped.
type driver struct {
	// poke wakes the goroutine for a cycle. It holds one token, so pokes
	// that arrive while a cycle is pending or running fold into one more.
	poke chan struct{}
	// stop is closed by Close, and done by the goroutine as it returns.
	stop, done chan struct{}
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
	d := &driver{poke: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
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
	// A failed flush is the log's failure, which stays the log's (aof.failed)
	// and which every later flush, and Close, returns.
	_ = e.FlushAOF()
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
