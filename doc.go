// Package keel is an embedded key-value store with Redis's data types and
// semantics: the engine behind the keel server, opened in-process.
//
// A DB is one instance: its keyspace, its memory bound, and optionally an
// append-only log that a restart replays. Methods take a context and Go
// types, are named after the Redis commands they run, and answer as those
// commands do: SET's options, INCR's overflow refusal, EXPIRE's conditions,
// TTL's answers. Each runs the command's typed operation in the scope the
// server runs the command in, so what a method changes is logged as the
// command would be, and a log written here replays on the server, and the
// other way around.
//
//	db, err := keel.Open(ctx, keel.Options{
//		MaxMemory:   256 << 20,
//		Persistence: &keel.Persistence{Path: "cache.aof", Fsync: keel.FsyncEverySec},
//	})
//	if err != nil { ... }
//	defer db.Close()
//
//	err = db.Set(ctx, "greeting", "hello", keel.WithTTL(time.Minute))
//	v, err := db.Get(ctx, "greeting") // keel.ErrNotFound when missing
//
// # Concurrency and durability
//
// A DB is safe for concurrent use: each call holds the instance's lock while
// it runs, as a command holds the server's event loop. A call that changed
// something, or could have read something not yet written to the log,
// returns once that is as durable as the fsync policy makes a server's
// reply: written to the operating system under FsyncEverySec and FsyncNo,
// and synced under FsyncAlways. The log is the instance's alone while it is
// open: a second Open of the same path fails with ErrLocked.
//
// # Transactions and scans
//
// Atomic runs commands as one transaction, as Redis's MULTI and EXEC do: a
// function queues them on a Tx, and they run together, with nothing between
// them, logged as one block; a command that fails does not undo the others.
// Scan walks the keys as SCAN does, as an iterator that never holds the lock
// while the loop's body runs.
//
// # Errors
//
// An error keeps the text Redis answers with - "WRONGTYPE Operation against
// a key holding the wrong kind of value", "ERR value is not an integer or
// out of range" - and errors.Is matches it against its category: ErrWrongType,
// ErrNotInteger, ErrOverflow, ErrOutOfMemory, ErrReadOnly, ErrPersistence,
// ErrClosed and ErrLocked. A key that holds nothing is ErrNotFound where
// Redis would answer nil.
//
// # Stability
//
// The package is experimental until v1: it arrives in v0.2.0-alpha.1, and a
// minor release may change it, with migration notes. Do is experimental
// within that. The log's format is the stronger promise: every v0 release
// reads every earlier v0 log.
package keel
