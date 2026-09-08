package io_multiplexing

import (
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pipe stands in for a socket: its read end becomes readable when its write
// end is written to, on both platforms.
func pipe(t *testing.T) (r, w int) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fds[0]); syscall.Close(fds[1]) })
	return fds[0], fds[1]
}

// The production loop retries interrupted waits. Runtime signals may interrupt
// an otherwise valid Check on either platform; tests must use the same contract.
func checkEvents(t *testing.T, mux IOMultiplexer) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := mux.Check()
		if err == syscall.EINTR && time.Now().Before(deadline) {
			continue
		}
		require.NoError(t, err)
		return events
	}
}

func TestCheckReportsTheDescriptorThatBecameReadable(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	r1, w1 := pipe(t)
	r2, _ := pipe(t)
	assert.NoError(t, mux.Monitor(Event{Fd: r1, Op: OpRead}))
	assert.NoError(t, mux.Monitor(Event{Fd: r2, Op: OpRead}))

	_, err = syscall.Write(w1, []byte{1})
	assert.NoError(t, err)

	events := checkEvents(t, mux)
	assert.Equal(t, []Event{{Fd: r1, Op: OpRead}}, events, "only the pipe that was written to is ready")
}

func TestCheckReportsEveryReadyDescriptor(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	var reads []int
	for i := 0; i < 5; i++ {
		r, w := pipe(t)
		reads = append(reads, r)
		assert.NoError(t, mux.Monitor(Event{Fd: r, Op: OpRead}))
		syscall.Write(w, []byte{byte(i)})
	}
	events := checkEvents(t, mux)
	var got []int
	for _, e := range events {
		got = append(got, e.Fd)
		assert.Equal(t, OpRead, e.Op)
	}
	assert.ElementsMatch(t, reads, got)
}

func TestCheckIsLevelTriggered(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	r, w := pipe(t)
	assert.NoError(t, mux.Monitor(Event{Fd: r, Op: OpRead}))
	syscall.Write(w, []byte{1})

	for i := 0; i < 3; i++ {
		events := checkEvents(t, mux)
		assert.Len(t, events, 1, "an unread pipe stays ready, check %d", i)
	}
	var buf [1]byte
	syscall.Read(r, buf[:])

	// Drained, the pipe is no longer reported. Check may now return empty when
	// its interval passes, so the property is that it never reports a
	// descriptor that is not ready - not that it only returns when one is.
	// The writer records that it has written before it writes, so an event
	// proves the write happened first, without measuring a duration that a
	// slow scheduler could make wrong either way.
	var written atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond)
		written.Store(true)
		syscall.Write(w, []byte{2})
	}()
	for {
		events := checkEvents(t, mux)
		if len(events) == 0 {
			continue // the interval passed with nothing ready
		}
		assert.Len(t, events, 1)
		assert.True(t, written.Load(), "Check reported a descriptor before anything was written")
		return
	}
}

// A bounded wait provides another loop turn without a readiness notification.
// It does not restore missing descriptor registrations or establish client recovery.
func TestCheckReturnsWithoutAnyDescriptorBecomingReady(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	r, w := pipe(t)
	assert.NoError(t, mux.Monitor(Event{Fd: r, Op: OpRead}))

	// If the timeout regresses to an infinite wait, wake the watched descriptor
	// so the test can fail and close resources instead of hanging the suite.
	watchdogDone := make(chan struct{})
	watchdog := time.AfterFunc(2*time.Second, func() {
		defer close(watchdogDone)
		_, _ = syscall.Write(w, []byte{1})
	})
	defer func() {
		if !watchdog.Stop() {
			<-watchdogDone
		}
	}()
	start := time.Now()
	events := checkEvents(t, mux)
	assert.Empty(t, events, "nothing was written, so nothing is ready")
	assert.Less(t, time.Since(start), 2*time.Second,
		"an idle Check must return on its own rather than parking forever")
}

func TestClosedWriteEndReadsAsReadable(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	r, w := pipe(t)
	assert.NoError(t, mux.Monitor(Event{Fd: r, Op: OpRead}))
	syscall.Close(w)

	events := checkEvents(t, mux)
	assert.Equal(t, []Event{{Fd: r, Op: OpRead}}, events, "a hang-up is reported as readable")
	n, err := syscall.Read(r, make([]byte, 8))
	assert.NoError(t, err)
	assert.Zero(t, n, "and the read then returns nothing, which is how the loop learns the peer went")
}

func TestMonitorRefusesABadDescriptor(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()
	assert.Error(t, mux.Monitor(Event{Fd: -1, Op: OpRead}))
}
