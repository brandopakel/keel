package server

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// withFreshShutdownState swaps in an unclosed channel for the duration of a
// test and puts the original back afterwards.
//
// A closed channel cannot be reopened, so a test that called requestShutdown on
// the real one would leave shuttingDown reporting true for every test that ran
// after it - and Go runs a package's tests in a single process, so that spreads.
// Saving and restoring package state is the usual way out when the thing under
// test is a global.
func withFreshShutdownState(t *testing.T) {
	t.Helper()
	ch, once := shutdown, shutdownOnce
	t.Cleanup(func() { shutdown, shutdownOnce = ch, once })
	shutdown, shutdownOnce = make(chan struct{}), new(sync.Once)
}

func TestShuttingDownReportsRequestedStop(t *testing.T) {
	withFreshShutdownState(t)

	assert.False(t, shuttingDown(), "a fresh server is not shutting down")
	requestShutdown()
	assert.True(t, shuttingDown(), "a requested stop must be visible to every loop")
}

// TestRequestShutdownIsIdempotent matters because two signals can arrive, and
// closing an already-closed channel panics.
func TestRequestShutdownIsIdempotent(t *testing.T) {
	withFreshShutdownState(t)

	assert.NotPanics(t, func() {
		requestShutdown()
		requestShutdown()
		requestShutdown()
	})
	assert.True(t, shuttingDown())
}

// TestShuttingDownDoesNotBlock pins the reason for the select/default: the
// check runs on every pass of the event loop and must never park it.
func TestShuttingDownDoesNotBlock(t *testing.T) {
	withFreshShutdownState(t)

	done := make(chan bool, 1)
	go func() { done <- shuttingDown() }()
	select {
	case got := <-done:
		assert.False(t, got)
	case <-make(chan struct{}):
		t.Fatal("unreachable")
	}
}

func TestWakeInvokesRegisteredWaker(t *testing.T) {
	orig := wakeFn
	t.Cleanup(func() { setWaker(orig) })

	var mu sync.Mutex
	calls := 0
	setWaker(func() { mu.Lock(); calls++; mu.Unlock() })

	wake()
	wake()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, calls, "wake must call whatever the running server registered")
}

// TestWakeWithoutWakerIsSafe covers the startup window: a signal can arrive
// before any server loop has registered how to interrupt itself.
func TestWakeWithoutWakerIsSafe(t *testing.T) {
	orig := wakeFn
	t.Cleanup(func() { setWaker(orig) })

	setWaker(nil)
	assert.NotPanics(t, wake)
}

// TestWakeDoesNotStopALoopThatIsNotShuttingDown.
//
// The wakeup pipe carries two meanings: stop, and keep turning because a
// rewrite has slices left. A byte cannot say which, so the loop reads the
// shutdown flag instead - and the flag is set before the wake, so a stop can
// never be mistaken for a poke.
//
// Reading the pipe as a stop unconditionally is what the loop used to do, and
// the first thing that ever poked it shut the server down mid-rewrite.
func TestWakeDoesNotStopALoopThatIsNotShuttingDown(t *testing.T) {
	withFreshShutdownState(t)

	woken := 0
	setWaker(func() { woken++ })
	defer setWaker(nil)

	// A poke while nothing has asked to stop.
	wake()
	assert.Equal(t, 1, woken, "the waker must fire")
	assert.False(t, shuttingDown(), "a poke must not look like a stop")

	// And a real stop still reads as one.
	requestShutdown()
	wake()
	assert.True(t, shuttingDown())
}
