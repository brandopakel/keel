// Command cache is Keel embedded as a cache in front of a slow source.
//
// Requests for a user read through keel.DB.GetOrSet: a user the cache holds
// is answered from it, and a user it does not is loaded from the source once,
// however many requests miss it at the same moment, then kept for a TTL.
// With -aof the cache is kept in an append-only log, so a second run starts
// warm: its requests are answered without a single load.
//
//	go run ./examples/embedded/cache
//	go run ./examples/embedded/cache -aof cache.aof   # run it twice
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brandopakel/keel"
)

func main() {
	aof := flag.String("aof", "", "keep the cache in this append-only log, replayed when the cache opens")
	flag.Parse()
	if err := run(context.Background(), os.Stdout, *aof); err != nil {
		log.Fatal(err)
	}
}

// source is what the cache stands in front of: a database, an HTTP API,
// anything slow enough to be worth not asking twice.
type source struct {
	loads atomic.Int64
}

// user looks a user up, slowly.
func (s *source) user(ctx context.Context, id string) (string, error) {
	s.loads.Add(1)
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "user number " + id, nil
}

// Cache answers requests for users from a keel.DB, loading what it does not
// hold from its source.
type Cache struct {
	db     *keel.DB
	source *source
	ttl    time.Duration
}

// User is the user id names: from the cache, or loaded from the source and
// kept for the cache's TTL.
func (c *Cache) User(ctx context.Context, id string) (string, error) {
	return c.db.GetOrSet(ctx, "user:"+id, func(ctx context.Context) (string, error) {
		return c.source.user(ctx, id)
	}, keel.WithTTL(c.ttl))
}

// run serves two rounds of 100 concurrent requests for 4 users and reports
// what the source was asked for each.
func run(ctx context.Context, out io.Writer, aof string) error {
	options := keel.Options{MaxMemory: 64 << 20}
	if aof != "" {
		options.Persistence = &keel.Persistence{Path: aof}
	}
	db, err := keel.Open(ctx, options)
	if err != nil {
		return err
	}
	defer db.Close()
	cache := &Cache{db: db, source: &source{}, ttl: time.Hour}

	for round := 1; round <= 2; round++ {
		before := cache.source.loads.Load()
		if err := requests(ctx, cache, 100, 4); err != nil {
			return err
		}
		fmt.Fprintf(out, "round %d: 100 requests for 4 users, %d loads\n", round, cache.source.loads.Load()-before)
	}
	stats, err := db.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cache holds %d keys, %d expiring\n", stats.Keys, stats.KeysWithExpiry)
	return nil
}

// requests makes n concurrent requests for users users, and checks each
// answer.
func requests(ctx context.Context, cache *Cache, n, users int) error {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := strconv.Itoa(i % users)
			name, err := cache.User(ctx, id)
			if err == nil && name != "user number "+id {
				err = fmt.Errorf("user %s answered %q", id, name)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}
