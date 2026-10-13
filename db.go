package keel

import (
	"context"
	"errors"
	"sync"

	"github.com/brandopakel/keel/internal/core"
)

// DB is an open instance. Its methods are safe for concurrent use.
type DB struct {
	e *core.Engine
	// A DB is used through its pointer, and holds state that cannot be
	// compared; this keeps it from being comparable from the first release,
	// so that adding such state is never a break.
	_ [0]func()

	// flights are GetOrSet's loads in progress, by key.
	mu      sync.Mutex
	flights map[string]*flight
}

// Options configure an instance. The zero value is an instance in memory
// alone, with no bound on its memory or its keys.
type Options struct {
	// MaxMemory bounds the bytes the instance holds, as Redis's maxmemory
	// does; zero is no bound. Past it, Eviction chooses the keys that go.
	MaxMemory uint64
	// MaxKeys bounds the number of keys; zero is no bound, as in Redis.
	MaxKeys int
	// Eviction is how a full instance chooses what to evict; the zero value
	// is EvictLRU.
	Eviction Eviction
	// Persistence keeps the instance's data in an append-only log that Open
	// replays; nil keeps it in memory alone.
	Persistence *Persistence
}

// Eviction is how a full instance chooses the keys it evicts, as Redis's
// maxmemory-policy does, over every key.
type Eviction uint8

const (
	// EvictLRU evicts the keys least recently used: allkeys-lru.
	EvictLRU Eviction = iota
	// EvictLFU evicts the keys least frequently used: allkeys-lfu.
	EvictLFU
	// EvictRandom evicts keys at random: allkeys-random.
	EvictRandom
)

// Persistence is an append-only log, Redis's AOF, of every write the
// instance makes, which Open replays.
type Persistence struct {
	// Path is the log. While the instance is open it holds a lock on a file
	// beside it, Path + ".lock", so that no other instance opens the log.
	Path string
	// Fsync is how often the log is synced to disk; the zero value is
	// FsyncEverySec.
	Fsync Fsync
}

// Fsync is how often the log is synced to disk, as Redis's appendfsync.
type Fsync uint8

const (
	// FsyncEverySec syncs at most once a second, Redis's default: a crash of
	// the machine loses up to a second of writes, a crash of the process none
	// a call has returned from.
	FsyncEverySec Fsync = iota
	// FsyncAlways syncs before a call that wrote returns.
	FsyncAlways
	// FsyncNo leaves syncing to the operating system.
	FsyncNo
)

// errBadOption refuses an option value outside its enumeration.
var errBadOption = errors.New("keel: an option is outside its values")

// coreOptions is o as the engine takes it.
func (o Options) coreOptions() (core.Options, error) {
	c := core.Options{MaxMemory: o.MaxMemory, MaxKeys: o.MaxKeys}
	switch o.Eviction {
	case EvictLRU:
		c.Eviction = core.EvictLRU
	case EvictLFU:
		c.Eviction = core.EvictLFU
	case EvictRandom:
		c.Eviction = core.EvictRandom
	default:
		return c, errBadOption
	}
	if p := o.Persistence; p != nil {
		c.AppendOnly, c.AppendFilename = true, p.Path
		switch p.Fsync {
		case FsyncEverySec:
			c.Fsync = core.FsyncEverySec
		case FsyncAlways:
			c.Fsync = core.FsyncAlways
		case FsyncNo:
			c.Fsync = core.FsyncNever
		default:
			return c, errBadOption
		}
	}
	return c, nil
}

// Open opens an instance held to o. With Persistence, it replays the log at
// its path - a log a crash cut off mid-record is repaired to the last whole
// one - and opens it for appending. ctx bounds the replay: cancelled, Open
// stops and returns an error wrapping ctx's, and leaves the log as it was.
// A log another instance has open is ErrLocked.
func Open(ctx context.Context, o Options) (*DB, error) {
	c, err := o.coreOptions()
	if err != nil {
		return nil, err
	}
	e, err := core.Open(ctx, c)
	if err != nil {
		return nil, wrap(err)
	}
	return &DB{e: e}, nil
}

// Close closes the instance: it writes and syncs whatever its log has
// buffered, whatever the fsync policy, so that once it returns nil every
// write the instance made is on disk, and releases the log's lock. Every
// later call, and a second Close, returns ErrClosed.
//
// Close takes no context: what it waits for is the final sync, which a
// cancelled Close would have to leave undone.
func (db *DB) Close() error {
	return wrap(db.e.Close())
}
