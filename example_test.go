package keel_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
