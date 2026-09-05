package io_multiplexing

import (
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

	events, err := mux.Check()
	assert.NoError(t, err)
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
	events, err := mux.Check()
	assert.NoError(t, err)
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
		events, err := mux.Check()
		assert.NoError(t, err)
		assert.Len(t, events, 1, "an unread pipe stays ready, check %d", i)
	}
	var buf [1]byte
	syscall.Read(r, buf[:])

	// Drained, the pipe is no longer reported; a write from elsewhere is
	// what ends the wait. The writer records that it has written before it
	// writes, so Check returning proves the write happened first - without a
	// measured duration, which a slow scheduler could make wrong either way.
	var written atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond)
		written.Store(true)
		syscall.Write(w, []byte{2})
	}()
	events, err := mux.Check()
	assert.NoError(t, err)
	assert.Len(t, events, 1)
	assert.True(t, written.Load(), "Check returned before anything was written")
}

func TestClosedWriteEndReadsAsReadable(t *testing.T) {
	mux, err := CreateIOMultiplexer()
	assert.NoError(t, err)
	defer mux.Close()

	r, w := pipe(t)
	assert.NoError(t, mux.Monitor(Event{Fd: r, Op: OpRead}))
	syscall.Close(w)

	events, err := mux.Check()
	assert.NoError(t, err)
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
