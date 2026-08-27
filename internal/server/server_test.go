package server

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"memkv/internal/core"

	"github.com/stretchr/testify/assert"
)

// socketPair returns a connected pair and guarantees the read side is clean of
// buffered state afterwards. pendingReads is keyed by fd and the OS reuses fd
// numbers aggressively, so a test that left an entry behind would corrupt the
// next one.
func socketPair(t *testing.T) (r, w int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	t.Cleanup(func() {
		delete(pendingReads, fds[0])
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	})
	return fds[0], fds[1]
}

// encodeCmd builds a RESP array of bulk strings, the wire form of a command.
func encodeCmd(parts ...string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return b.Bytes()
}

// drain calls readCommandsFD until it has collected want commands, so a test
// does not depend on how many reads the kernel splits a payload into.
func drain(t *testing.T, fd, want int) []*core.MemKVCmd {
	t.Helper()
	var got []*core.MemKVCmd
	for i := 0; len(got) < want && i < 4096; i++ {
		cmds, err := readCommandsFD(fd)
		if err != nil {
			t.Fatalf("readCommandsFD: %v", err)
		}
		got = append(got, cmds...)
	}
	return got
}

// TestReadCommandsFDReassemblesSplitCommand is the case the buffer exists for:
// a value far larger than one read, delivered in fragments that do not line up
// with frame boundaries.
func TestReadCommandsFDReassemblesSplitCommand(t *testing.T) {
	r, w := socketPair(t)

	value := strings.Repeat("abcdefgh", 16*1024) // 128 KB, ~32 reads
	payload := encodeCmd("SET", "k", value)

	go func() {
		for off := 0; off < len(payload); off += 1000 {
			end := off + 1000
			if end > len(payload) {
				end = len(payload)
			}
			for chunk := payload[off:end]; len(chunk) > 0; {
				n, err := syscall.Write(w, chunk)
				if err != nil {
					return
				}
				chunk = chunk[n:]
			}
		}
	}()

	cmds := drain(t, r, 1)
	assert.Len(t, cmds, 1)
	assert.Equal(t, "SET", cmds[0].Cmd)
	assert.Equal(t, []string{"k", value}, cmds[0].Args, "the 128KB value must survive reassembly intact")
	assert.NotContains(t, pendingReads, r, "a completed frame must leave no buffer behind")
}

// TestReadCommandsFDParsesPipelinedBatch covers the other direction: many whole
// commands arriving in a single read.
func TestReadCommandsFDParsesPipelinedBatch(t *testing.T) {
	r, w := socketPair(t)

	var batch []byte
	for i := 0; i < 50; i++ {
		batch = append(batch, encodeCmd("PING")...)
	}
	if _, err := syscall.Write(w, batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmds := drain(t, r, 50)
	assert.Len(t, cmds, 50, "every pipelined command must be answered, not just the first")
	for _, c := range cmds {
		assert.Equal(t, "PING", c.Cmd)
	}
	assert.NotContains(t, pendingReads, r, "a batch of whole commands must leave no buffer behind")
}

// TestReadCommandsFDLeavesNoBufferBetweenCommands pins the memory property that
// makes the event loop cheap per connection: a connection sending whole commands
// holds no buffer at all between them.
func TestReadCommandsFDLeavesNoBufferBetweenCommands(t *testing.T) {
	r, w := socketPair(t)

	// A partial frame must buffer...
	partial := encodeCmd("SET", "k", "value")
	if _, err := syscall.Write(w, partial[:len(partial)-4]); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readCommandsFD(r); err != nil {
		t.Fatalf("readCommandsFD: %v", err)
	}
	assert.Contains(t, pendingReads, r, "an unfinished frame has to be held somewhere")

	// ...and the buffer must be released the moment the frame completes.
	if _, err := syscall.Write(w, partial[len(partial)-4:]); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmds := drain(t, r, 1)
	assert.Len(t, cmds, 1)
	assert.NotContains(t, pendingReads, r, "the buffer must be dropped once the frame completes")
}

// TestReadCommandsFDDoesNotAliasScratchBuffer guards the assumption that makes
// the shared read buffer safe. Commands are parsed straight out of readScratch,
// so if ParseCmd ever returned []byte views instead of copying into strings, an
// earlier command's arguments would be silently rewritten by a later read.
func TestReadCommandsFDDoesNotAliasScratchBuffer(t *testing.T) {
	r, w := socketPair(t)

	if _, err := syscall.Write(w, encodeCmd("SET", "first", "AAAAAAAAAAAAAAAA")); err != nil {
		t.Fatalf("write: %v", err)
	}
	first := drain(t, r, 1)
	assert.Len(t, first, 1)

	// A second read reuses the same scratch array with different bytes.
	if _, err := syscall.Write(w, encodeCmd("SET", "second", "BBBBBBBBBBBBBBBB")); err != nil {
		t.Fatalf("write: %v", err)
	}
	second := drain(t, r, 1)
	assert.Len(t, second, 1)

	assert.Equal(t, []string{"first", "AAAAAAAAAAAAAAAA"}, first[0].Args,
		"the first command must not be rewritten by the read that followed it")
	assert.Equal(t, []string{"second", "BBBBBBBBBBBBBBBB"}, second[0].Args)
}

// TestReadCommandsFDEnforcesQueryBufferLimit covers the denial-of-service the
// bound exists for: a frame the client opens and never finishes.
func TestReadCommandsFDEnforcesQueryBufferLimit(t *testing.T) {
	r, w := socketPair(t)

	original := maxQueryBuffer
	maxQueryBuffer = 8 * 1024
	defer func() { maxQueryBuffer = original }()

	// A header promising far more than will ever arrive, then a trickle.
	header := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1000000\r\n")
	if _, err := syscall.Write(w, header); err != nil {
		t.Fatalf("write: %v", err)
	}

	var err error
	for i := 0; i < 64 && err == nil; i++ {
		if _, werr := syscall.Write(w, bytes.Repeat([]byte("x"), 1024)); werr != nil {
			break
		}
		_, err = readCommandsFD(r)
	}

	assert.Error(t, err, "an unfinished frame must not be buffered without limit")
	assert.True(t, errors.Is(err, core.ErrProtocol),
		"the limit must report as a protocol error so the client is told before the connection closes")
}
