package keel

import (
	"context"
	"iter"

	"github.com/brandopakel/keel/internal/core"
)

// ScanOptions choose the keys Scan yields and the size of its steps, as
// SCAN's options do. The zero value yields every key, in steps of Redis's
// default size.
type ScanOptions struct {
	// Match is a glob a key must match, as SCAN's MATCH: *, ?, [...] and
	// backslash escapes. Empty yields keys of any name.
	Match string
	// Type is the type a key must hold, as SCAN's TYPE: "string", "hash",
	// "list", "set", "zset", and the other names Type answers with. Empty
	// yields keys of any type.
	Type string
	// Count is how many keys each step looks at, as SCAN's COUNT: the work
	// done under the instance's lock at a time, not a number of keys
	// yielded. Zero is Redis's default, 10.
	Count int
}

// Scan walks the instance's keys, yielding each that opts keep, as a client
// walks them with SCAN: one step at a time from cursor 0 until the cursor
// comes back 0, each step a SCAN of its own. The lock is held for one step
// and never while the loop's body runs, so the body may call the DB, change
// the keys it walks, or take its time, and other calls run between steps.
//
// It guarantees what SCAN guarantees: a key that holds something for the
// whole walk is yielded at least once. A key may be yielded more than once,
// and a key added or deleted during the walk may or may not be. Nothing is
// held between steps, so a walk that stops early costs nothing.
//
// An error ends the walk, yielded with an empty key: ctx's, when it is done
// before a step; ErrClosed; or SCAN's refusals, a Count outside 1 to SCAN's
// bound, or a Match too costly for a step's work limit.
func (db *DB) Scan(ctx context.Context, opts ScanOptions) iter.Seq2[string, error] {
	o := core.ScanOptions{Match: opts.Match, Type: opts.Type, Count: opts.Count}
	return func(yield func(string, error) bool) {
		var cursor uint64
		for {
			keys, next, err := db.e.Scan(ctx, cursor, o)
			if err != nil {
				yield("", wrap(err))
				return
			}
			for _, key := range keys {
				if !yield(key, nil) {
					return
				}
			}
			if next == 0 {
				return
			}
			cursor = next
		}
	}
}
