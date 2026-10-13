// Command ratelimit is Keel embedded as a fixed-window rate limiter.
//
// Each request counts against its client's window with INCR, and the first
// request of a window starts it with EXPIRE ... NX, so the count is gone when
// the window ends. The two run in one keel.DB.Atomic transaction: no request
// sees a count without its expiry, and a crash between them cannot leave a
// counter that never resets, because the log holds both or neither.
//
//	go run ./examples/embedded/ratelimit
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/brandopakel/keel"
)

func main() {
	if err := run(context.Background(), os.Stdout, time.Second); err != nil {
		log.Fatal(err)
	}
}

// Limiter allows each client limit requests a window.
type Limiter struct {
	db     *keel.DB
	limit  int64
	window time.Duration
}

// Allow counts a request of client's against its window, and reports
// whether the window allows it.
func (l *Limiter) Allow(ctx context.Context, client string) (bool, error) {
	key := "ratelimit:" + client
	var count *keel.Result[int64]
	err := l.db.Atomic(ctx, func(tx *keel.Tx) error {
		count = tx.Incr(key)
		tx.Expire(key, l.window, keel.ExpireNX)
		return nil
	})
	if err != nil {
		return false, err
	}
	n, err := count.Value()
	if err != nil {
		return false, err
	}
	return n <= l.limit, nil
}

// run sends two clients' requests at a limiter of 5 a window, then waits
// for the window to end and asks again.
func run(ctx context.Context, out io.Writer, window time.Duration) error {
	db, err := keel.Open(ctx, keel.Options{})
	if err != nil {
		return err
	}
	defer db.Close()
	limiter := &Limiter{db: db, limit: 5, window: window}

	for _, client := range []string{"ada", "grace"} {
		requests := map[string]int{"ada": 8, "grace": 3}[client]
		allowed := 0
		for range requests {
			ok, err := limiter.Allow(ctx, client)
			if err != nil {
				return err
			}
			if ok {
				allowed++
			}
		}
		fmt.Fprintf(out, "%s: %d of %d requests allowed\n", client, allowed, requests)
	}

	time.Sleep(window + window/10)
	ok, err := limiter.Allow(ctx, "ada")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ada, in the next window: allowed %v\n", ok)
	return nil
}
