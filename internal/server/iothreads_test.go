package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
)

// pairedClients builds n connected socket pairs and a client for each, with the
// given payload already written to every one of them.
func pairedClients(t *testing.T, n int, payload []byte) []*client {
	t.Helper()
	cs := make([]*client, 0, n)
	for i := 0; i < n; i++ {
		r, w := socketPair(t)
		if _, err := syscall.Write(w, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		c := &client{fd: r}
		clients[r] = c
		cs = append(cs, c)
	}
	return cs
}

// TestIOPoolReadsEveryConnectionExactlyOnce is the basic contract of a phase.
//
// Enough connections are used to clear the threshold, so the work really is
// spread across the workers rather than quietly falling back to the loop
// thread - which is also what makes this worth running under -race.
func TestIOPoolReadsEveryConnectionExactlyOnce(t *testing.T) {
	const threads = 4
	const conns = 16
	assert.GreaterOrEqual(t, conns, ioThreshold(threads), "the test must clear the threshold")

	pool := newIOPool(threads)
	defer pool.stop()

	cs := pairedClients(t, conns, encodeCmd("PING"))
	pool.run(cs, false)

	for i, c := range cs {
		assert.NoError(t, c.err, "connection %d", i)
		assert.Len(t, c.cmds, 1, "connection %d must have been read exactly once", i)
		assert.Equal(t, "PING", c.cmds[0].Cmd)
	}
}

// TestIOPoolBelowThresholdStillReadsEverything. A batch too small to be worth
// distributing is done on the loop thread instead, and must be no less complete
// for it.
func TestIOPoolBelowThresholdStillReadsEverything(t *testing.T) {
	pool := newIOPool(8)
	defer pool.stop()

	cs := pairedClients(t, 3, encodeCmd("PING"))
	assert.Less(t, len(cs), ioThreshold(8), "the test must stay under the threshold")

	pool.run(cs, false)
	for i, c := range cs {
		assert.NoError(t, c.err, "connection %d", i)
		assert.Len(t, c.cmds, 1)
	}
}

// TestIOPoolWithOneThreadNeedsNoWorkers. The default configuration must not
// spawn goroutines it will never use.
func TestIOPoolWithOneThreadNeedsNoWorkers(t *testing.T) {
	pool := newIOPool(1)
	defer pool.stop()
	assert.Empty(t, pool.jobs, "a single-threaded pool has nothing to hand work to")

	cs := pairedClients(t, 2, encodeCmd("PING"))
	pool.run(cs, false)
	for _, c := range cs {
		assert.Len(t, c.cmds, 1)
	}
}

// TestIOPoolWritesEveryReply covers the other phase, including that a reply
// staged in the arena arrives intact after being written from another thread.
func TestIOPoolWritesEveryReply(t *testing.T) {
	const threads = 4
	const conns = 16

	pool := newIOPool(threads)
	defer pool.stop()

	var arena replyArena
	cs := make([]*client, 0, conns)
	fds := make([]int, 0, conns)
	want := make([]string, 0, conns)
	for i := 0; i < conns; i++ {
		r, w := socketPair(t)
		c := &client{fd: w}
		clients[w] = c
		c.outStart = len(arena.buf)
		arena.buf = append(arena.buf, fmt.Sprintf("+reply-%d\r\n", i)...)
		c.outEnd = len(arena.buf)
		c.inArena = true
		cs = append(cs, c)
		fds = append(fds, r)
		want = append(want, fmt.Sprintf("+reply-%d\r\n", i))
	}
	for _, c := range cs {
		c.out = arena.buf[c.outStart:c.outEnd]
	}

	pool.run(cs, true)

	for i, fd := range fds {
		got := make([]byte, len(want[i]))
		n, err := syscall.Read(fd, got)
		assert.NoError(t, err)
		assert.Equal(t, want[i], string(got[:n]), "connection %d", i)
	}
}

// TestArenaWindowsSurviveItsGrowth is the reason the reply positions are kept
// as offsets and only turned into slices at the end of the cycle.
//
// Appending to the arena can move the array. A slice taken before that move
// points at the old one, so every reply staged earlier in the cycle would be
// read back from a buffer nothing is writing to any more - stale for some
// connections and not others, which is the worst kind of bug to find later.
func TestArenaWindowsSurviveItsGrowth(t *testing.T) {
	var arena replyArena
	arena.reset()

	type window struct {
		c    *client
		want string
	}
	var windows []window
	for i := 0; i < 500; i++ {
		c := &client{fd: -1}
		reply := fmt.Sprintf("+%s-%d\r\n", strings.Repeat("x", i), i)
		c.outStart = len(arena.buf)
		arenaWriter{&arena}.Write([]byte(reply))
		c.outEnd = len(arena.buf)
		windows = append(windows, window{c, reply})
	}

	for _, w := range windows {
		w.c.out = arena.buf[w.c.outStart:w.c.outEnd]
	}
	for i, w := range windows {
		assert.Equal(t, w.want, string(w.c.out), "reply %d must survive every later append", i)
	}
}

// TestArenaGivesBackWhatABurstMadeItGrow. One deeply pipelined moment must not
// leave the server holding that buffer for the rest of its life - the whole
// point of the arena is that per-connection memory stays near zero.
func TestArenaGivesBackWhatABurstMadeItGrow(t *testing.T) {
	var arena replyArena

	arena.reset()
	arenaWriter{&arena}.Write(make([]byte, 4*arenaShrinkAbove))
	grown := cap(arena.buf)
	assert.Greater(t, grown, arenaShrinkAbove)

	// A quiet cycle after the burst.
	arena.reset()
	arenaWriter{&arena}.Write([]byte("+OK\r\n"))
	arena.reset()

	assert.Less(t, cap(arena.buf), grown, "the arena must give the burst's memory back")
}

// TestArenaKeepsCapacityWhileItIsBeingUsed. Shrinking has to be about a burst
// that has passed, not about ordinary variation, or a steadily busy server
// would reallocate its arena every cycle.
func TestArenaKeepsCapacityWhileItIsBeingUsed(t *testing.T) {
	var arena replyArena
	arena.reset()
	arenaWriter{&arena}.Write(make([]byte, 4*arenaShrinkAbove))
	grown := cap(arena.buf)

	for i := 0; i < 5; i++ {
		arena.reset()
		arenaWriter{&arena}.Write(make([]byte, 3*arenaShrinkAbove))
		assert.Equal(t, grown, cap(arena.buf), "a still-busy arena must not be reallocated")
	}
}

// TestCaptureWriterDoesNotCopyASingleReply is the optimisation that keeps a
// large value cheap: a batch of one has nothing to coalesce, so its reply is
// kept by reference rather than copied into the arena.
func TestCaptureWriterDoesNotCopyASingleReply(t *testing.T) {
	var w captureWriter
	reply := []byte("$5\r\nhello\r\n")
	n, err := w.Write(reply)
	assert.NoError(t, err)
	assert.Equal(t, len(reply), n)
	assert.Equal(t, &reply[0], &w.p[0], "a single reply must be kept, not copied")

	// A second write is not expected from any command today, but if one ever
	// arrived it has to append rather than silently replace the first.
	_, err = w.Write([]byte(":1\r\n"))
	assert.NoError(t, err)
	assert.Equal(t, "$5\r\nhello\r\n:1\r\n", string(w.p))
	assert.Equal(t, "$5\r\nhello\r\n", string(reply), "and must not have written through the first")
}

// TestExecuteRunCoalescesABatchButNotASingleReply pins which of the two staging
// paths a batch takes, since the whole reason both exist is that each is wrong
// for the other's case.
func TestExecuteRunCoalescesABatchButNotASingleReply(t *testing.T) {
	var arena replyArena
	arena.reset()

	one := &client{fd: -1, cmds: []*core.Command{{Cmd: "PING"}}}
	assert.True(t, executeRun(one, &arena))
	assert.False(t, one.inArena, "a batch of one must not be copied into the arena")
	assert.Equal(t, "+PONG\r\n", string(one.out))
	assert.Empty(t, arena.buf)

	many := &client{fd: -1, cmds: []*core.Command{{Cmd: "PING"}, {Cmd: "PING"}, {Cmd: "PING"}}}
	assert.True(t, executeRun(many, &arena))
	assert.True(t, many.inArena, "a batch of several must be coalesced into one write")
	many.out = arena.buf[many.outStart:many.outEnd]
	assert.Equal(t, "+PONG\r\n+PONG\r\n+PONG\r\n", string(many.out))

	none := &client{fd: -1}
	assert.False(t, executeRun(none, &arena), "nothing to say means nothing to write")
}

// TestThreadedServerAnswersEveryClient runs the real loop with I/O threads on
// and several connections pipelining at once.
//
// This is the test that matters for the whole change: reads and writes happen
// on other threads while execution happens on the loop's, so under -race it is
// what would catch a store or a connection being touched from two places.
func TestThreadedServerAnswersEveryClient(t *testing.T) {
	withFreshShutdownState(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot find a free port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	oldHost, oldPort, oldThreads := config.Host, config.Port, config.IOThreads
	config.Host, config.Port, config.IOThreads = "127.0.0.1", port, 4
	defer func() { config.Host, config.Port, config.IOThreads = oldHost, oldPort, oldThreads }()

	var wg sync.WaitGroup
	wg.Add(1)
	go RunAsyncTCPServer(&wg)
	defer func() {
		requestShutdown()
		wake()
		wg.Wait()
		for fd := range clients {
			delete(clients, fd)
		}
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var dialed net.Conn
	for i := 0; i < 200 && dialed == nil; i++ {
		if conn, derr := net.Dial("tcp", addr); derr == nil {
			dialed = conn
		}
	}
	if dialed == nil {
		t.Skip("server did not come up")
	}
	dialed.Close()

	const conns = 16
	const perConn = 40
	var clientsWG sync.WaitGroup
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		clientsWG.Add(1)
		go func(id int) {
			defer clientsWG.Done()
			conn, derr := net.Dial("tcp", addr)
			if derr != nil {
				errs <- derr
				return
			}
			defer conn.Close()

			// Pipelined, so a single read on the server side yields a batch and
			// the coalescing path is the one under test.
			var out []byte
			for j := 0; j < perConn; j++ {
				key := fmt.Sprintf("k-%d-%d", id, j)
				out = append(out, encodeCmd("SET", key, fmt.Sprintf("v-%d-%d", id, j))...)
				out = append(out, encodeCmd("GET", key)...)
			}
			if _, werr := conn.Write(out); werr != nil {
				errs <- werr
				return
			}

			r := bufio.NewReader(conn)
			for j := 0; j < perConn; j++ {
				line, rerr := r.ReadString('\n')
				if rerr != nil {
					errs <- fmt.Errorf("client %d reply %d: %w", id, j, rerr)
					return
				}
				if line != "+OK\r\n" {
					errs <- fmt.Errorf("client %d SET %d: got %q", id, j, line)
					return
				}
				header, rerr := r.ReadString('\n')
				if rerr != nil {
					errs <- fmt.Errorf("client %d header %d: %w", id, j, rerr)
					return
				}
				want := fmt.Sprintf("v-%d-%d", id, j)
				if header != fmt.Sprintf("$%d\r\n", len(want)) {
					errs <- fmt.Errorf("client %d GET %d header: got %q", id, j, header)
					return
				}
				body, rerr := r.ReadString('\n')
				if rerr != nil {
					errs <- fmt.Errorf("client %d body %d: %w", id, j, rerr)
					return
				}
				if body != want+"\r\n" {
					errs <- fmt.Errorf("client %d GET %d: got %q want %q", id, j, body, want)
					return
				}
			}
		}(i)
	}
	clientsWG.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
