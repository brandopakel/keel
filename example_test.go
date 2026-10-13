package keel_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/brandopakel/keel"
)

func Example() {
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Options{MaxMemory: 64 << 20})
	if err != nil {
		panic(err)
	}
	defer db.Close()

	if err := db.Set(ctx, "greeting", "hello", keel.WithTTL(time.Minute)); err != nil {
		panic(err)
	}
	v, err := db.Get(ctx, "greeting")
	fmt.Println(v, err)

	_, err = db.Get(ctx, "nobody")
	fmt.Println(errors.Is(err, keel.ErrNotFound))
	// Output:
	// hello <nil>
	// true
}

func ExampleOpen_persistence() {
	ctx := context.Background()
	path := filepath.Join(exampleDir(), "cache.aof")
	db, err := keel.Open(ctx, keel.Options{
		Persistence: &keel.Persistence{Path: path, Fsync: keel.FsyncAlways},
	})
	if err != nil {
		panic(err)
	}
	db.Set(ctx, "k", "v")
	db.Close()

	// The log replays: what was written is there again.
	db, err = keel.Open(ctx, keel.Options{Persistence: &keel.Persistence{Path: path}})
	if err != nil {
		panic(err)
	}
	defer db.Close()
	fmt.Println(db.Get(ctx, "k"))
	// Output: v <nil>
}

func ExampleDB_Incr() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	db.Incr(ctx, "hits")
	n, _ := db.IncrBy(ctx, "hits", 10)
	fmt.Println(n)

	db.Set(ctx, "name", "ada")
	_, err := db.Incr(ctx, "name")
	fmt.Println(errors.Is(err, keel.ErrNotInteger), err)
	// Output:
	// 11
	// true ERR value is not an integer or out of range
}

func ExampleDB_TTL() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	db.Set(ctx, "session", "token")
	ttl, _ := db.TTL(ctx, "session")
	fmt.Println(ttl == keel.NoTTL)

	db.Expire(ctx, "session", time.Hour)
	ttl, _ = db.TTL(ctx, "session")
	fmt.Println(ttl > 59*time.Minute)

	set, _ := db.Expire(ctx, "session", time.Minute, keel.ExpireGT)
	fmt.Println(set)
	// Output:
	// true
	// true
	// false
}

func ExampleDB_Do() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	db.Do(ctx, "HSET", "user:1", "name", "Ada")
	name, _ := db.Do(ctx, "HGET", "user:1", "name")
	fmt.Println(name)

	_, err := db.Get(ctx, "user:1")
	fmt.Println(errors.Is(err, keel.ErrWrongType))
	// Output:
	// Ada
	// true
}

func ExampleDB_GetOrSet() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	load := func(ctx context.Context) (string, error) {
		// A database query, an HTTP call: whatever the cache stands in front of.
		return "Ada Lovelace", nil
	}
	name, _ := db.GetOrSet(ctx, "user:1:name", load, keel.WithTTL(10*time.Minute))
	fmt.Println(name)
	// Output: Ada Lovelace
}

func ExampleDB_CompareAndSwap() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	db.Set(ctx, "config:version", "1")
	swapped, _ := db.CompareAndSwap(ctx, "config:version", "1", "2")
	fmt.Println(swapped)
	swapped, _ = db.CompareAndSwap(ctx, "config:version", "1", "3")
	fmt.Println(swapped)
	// Output:
	// true
	// false
}

func ExampleDB_Stats() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	db.Set(ctx, "k", "v")
	db.Get(ctx, "k")
	db.Get(ctx, "missing")
	s, _ := db.Stats(ctx)
	fmt.Println(s.Keys, s.KeyspaceHits, s.KeyspaceMisses)
	// Output: 1 1 1
}

func ExampleDB_Atomic() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()

	// Count a hit and start its window together: no other call sees the
	// count without its expiry.
	var hits *keel.Result[int64]
	err := db.Atomic(ctx, func(tx *keel.Tx) error {
		hits = tx.Incr("hits:ada")
		tx.Expire("hits:ada", time.Minute, keel.ExpireNX)
		return nil
	})
	fmt.Println(err)
	fmt.Println(hits.Value())
	// Output:
	// <nil>
	// 1 <nil>
}

func ExampleDB_Atomic_failure() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()
	db.Set(ctx, "name", "Ada")

	// As in Redis, a command that fails does not undo the others.
	var incr *keel.Result[int64]
	db.Atomic(ctx, func(tx *keel.Tx) error {
		tx.Set("seen", "yes")
		incr = tx.Incr("name")
		return nil
	})
	fmt.Println(errors.Is(incr.Err(), keel.ErrNotInteger))
	fmt.Println(db.Get(ctx, "seen"))
	// Output:
	// true
	// yes <nil>
}

func ExampleDB_Scan() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()
	db.MSet(ctx, map[string]string{"user:1": "Ada", "user:2": "Grace", "session:9": "x"})

	var users []string
	for key, err := range db.Scan(ctx, keel.ScanOptions{Match: "user:*"}) {
		if err != nil {
			panic(err)
		}
		users = append(users, key)
	}
	slices.Sort(users)
	fmt.Println(users)
	// Output: [user:1 user:2]
}

func ExampleDB_MGet() {
	ctx := context.Background()
	db, _ := keel.Open(ctx, keel.Options{})
	defer db.Close()
	db.MSet(ctx, map[string]string{"a": "1", "b": "2"})

	values, _ := db.MGet(ctx, "a", "b", "missing")
	fmt.Println(values)
	// Output: map[a:1 b:2]
}
