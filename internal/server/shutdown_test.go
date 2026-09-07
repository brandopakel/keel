package server

import (
	"sync"
	"testing"
	"time"

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

func TestDetachingWakerWaitsForActiveCallback(t *testing.T) {
	orig := wakeFn
	entered, release, detached := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
		setWaker(orig)
	})
	setWaker(func() { close(entered); <-release })
	workers.Add(1)
	go func() { defer workers.Done(); wake() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	// The active callback holds the same lock used by descriptor detachment.
	// This deterministic check catches the old copy-unlock-call behavior even
	// if the detaching goroutine is delayed by the scheduler.
	if wakeMu.TryLock() {
		wakeMu.Unlock()
		t.Fatal("active callback did not serialize descriptor detachment")
	}
	attempting := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		close(attempting)
		setWaker(nil)
		close(detached)
	}()
	<-attempting
	early := false
	select {
	case <-detached:
		early = true
	default:
	}
	releaseOnce.Do(func() { close(release) })
	workers.Wait()
	assert.False(t, early, "descriptor teardown must wait until the old callback returns")
	// A detached callback must not be invoked again (it would close entered twice).
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
