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
// order it runs it. cmd/keel runs the same three itself, because its loop
// drives the engine and its startup logs a warning between the first two;
// see "Phase 3: the instance contract" in docs/embedding-plan.md.

// ErrClosed is what an engine that has been closed answers: a second Close,
// and, once there are calls for embedded callers, every call after Close.
var ErrClosed = errors.New("instance is closed")

// Open returns an engine held to o, with its log replayed and open if
// o.AppendOnly, and its replication started.
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
	e, err := NewEngine(o)
	if err != nil {
		return nil, err
	}
	if err := e.StartAOF(ctx, ""); err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("appendonly: %w", err)
	}
	if err := e.InitReplication(); err != nil {
		_ = e.Close()
		return nil, err
	}
	return e, nil
}

// Close ends e: it closes e's log, writing and syncing whatever is buffered
// whatever the fsync policy, so that when it returns nil every write e has
// run is on disk, and then releases the lock beside the log. Closing a closed
// engine returns ErrClosed, as closing a closed file does.
//
// It takes e's lock, so whatever drives e has to have stopped, or let go of
// it: the server calls it once its loop has returned.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	e.closed = true
	err := e.CloseAOF()
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
// AppendOnly, and otherwise the lock beside the log at AppendFilename taken
// (loglock.go), and the log replayed into e and opened for appending. It is
// what the server's startup ran as server.StartAOF, moved here so that Open
// runs the same sequence. What it has started when it fails, Close ends.
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
// created or opened. From there, startup runs to completion. Stopping
// between opening a new log and writing a legacy log's keyspace into it is
// the one interruption that would lose data: the next start would prefer the
// new, empty log.
//
// It logs what the server's startup has always logged, in the same words and
// order.
func (e *Engine) StartAOF(ctx context.Context, legacy string) error {
	options := e.Configuration().WithDefaults()
	if !options.AppendOnly {
		return nil
	}
	path := options.AppendFilename
	// One instance per log: the lock is taken before anything of the log is
	// read, and held until Close.
	lock, err := lockLog(path)
	switch {
	case err == nil:
		e.logLock = lock
	case errors.Is(err, errLockUnsupported):
		aofLog("%v; nothing keeps a second instance off this log", err)
	default:
		return err
	}
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
	if err := e.OpenAOF(path); err != nil {
		return err
	}

	// Having replayed the old file, write the whole keyspace into the new one
	// before anything else appends to it.
	//
	// Without this the fallback loses the data it exists to save, one restart
	// later rather than immediately. The first start reads memkv-master.aof and
	// opens an empty keel-master.aof; the second start sees keel-master.aof
	// present, prefers it, and replays only what was written after the
	// migration. Everything that lived solely in the old log is gone, and the
	// old log is still sitting there looking like a backup.
	//
	// A rewrite is exactly the right thing here: the shortest log producing the
	// current state. It runs to completion before the server serves anyone,
	// which at startup costs one pass over a keyspace that was just built by
	// one pass over the same data.
	if readFrom != path {
		if err := e.RewriteAOF(); err != nil {
			return fmt.Errorf("migrating %s to %s: %w", readFrom, path, err)
		}
		aofLog("wrote the replayed keyspace to %s; %s is no longer read",
			path, readFrom)
	}

	aofLog("on, %s, appendfsync %s", path, options.Fsync)
	return nil
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
