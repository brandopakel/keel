package keel

import (
	"context"
	"errors"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// Atomic runs the commands fn queues on tx as one transaction, as Redis's
// MULTI and EXEC run theirs. fn queues them; once it returns nil, they run in
// the order queued, under one hold of the instance's lock, so that no other
// call runs between them; and what they write is logged as one MULTI ...
// EXEC block, which a replay applies whole or, cut off by a crash, not at
// all. Each queued method answers with a Result, which holds the command's
// answer once Atomic has returned.
//
// As in Redis, a command that fails fails alone: the commands after it still
// run, and those before it are not undone, for there is no rollback. Atomic
// returns nil once the transaction has run, whatever its commands answered:
// read each Result for that.
//
// The transaction runs whole or not at all. If fn returns an error, nothing
// runs and Atomic returns that error. Nothing runs either, and Atomic returns
// why, if ctx is done first, the DB is closed, its log has failed (while a
// failed write or sync is retried, a transaction that writes is refused with
// ErrPersistence, as Redis refuses EXEC), or a replica refuses any of the
// commands. A Result of a command that did not run holds that error.
//
// Once the transaction has run, Atomic waits for what it wrote to be as
// durable as one call's, as every method does: an error then, ctx's or
// ErrPersistence, comes after the commands ran.
//
// fn runs before the lock is taken, so it may take its time, but it cannot
// read what the transaction will write: a Result is empty until Atomic
// returns. A Tx is not safe for concurrent use, and is spent once Atomic
// returns: a command queued on it after that does not run.
//
// The transaction counts as the commands a client sends for it, its MULTI
// and EXEC with the rest, as the server counts them.
func (db *DB) Atomic(ctx context.Context, fn func(tx *Tx) error) error {
	tx := &Tx{db: db}
	defer func() { tx.spent = true }() // should fn panic
	err := fn(tx)
	tx.spent = true
	if err == nil {
		err = wrap(db.e.Exec(ctx, tx.calls))
		if tx.ran() {
			return err
		}
	}
	for _, refuse := range tx.refusals {
		refuse(err)
	}
	return err
}

// Tx is a transaction Atomic is building: the commands queued on it, which
// run together once the function given to Atomic returns. Its methods are
// the DB's, without a context, each answering with a Result.
type Tx struct {
	db       *DB
	calls    []core.TxCall
	refusals []func(error)
	answered int
	spent    bool
}

// errNotRun is a Result's error before its transaction has run, and
// errSpent that of a command queued on a Tx whose Atomic has returned.
var (
	errNotRun = errors.New("keel: the transaction has not run yet")
	errSpent  = errors.New("keel: command queued on a Tx after its Atomic returned")
)

// ran reports whether tx's transaction ran: whether its commands were
// answered, every one, as Exec runs every one or none.
func (tx *Tx) ran() bool { return len(tx.calls) > 0 && tx.answered == len(tx.calls) }

// Result is the answer of a command queued on a Tx, which Atomic gives it.
// Until then it holds an error saying so, and for a command that did not run,
// the reason it did not.
type Result[T any] struct {
	value T
	err   error
	// Not comparable, so that what it holds may change without a break.
	_ [0]func()
}

// Value is the command's answer and its error, as the DB's method of the
// same name returns them.
func (r *Result[T]) Value() (T, error) { return r.value, r.err }

// Err is the command's error, nil if it succeeded.
func (r *Result[T]) Err() error { return r.err }

// queue adds the command name to tx, run by the DB's method that stands for
// it, whose answer is the Result's.
func queue[T any](tx *Tx, name string, method func(context.Context) (T, error)) *Result[T] {
	r := &Result[T]{err: errNotRun}
	if tx.spent {
		r.err = errSpent
		return r
	}
	tx.calls = append(tx.calls, core.TxCall{Name: name, Run: func(ctx context.Context) {
		r.value, r.err = method(ctx)
		tx.answered++
	}})
	tx.refusals = append(tx.refusals, func(err error) {
		var zero T
		r.value, r.err = zero, err
	})
	return r
}

// status is the answer of a method whose answer is its error alone.
func status(err error) (struct{}, error) { return struct{}{}, err }

// Get queues GET: see DB.Get.
func (tx *Tx) Get(key string) *Result[string] {
	return queue(tx, "GET", func(ctx context.Context) (string, error) { return tx.db.Get(ctx, key) })
}

// GetBytes queues GET: see DB.GetBytes.
func (tx *Tx) GetBytes(key string) *Result[[]byte] {
	return queue(tx, "GET", func(ctx context.Context) ([]byte, error) { return tx.db.GetBytes(ctx, key) })
}

// Set queues SET: see DB.Set.
func (tx *Tx) Set(key, value string, opts ...SetOption) *Result[struct{}] {
	return queue(tx, "SET", func(ctx context.Context) (struct{}, error) {
		return status(tx.db.Set(ctx, key, value, opts...))
	})
}

// SetBytes queues SET: see DB.SetBytes. The bytes are copied as it queues.
func (tx *Tx) SetBytes(key string, value []byte, opts ...SetOption) *Result[struct{}] {
	return tx.Set(key, string(value), opts...)
}

// SetNX queues SET with NX: see DB.SetNX.
func (tx *Tx) SetNX(key, value string, opts ...SetOption) *Result[bool] {
	return queue(tx, "SET", func(ctx context.Context) (bool, error) { return tx.db.SetNX(ctx, key, value, opts...) })
}

// SetXX queues SET with XX: see DB.SetXX.
func (tx *Tx) SetXX(key, value string, opts ...SetOption) *Result[bool] {
	return queue(tx, "SET", func(ctx context.Context) (bool, error) { return tx.db.SetXX(ctx, key, value, opts...) })
}

// GetDel queues GETDEL: see DB.GetDel.
func (tx *Tx) GetDel(key string) *Result[string] {
	return queue(tx, "GETDEL", func(ctx context.Context) (string, error) { return tx.db.GetDel(ctx, key) })
}

// Incr queues INCR: see DB.Incr.
func (tx *Tx) Incr(key string) *Result[int64] {
	return queue(tx, "INCR", func(ctx context.Context) (int64, error) { return tx.db.Incr(ctx, key) })
}

// Decr queues DECR: see DB.Decr.
func (tx *Tx) Decr(key string) *Result[int64] {
	return queue(tx, "DECR", func(ctx context.Context) (int64, error) { return tx.db.Decr(ctx, key) })
}

// IncrBy queues INCRBY: see DB.IncrBy.
func (tx *Tx) IncrBy(key string, by int64) *Result[int64] {
	return queue(tx, "INCRBY", func(ctx context.Context) (int64, error) { return tx.db.IncrBy(ctx, key, by) })
}

// DecrBy queues DECRBY: see DB.DecrBy.
func (tx *Tx) DecrBy(key string, by int64) *Result[int64] {
	return queue(tx, "DECRBY", func(ctx context.Context) (int64, error) { return tx.db.DecrBy(ctx, key, by) })
}

// IncrByFloat queues INCRBYFLOAT: see DB.IncrByFloat.
func (tx *Tx) IncrByFloat(key string, by float64) *Result[float64] {
	return queue(tx, "INCRBYFLOAT", func(ctx context.Context) (float64, error) { return tx.db.IncrByFloat(ctx, key, by) })
}

// Append queues APPEND: see DB.Append.
func (tx *Tx) Append(key, value string) *Result[int64] {
	return queue(tx, "APPEND", func(ctx context.Context) (int64, error) { return tx.db.Append(ctx, key, value) })
}

// StrLen queues STRLEN: see DB.StrLen.
func (tx *Tx) StrLen(key string) *Result[int64] {
	return queue(tx, "STRLEN", func(ctx context.Context) (int64, error) { return tx.db.StrLen(ctx, key) })
}

// GetRange queues GETRANGE: see DB.GetRange.
func (tx *Tx) GetRange(key string, start, end int64) *Result[string] {
	return queue(tx, "GETRANGE", func(ctx context.Context) (string, error) { return tx.db.GetRange(ctx, key, start, end) })
}

// SetRange queues SETRANGE: see DB.SetRange.
func (tx *Tx) SetRange(key string, offset int64, value string) *Result[int64] {
	return queue(tx, "SETRANGE", func(ctx context.Context) (int64, error) { return tx.db.SetRange(ctx, key, offset, value) })
}

// MGet queues MGET: see DB.MGet.
func (tx *Tx) MGet(keys ...string) *Result[map[string]string] {
	return queue(tx, "MGET", func(ctx context.Context) (map[string]string, error) { return tx.db.MGet(ctx, keys...) })
}

// MSet queues MSET: see DB.MSet.
func (tx *Tx) MSet(values map[string]string) *Result[struct{}] {
	return queue(tx, "MSET", func(ctx context.Context) (struct{}, error) { return status(tx.db.MSet(ctx, values)) })
}

// Del queues DEL: see DB.Del.
func (tx *Tx) Del(keys ...string) *Result[int64] {
	return queue(tx, "DEL", func(ctx context.Context) (int64, error) { return tx.db.Del(ctx, keys...) })
}

// Exists queues EXISTS: see DB.Exists.
func (tx *Tx) Exists(keys ...string) *Result[int64] {
	return queue(tx, "EXISTS", func(ctx context.Context) (int64, error) { return tx.db.Exists(ctx, keys...) })
}

// Type queues TYPE: see DB.Type.
func (tx *Tx) Type(key string) *Result[string] {
	return queue(tx, "TYPE", func(ctx context.Context) (string, error) { return tx.db.Type(ctx, key) })
}

// Rename queues RENAME: see DB.Rename.
func (tx *Tx) Rename(from, to string) *Result[struct{}] {
	return queue(tx, "RENAME", func(ctx context.Context) (struct{}, error) { return status(tx.db.Rename(ctx, from, to)) })
}

// RenameNX queues RENAMENX: see DB.RenameNX.
func (tx *Tx) RenameNX(from, to string) *Result[bool] {
	return queue(tx, "RENAMENX", func(ctx context.Context) (bool, error) { return tx.db.RenameNX(ctx, from, to) })
}

// DBSize queues DBSIZE: see DB.DBSize.
func (tx *Tx) DBSize() *Result[int64] {
	return queue(tx, "DBSIZE", func(ctx context.Context) (int64, error) { return tx.db.DBSize(ctx) })
}

// FlushDB queues FLUSHDB: see DB.FlushDB.
func (tx *Tx) FlushDB() *Result[struct{}] {
	return queue(tx, "FLUSHDB", func(ctx context.Context) (struct{}, error) { return status(tx.db.FlushDB(ctx)) })
}

// Expire queues PEXPIRE, its ttl reckoned from when the transaction runs:
// see DB.Expire.
func (tx *Tx) Expire(key string, ttl time.Duration, opts ...ExpireOption) *Result[bool] {
	return queue(tx, "PEXPIRE", func(ctx context.Context) (bool, error) { return tx.db.Expire(ctx, key, ttl, opts...) })
}

// ExpireAt queues PEXPIREAT: see DB.ExpireAt.
func (tx *Tx) ExpireAt(key string, t time.Time, opts ...ExpireOption) *Result[bool] {
	return queue(tx, "PEXPIREAT", func(ctx context.Context) (bool, error) { return tx.db.ExpireAt(ctx, key, t, opts...) })
}

// Persist queues PERSIST: see DB.Persist.
func (tx *Tx) Persist(key string) *Result[bool] {
	return queue(tx, "PERSIST", func(ctx context.Context) (bool, error) { return tx.db.Persist(ctx, key) })
}

// TTL queues PTTL: see DB.TTL.
func (tx *Tx) TTL(key string) *Result[time.Duration] {
	return queue(tx, "PTTL", func(ctx context.Context) (time.Duration, error) { return tx.db.TTL(ctx, key) })
}

// ExpireTime queues PEXPIRETIME: see DB.ExpireTime.
func (tx *Tx) ExpireTime(key string) *Result[time.Time] {
	return queue(tx, "PEXPIRETIME", func(ctx context.Context) (time.Time, error) { return tx.db.ExpireTime(ctx, key) })
}

// CompareAndSwap queues SET IFEQ: see DB.CompareAndSwap.
func (tx *Tx) CompareAndSwap(key, old, value string, opts ...SetOption) *Result[bool] {
	return queue(tx, "SET", func(ctx context.Context) (bool, error) {
		return tx.db.CompareAndSwap(ctx, key, old, value, opts...)
	})
}

// DeleteIf queues DELEX IFEQ: see DB.DeleteIf.
func (tx *Tx) DeleteIf(key, value string) *Result[bool] {
	return queue(tx, "DELEX", func(ctx context.Context) (bool, error) { return tx.db.DeleteIf(ctx, key, value) })
}
