package server

import (
	"log"
	"os"
	"sync"
	"time"
)

// shutdownGrace bounds how long a graceful stop may take before the process
// stops waiting for it.
const shutdownGrace = 5 * time.Second

// shutdown is closed once a termination signal has arrived.
//
// A closed channel is Go's broadcast primitive: every reader observes the close,
// no reader consumes it, and a select with a default branch turns it into a
// non-blocking check. It replaces `for atomic.LoadInt32(&eStatus) == Busy {}`,
// an empty loop that spun a core at 100% for the whole of shutdown.
// shutdownOnce is a pointer because sync.Once contains a Mutex and copying a
// lock is a bug - go vet's copylocks check flags it. A pointer lets a test swap
// the pair out and put them back without ever copying the Once itself.
var (
	shutdown     = make(chan struct{})
	shutdownOnce = new(sync.Once)
)

// requestShutdown asks every server loop to stop. Safe to call repeatedly;
// closing an already-closed channel would panic, which is what the Once guards.
func requestShutdown() { shutdownOnce.Do(func() { close(shutdown) }) }

// shuttingDown reports whether a stop has been requested, without blocking.
func shuttingDown() bool {
	select {
	case <-shutdown:
		return true
	default:
		return false
	}
}

// A running server registers a waker: something that makes its loop return
// promptly from whatever call it is parked in.
//
// Setting the flag is not enough on its own. Both loops block indefinitely by
// design - Check passes no timeout to EpollWait or Kevent, and Accept waits for
// a client - so without a waker a stop would not be noticed until some client
// happened to do something, which on an idle server is never.
var (
	wakeMu sync.Mutex
	wakeFn func()
)

func setWaker(f func()) {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	wakeFn = f
}

func wake() {
	wakeMu.Lock()
	f := wakeFn
	wakeMu.Unlock()
	if f != nil {
		f()
	}
}

// WaitForSignal stops the server on SIGINT or SIGTERM.
//
// It returns instead of calling os.Exit, so the deferred Close calls in the
// server loop actually run. os.Exit skips every pending defer, which left the
// listening socket and the epoll/kqueue descriptor to be reclaimed by the kernel
// on process teardown rather than closed on the way out, and made the
// "shutting down gracefully" it logged untrue.
//
// Once the loop returns, main's wg.Wait unblocks and the process exits by
// itself.
func WaitForSignal(wg *sync.WaitGroup, signals chan os.Signal) {
	defer wg.Done()

	sig := <-signals
	log.Println("received", sig, "- shutting down")
	requestShutdown()
	wake()

	// A backstop, not the normal path: it dies with the process as soon as the
	// loop finishes. It only ever fires if the loop cannot finish - wedged
	// writing to a client that has stopped reading, say - or if whoever sent the
	// first signal is not inclined to wait.
	go func() {
		select {
		case sig := <-signals:
			log.Println("received", sig, "again - exiting now")
		case <-time.After(shutdownGrace):
			log.Println("still shutting down after", shutdownGrace, "- exiting now")
		}
		os.Exit(1)
	}()
}
