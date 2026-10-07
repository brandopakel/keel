package core

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// An engine's lifecycle (plan phase 3): Open starts one from its options and
// its log, and Close ends it.
//
// Open is NewEngine, then the log's startup (StartAOF), then replication's
// start (InitReplication): what the server's startup has always run, in the
// order it runs it. It then starts the engine's driver, its maintenance
// goroutine (driver.go). cmd/keel runs the same three itself, because its loop
// drives the engine and its startup logs a warning between the first two;
// see "Phase 3: the instance contract" in docs/embedding-plan.md.

// ErrClosed is what an engine that has been closed answers: a second Close,
// and, once there are calls for embedded callers, every call after Close.
var ErrClosed = errors.New("instance is closed")

// Open returns an engine held to o, with its log replayed and open if
// o.AppendOnly, its replication started, and its maintenance goroutine
// running: from then on whoever touches the engine holds its lock (Lock), as
// the goroutine does for each cycle. Before it reads the log, it takes the
// lock beside it (loglock.go), which keeps a second instance off the log until
// Close; the server, which starts its log without Open, takes none. Open
// refuses the options only the server's loop can drive: AsyncAppend,
// ReplicaOf and ReplicationFeed.
//
// ctx bounds the startup alone, and the engine does not keep it. Cancelled
// while the log is replayed, Open stops, returns an error that wraps ctx's,
// and leaves every file as it found it: a replay only reads, and nothing is
// written until it has finished. Once it has, startup runs to completion.
//
// An error of the log's startup is returned as the server's startup returns
// it, after "appendonly: ". Whatever failed, nothing Open started is left
// running or open.
func Open(ctx context.Context, o Options) (*Engine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case o.AsyncAppend:
		return nil, errOpenAsyncAppend
	case o.ReplicaOf != "" || o.ReplicationFeed:
		return nil, errOpenReplication
	}
	e, err := NewEngine(o)
	if err != nil {
		return nil, err
	}
	if err := e.lockLog(); err != nil {
		return nil, fmt.Errorf("appendonly: %w", err)
	}
	if err := e.StartAOF(ctx, ""); err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("appendonly: %w", err)
	}
	if err := e.InitReplication(); err != nil {
		_ = e.Close()
		return nil, err
	}
	e.startDriver()
	return e, nil
}

// Close ends e. It marks e closed, so every later call returns ErrClosed,
// stops e's maintenance goroutine and waits for it, then closes e's log,
// writing and syncing whatever is buffered whatever the fsync policy, so that
// when it returns nil every write e has run is on disk. It then wakes the
// calls waiting for that, and last releases the lock beside the log. If the
// final flush fails, Close still closes the log and releases its lock, and
// returns the failure: for an engine Open made, as ErrPersistence. Closing a
// closed engine returns ErrClosed, as closing a closed file does.
//
// It takes e's lock, so whatever else drives e has to have stopped, or let go
// of it: the server calls it once its loop has returned. Close does not stop a
// caller that goes on running EvalAndResponse itself, which is the primitive a
// driver runs under e's lock and checks nothing on a command's path: a driver
// stops before it closes, as the server's loop does, and the calls embedded
// callers make (plan phase 3, part 5) refuse with ErrClosed.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	e.closed = true
	e.mu.Unlock()
	// Without the lock, which the goroutine may be waiting for to run its
	// last cycle; it never runs another once it has returned.
	e.stopDriver()
	e.mu.Lock()
	err := e.CloseAOF()
	// The final flush covered every waiting call, or failed them all.
	if d := e.driver; d != nil {
		if err != nil {
			if d.failed == nil {
				d.failed = err
			}
			err = fmt.Errorf("%w: %w", ErrPersistence, err)
		}
		d.publish()
	}
	// Last, so that no other instance can open the log while this one may
	// still write to it.
	if e.logLock != nil {
		if releaseErr := e.logLock.release(); err == nil {
			err = releaseErr
		}
		e.logLock = nil
	}
	e.mu.Unlock()
	return err
}

// StartAOF runs the log's startup on e, as e's options say: nothing unless
// AppendOnly, and otherwise the log at AppendFilename replayed into e and
// opened for appending. It is what the server's startup ran as
// server.StartAOF, moved here so that Open runs the same sequence. It takes no
// lock beside the log: Open does, before it, and the server, which runs it
// without Open, takes none, as Redis takes none. What it has started when it
// fails, Close ends.
//
// Loading first and opening second is deliberate: opening installs the hook
// that records evictions, and replaying a log with that hook live would append
// what it is currently reading. A truncated final command is the ordinary shape
// of a crash, so it is logged and the engine starts anyway on the commands
// before it; anything else is a refusal to start, because beginning from a
// partially understood log means quietly serving a keyspace that is missing
// whatever came after the part that failed.
//
// legacy names a log written under an earlier name, which the server passes
// and Open does not: when there is no log at AppendFilename but there is one
// at legacy, that one is replayed, and the keyspace it holds is then written
// into the new log (see aofReadPath).
//
// ctx is looked at while the log is replayed, and once more when the replay
// has finished, before anything is written: a torn tail repaired, or a log
// created or opened. From there, startup runs to completion: it writes, and
// nothing after the replay takes long. A legacy log's migration publishes the
// new log only once it holds the whole keyspace (migrateAOF), so even a crash
// part of the way through it loses nothing.
//
// It logs what the server's startup has always logged, in the same words and
// order.
func (e *Engine) StartAOF(ctx context.Context, legacy string) error {
	options := e.Configuration().WithDefaults()
	if !options.AppendOnly {
		return nil
	}
	path := options.AppendFilename
	// Before the log is read, because a node that cannot establish which term it
	// is in must not reach the point of serving anything at that term.
	if err := e.LoadTerm(path); err != nil {
		return err
	}

	readFrom := aofReadPath(path, legacy)
	if readFrom != path {
		aofLog("reading %s, written before the rename; "+
			"new records go to %s", readFrom, path)
	}

	applied, err := e.loadAOF(ctx, readFrom)
	if err == nil || IsTruncatedAOF(err) {
		// The replay has finished. Stopping here is still harmless, and is the
		// last place it is: from the repair on, startup writes.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("startup stopped after replaying %s: %w", readFrom, ctxErr)
		}
	}
	switch {
	case IsTruncatedAOF(err):
		if repairErr := RepairAOFTail(err); repairErr != nil {
			return repairErr
		}
		aofLog("%v - starting from what was intact", err)
	case err != nil:
		return err
	case applied > 0:
		aofLog("replayed %d commands from %s", applied, readFrom)
	}
	if readFrom == path {
		if err := e.OpenAOF(path); err != nil {
			return err
		}
	} else if err := e.migrateAOF(path); err != nil {
		return fmt.Errorf("migrating %s to %s: %w", readFrom, path, err)
	} else {
		aofLog("wrote the replayed keyspace to %s; %s is no longer read",
			path, readFrom)
	}

	aofLog("on, %s, appendfsync %s", path, options.Fsync)
	return nil
}

// migratingSuffix names the log a legacy log's keyspace is written into before
// it has the name it is kept under.
const migratingSuffix = ".migrating"

// migrateAOF writes the keyspace e replayed from a legacy log into a new log
// at path, and opens it for appending, before anything else appends to it.
//
// Without this the fallback loses the data it exists to save, one restart
// later rather than immediately. The first start reads memkv-master.aof and
// opens an empty keel-master.aof; the second start sees keel-master.aof
// present, prefers it, and replays only what was written after the
// migration. Everything that lived solely in the old log is gone, and the old
// log is still sitting there looking like a backup.
//
// For the same reason, the new log has its name only once it holds the whole
// keyspace. It is written under path + migratingSuffix, synced, and renamed
// onto path, the directory synced after, as Redis publishes a rewritten log,
// so that anything that stops it before the rename leaves no log at path, and
// the next start replays the legacy log again. A log left under the temporary
// name by such a stop is removed first.
//
// A rewrite is exactly the right thing here: the shortest log producing the
// current state. It runs to completion before the server serves anyone, which
// at startup costs one pass over a keyspace that was just built by one pass
// over the same data.
func (e *Engine) migrateAOF(path string) error {
	migrating := path + migratingSuffix
	if err := os.Remove(migrating); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := e.OpenAOF(migrating); err != nil {
		return err
	}
	if err := e.RewriteAOF(); err != nil {
		return err
	}
	return e.RenameAOF(path)
}

// aofReadPath chooses which log to replay at startup.
//
// The server's default log was ./memkv-master.aof before the server was
// renamed and is ./keel-master.aof now. Without this, the first restart after
// the rename finds nothing at the new name, replays nothing, and serves an
// empty keyspace beside a perfectly good log it never opened - no error and
// no warning, which is the worst shape a data loss can take.
//
// The old name is a fallback and not a merge: if both files are there, the
// current one is the live log and the old one is whatever was left behind. Only
// the current name is ever written. With no legacy name, the current one is
// read.
func aofReadPath(current, legacy string) string {
	if legacy == "" {
		return current
	}
	if _, err := os.Stat(current); err == nil {
		return current
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return current
}
