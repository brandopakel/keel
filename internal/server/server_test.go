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
// buffered state afterwards. clients is keyed by fd and the OS reuses fd
// numbers aggressively, so a test that left an entry behind would corrupt the
// next one.
func socketPair(t *testing.T) (r, w int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	t.Cleanup(func() {
		delete(clients, fds[0])
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	})
	return fds[0], fds[1]
}

// testScratch stands in for the per-thread read scratch the pool hands out. It
// is one array reused by every call, deliberately: several of these tests exist
// to prove that a command parsed out of it is not invalidated by the next read.
var testScratch = make([]byte, readChunkSize)

// readCommandsFD reads one batch from fd through the connection state the event
// loop would be keeping for it.
func readCommandsFD(fd int) ([]*core.MemKVCmd, error) {
	c := clients[fd]
	if c == nil {
		c = &client{fd: fd}
		clients[fd] = c
	}
	return c.readCommands(testScratch)
}

// buffered returns the partial-frame buffer held for fd, or nil if there is
// none. A connection sending whole commands must hold nothing between them.
func buffered(fd int) *connBuffer {
	if c := clients[fd]; c != nil {
		return c.buf
	}
	return nil
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
	assert.Nil(t, buffered(r), "a completed frame must leave no buffer behind")
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
	assert.Nil(t, buffered(r), "a batch of whole commands must leave no buffer behind")
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
	assert.NotNil(t, buffered(r), "an unfinished frame has to be held somewhere")

	// ...and the buffer must be released the moment the frame completes.
	if _, err := syscall.Write(w, partial[len(partial)-4:]); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmds := drain(t, r, 1)
	assert.Len(t, cmds, 1)
	assert.Nil(t, buffered(r), "the buffer must be dropped once the frame completes")
}

// TestReadCommandsFDDoesNotAliasScratchBuffer guards the assumption that makes
// a reused read buffer safe. Commands are parsed straight out of the thread's
// scratch, so if ParseCmd ever returned []byte views instead of copying into
// strings, an earlier command's arguments would be silently rewritten by a
// later read - and with I/O threads the scratch is now reused across several
// connections in a row, not just across reads of one.
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

// TestReadCommandsFDDoesNotPreallocateForClaimedSize covers the clamp on sized
// reads. FrameShortfall reports what the client says is coming, and a client is
// free to lie: announcing a 256MB payload must not cost 256MB of buffer before
// any of it has actually turned up.
func TestReadCommandsFDDoesNotPreallocateForClaimedSize(t *testing.T) {
	r, w := socketPair(t)

	header := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$268435456\r\nabc")
	if _, err := syscall.Write(w, header); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readCommandsFD(r); err != nil {
		t.Fatalf("readCommandsFD: %v", err)
	}

	b := buffered(r)
	if b == nil {
		t.Fatal("an unfinished frame should be buffered")
	}
	// One more read, now that a shortfall of 256MB is knowable from the buffer.
	if _, err := syscall.Write(w, []byte("def")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readCommandsFD(r); err != nil {
		t.Fatalf("readCommandsFD: %v", err)
	}

	assert.Less(t, cap(buffered(r).data), 4*readChunkSize,
		"buffer must grow with bytes actually received, not with the size the client claimed")
}

// TestReadCommandsFDReassemblesValueLargerThanOneRead exercises the sized-read
// path end to end: the value is several times readChunkSize, so it is delivered
// through the connection buffer rather than the shared scratch.
func TestReadCommandsFDReassemblesValueLargerThanOneRead(t *testing.T) {
	r, w := socketPair(t)

	value := strings.Repeat("0123456789abcdef", 64*1024) // 1 MB
	payload := encodeCmd("SET", "big", value)

	go func() {
		for chunk := payload; len(chunk) > 0; {
			n, err := syscall.Write(w, chunk)
			if err != nil {
				return
			}
			chunk = chunk[n:]
		}
	}()

	cmds := drain(t, r, 1)
	assert.Len(t, cmds, 1)
	assert.Equal(t, "SET", cmds[0].Cmd)
	assert.Equal(t, value, cmds[0].Args[1], "a 1MB value must survive reassembly byte for byte")
	assert.Nil(t, buffered(r), "the buffer must be released once the frame completes")
}

// TestReadCommandsFDHandlesReadLargerThanScratch is a regression test for a
// panic: "slice bounds out of range [:98200] with capacity 65536".
//
// Once a frame is in progress the read goes into the connection's own buffer and
// is sized to what the frame needs, so it can return far more than readScratch
// holds. Code that reslices the scratch by that count crashes the whole server.
//
// The reassembly tests above did not catch it. A socketpair's default buffer is
// small enough that no single read ever came back larger than readScratch, so
// the bad path was never taken until real TCP, with a much larger receive
// buffer, handed back 98200 bytes at once. This test enlarges the socket buffers
// to force it, and asserts a big read actually happened rather than trusting it.
func TestReadCommandsFDHandlesReadLargerThanScratch(t *testing.T) {
	r, w := socketPair(t)
	for _, fd := range []int{r, w} {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 8<<20); err != nil {
			t.Skipf("cannot size socket buffer: %v", err)
		}
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 8<<20); err != nil {
			t.Skipf("cannot size socket buffer: %v", err)
		}
	}

	value := strings.Repeat("x", 2<<20) // 2 MB
	payload := encodeCmd("SET", "big", value)
	go func() {
		for chunk := payload; len(chunk) > 0; {
			n, err := syscall.Write(w, chunk)
			if err != nil {
				return
			}
			chunk = chunk[n:]
		}
	}()

	var (
		cmds     []*core.MemKVCmd
		prev     int
		biggest  int
		attempts int
	)
	for len(cmds) == 0 && attempts < 8192 {
		attempts++
		got, err := readCommandsFD(r)
		if err != nil {
			t.Fatalf("readCommandsFD: %v", err)
		}
		cmds = append(cmds, got...)
		if b := buffered(r); b != nil {
			if jump := len(b.data) - prev; jump > biggest {
				biggest = jump
			}
			prev = len(b.data)
		}
	}

	if biggest <= readChunkSize {
		t.Skipf("no read exceeded readChunkSize (largest was %d); this platform "+
			"will not deliver enough in one read to exercise the case", biggest)
	}
	assert.Len(t, cmds, 1)
	assert.Equal(t, value, cmds[0].Args[1], "a 2MB value must survive a read larger than the scratch buffer")
}
