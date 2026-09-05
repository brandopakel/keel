package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
)

// readChunkSize is how much we attempt to pull off a socket when we have no
// better idea, which is any read that does not continue a frame already in
// progress.
//
// 4096 was costing a syscall every 4KB: 64 of them to receive one 256KB value,
// and syscalls rather than copying are what dominates the large-value path -
// GET, which crosses the kernel boundary once, beats SET at every size despite
// doing more copying. Raising it to 64KB measured 1.66x at d=65536 and 1.83x at
// d=262144, and nothing either way below 4KB. It is close to free: the scratch
// is one array per I/O thread, not one per connection.
const readChunkSize = 64 * 1024

// maxDirectRead caps a single sized read so that a frame header claiming a huge
// payload cannot turn into an equally huge allocation before any of that
// payload has actually arrived.
const maxDirectRead = 1 << 20

// maxQueryBuffer bounds incomplete request bytes per connection.
var maxQueryBuffer = 16 * 1024 * 1024

const maxOutputBuffer = 64 << 20
const maxRetainedClientBytes = 256 << 20

var retainedClientBytes int

// A read lands in a scratch array rather than one allocated per read.
//
// Each read is fully dealt with - parsed, or copied into a connBuffer - before
// the same thread starts another, so the array can be reused, and a read
// holding only whole commands is parsed straight out of it without any copy at
// all. There is one per I/O thread rather than one for the whole server, which
// still leaves it well short of one per connection; ioPool owns them.
//
// That is only sound because ParseCmd converts payloads with string(data[...]),
// which allocates a copy. No command holds a reference back into the scratch.
// If the parser ever starts returning []byte views, this has to become a
// per-read allocation again.

// connBuffer holds the bytes of a command that arrived split across reads.
//
// Only a connection with an unfinished frame has one, and it is dropped as soon
// as the frame completes, so idle connections hold nothing. That is deliberate:
// per-connection memory is the event loop's main advantage over a goroutine per
// connection, and a buffer retained per client would give it away.
//
// data[off:] is the unparsed remainder. Consuming a command moves off rather
// than reallocating, and the remainder slides to the front only when the array
// genuinely needs the room. The previous version allocated a fresh array and
// copied the entire remainder after every read, so a value spanning k reads was
// copied k times over - 8.5MB of copying to receive one 256KB value. Cost here
// is linear in the size of the value.
type connBuffer struct {
	data []byte
	off  int
}

func (b *connBuffer) unparsed() []byte { return b.data[b.off:] }
func (b *connBuffer) size() int        { return len(b.data) - b.off }

// add appends p, reclaiming the space held by already-parsed bytes first.
func (b *connBuffer) add(p []byte) {
	b.reserve(len(p))
	b.data = append(b.data, p...)
}

// reserve makes room for n more bytes without a further allocation.
func (b *connBuffer) reserve(n int) {
	if cap(b.data)-len(b.data) >= n {
		return
	}
	if b.off > 0 {
		// Parsed bytes at the front are dead weight: slide the remainder down
		// rather than allocating a larger array to hold both.
		m := copy(b.data, b.data[b.off:])
		b.data = b.data[:m]
		b.off = 0
		if cap(b.data)-len(b.data) >= n {
			return
		}
	}
	grown := make([]byte, len(b.data), max(2*cap(b.data), len(b.data)+n))
	copy(grown, b.data)
	b.data = grown
}

// spare returns the n writable bytes at the tail, to be read into directly.
// reserve(n) must have been called first.
func (b *connBuffer) spare(n int) []byte { return b.data[len(b.data) : len(b.data)+n] }

// commit accepts the n bytes just written into spare.
func (b *connBuffer) commit(n int) { b.data = b.data[:len(b.data)+n] }

// client is the per-connection state one cycle of the loop carries.
//
// The loop used to read, execute and reply for one connection before looking at
// the next, so none of this had to outlive a local variable. I/O threading
// splits those steps into phases across all ready connections at once, so what
// was local becomes per-connection.
//
// It is kept deliberately thin, because per-connection memory is the event
// loop's main advantage over a goroutine per connection and this is exactly
// where that advantage would be given away. buf is nil unless a frame is
// half-arrived, out is a window into one shared arena rather than a buffer of
// its own, and both are dropped at the end of the cycle. An idle connection
// holds the struct and nothing else.
type client struct {
	fd              int
	lastProgress    time.Time
	closeAfterWrite bool
	authenticated   bool
	accounted       int
	outBytes        int
	frames          []int

	// buf holds the bytes of a command that arrived split across reads, and is
	// nil the rest of the time.
	buf *connBuffer

	// cmds and err are what the read phase produced, for the execute phase.
	cmds []*core.Command
	err  error

	// out is the reply to send in the write phase. When inArena it is a window
	// into the cycle's arena, recorded as offsets because appending to the
	// arena can move it, and resolved to a slice once the cycle stops growing.
	out              []byte
	inArena          bool
	outStart, outEnd int
}

// clients is every connected socket, keyed by descriptor.
//
// Only the loop thread ever adds to or removes from it, and only between
// phases. I/O threads are handed pointers to the values and never touch the map
// itself, which is what keeps it safe without a lock.
var clients = make(map[int]*client)

// readCommands reads once from the socket and returns every complete command
// that is now available, leaving any trailing partial command buffered for next
// time.
//
// scratch belongs to whichever thread is calling, and nothing that survives the
// call points into it.
func (c *client) readCommands(scratch []byte) ([]*core.Command, error) {
	b := c.buf
	if b != nil && b.size() >= maxQueryBuffer {
		// Wrap ErrProtocol so the caller replies before hanging up: the client
		// learns why instead of seeing the connection vanish.
		return nil, fmt.Errorf("%w: query buffer limit of %d bytes exceeded",
			core.ErrProtocol, maxQueryBuffer)
	}

	var (
		n   int
		err error
	)
	if b == nil {
		// Nothing half-parsed, so land in the thread's scratch: no
		// per-connection memory, and no copy at all unless this read ends
		// mid-frame.
		n, err = syscall.Read(c.fd, scratch)
	} else {
		// A frame is in progress. Read straight into the connection's own
		// buffer - going via the scratch would only add a copy - and ask for
		// exactly what the frame still needs rather than a fixed chunk.
		want := core.FrameShortfall(b.unparsed())
		if want <= 0 {
			// Shortfall unknown: the missing bytes are a header whose digits
			// have not all arrived, so how much follows it is not yet settled.
			want = readChunkSize
		}
		// Never speculate further than the client has already backed up. A
		// header claiming 512MB must not become a 512MB allocation before any
		// of the payload shows up; growing with the data still reaches a large
		// read size within a few doublings.
		if limit := b.size() + readChunkSize; want > limit {
			want = limit
		}
		if want > maxDirectRead {
			want = maxDirectRead
		}
		want = min(want, maxQueryBuffer-b.size())
		b.reserve(want)
		n, err = syscall.Read(c.fd, b.spare(want))
	}

	if err == syscall.EAGAIN || err == syscall.EINTR {
		// The socket was reported readable but has nothing for us: a spurious
		// wakeup, or another path drained it first. That is not a failure, and
		// the caller must not close the connection over it.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// The socket was reported readable but yielded nothing, which means the
		// peer has closed its end.
		return nil, io.EOF
	}

	// Only one of these is valid: n can exceed len(scratch) when the read went
	// into the connection's own buffer, so the scratch must not be resliced by
	// it.
	c.lastProgress = time.Now()
	var src []byte
	if b != nil {
		b.commit(n)
		src = b.unparsed()
	} else {
		src = scratch[:n]
	}

	var cmds []*core.Command
	used := 0
	for used < len(src) {
		cmd, consumed, perr := core.ParseCmd(src[used:])
		if errors.Is(perr, core.ErrIncompleteFrame) {
			// The rest of this command has not arrived yet. Keep what we have.
			break
		}
		if perr != nil {
			return cmds, perr
		}
		cmds = append(cmds, cmd)
		used += consumed
	}

	switch {
	case used == len(src):
		// Nothing partial left over, so the connection goes back to holding no
		// buffer at all.
		c.buf = nil
	case b != nil:
		b.off += used
	default:
		// First partial frame on this connection: start buffering the remainder.
		nb := &connBuffer{}
		nb.add(src[used:])
		c.buf = nb
	}
	return cmds, nil
}

// closeClient tears down a connection and drops everything held for it.
func closeClient(c *client) {
	retainedClientBytes -= c.accounted
	c.accounted = 0
	c.cmds = nil
	delete(clients, c.fd)
	c.buf = nil
	c.out = nil
	syscall.Close(c.fd)
}

// responseRw runs one command and writes its reply. The one thing a command
// cannot answer for itself is being unknown, which comes back as an error and
// is answered here.
func responseRw(cmd *core.Command, rw io.ReadWriter) {
	if err := core.EvalAndResponse(cmd, rw); err != nil {
		responseErrorRw(err, rw)
	}
}

// responseErrorRw writes err to the client as a RESP error.
func responseErrorRw(err error, rw io.ReadWriter) {
	if _, werr := rw.Write(core.Encode(err, false)); werr != nil {
		log.Println("failed to send error reply:", werr)
	}
}

// listenTCP opens a non-blocking listening socket on host:port.
//
// Non-blocking from the start, because the loop must never park inside an
// accept or a read on one descriptor while others are ready; it parks only in
// the multiplexer, which watches them all. SO_REUSEADDR lets a restart bind
// the port at once rather than waiting out the previous process's connections
// in TIME_WAIT, which is what Redis and Go's own net package both do.
func listenTCP(host string, port, backlog int) (int, error) {
	addr, err := ipv4Address(host, port)
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	for _, step := range []struct {
		what string
		do   func() error
	}{
		{"set non-blocking on", func() error { return syscall.SetNonblock(fd, true) }},
		{"set SO_REUSEADDR on", func() error {
			return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		}},
		{"bind", func() error { return syscall.Bind(fd, addr) }},
		{"listen on", func() error { return syscall.Listen(fd, backlog) }},
	} {
		if err := step.do(); err != nil {
			syscall.Close(fd)
			return -1, fmt.Errorf("%s %s:%d: %w", step.what, host, port, err)
		}
	}
	return fd, nil
}

// ipv4Address turns the configured host into a socket address.
//
// net.ParseIP returns every address in its 16-byte form, so the first four
// bytes of an IPv4 address are zero. Copying those into the socket address
// bound 0.0.0.0 whatever -host said, silently; To4 gives the four bytes that
// were meant. Only IPv4 is bound, because the event loop's socket is AF_INET.
func ipv4Address(host string, port int) (*syscall.SockaddrInet4, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("-host %q is not an IP address", host)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("-host %q is not an IPv4 address, and the event loop binds IPv4 only", host)
	}
	addr := &syscall.SockaddrInet4{Port: port}
	copy(addr.Addr[:], ip4)
	return addr, nil
}

// acceptClient takes one waiting connection off the listening socket, sets it
// up and registers it with the multiplexer.
//
// Everything here configures this one socket. A failure is that connection's
// problem, not the server's, so it costs the client its connection and nothing
// else. These used to return from the loop, which unwound RunAsyncTCPServer
// and took every other connected client down over one bad descriptor.
func acceptClient(serverFD int, mux io_multiplexing.IOMultiplexer) (*client, bool) {
	connFD, _, err := syscall.Accept(serverFD)
	if err != nil {
		log.Println("accept:", err)
		return nil, false
	}
	if err := syscall.SetNonblock(connFD, true); err != nil {
		log.Println("set nonblock:", err)
		syscall.Close(connFD)
		return nil, false
	}

	// Disable Nagle's algorithm on the client socket.
	//
	// Nagle holds a small reply back until the peer acknowledges the
	// previous segment, and the peer's delayed-ACK timer sits on that
	// acknowledgement. Writing one reply per command means every reply is
	// its own small segment, so a pipelined client pays the timer on every
	// batch: measured on Linux, the server served ~1220 batches/second at
	// P=8, P=16 and P=64 alike, which is 41ms per batch per connection
	// against a 40ms delayed-ACK timer. Redis sets TCP_NODELAY on client
	// sockets for this reason; Go's net package sets it by default.
	if err := syscall.SetsockoptInt(connFD, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1); err != nil {
		log.Println("TCP_NODELAY:", err)
	}

	if err := mux.Monitor(io_multiplexing.Event{Fd: connFD, Op: io_multiplexing.OpRead}); err != nil {
		log.Println("monitor:", err)
		syscall.Close(connFD)
		return nil, false
	}
	return &client{fd: connFD, lastProgress: time.Now()}, true
}

// replyBuffer collects the replies produced from one read so they can be sent
// as a single write.
//
// Writing one reply per command costs one write syscall per command, which
// caps a pipelined client at whatever rate the machine can issue syscalls -
// measured on Linux, a flat ~124k ops/second regardless of how many commands
// were pipelined into each batch. Coalescing a batch into one write removes
// that ceiling and was worth 4.1x at P=8, 8.1x at P=16 and 13.9x at P=64.
type replyBuffer struct{ buf bytes.Buffer }

func (r *replyBuffer) Read([]byte) (int, error)    { return 0, io.EOF }
func (r *replyBuffer) Write(p []byte) (int, error) { return r.buf.Write(p) }

// executeRun runs a connection's parsed commands and stages the reply for the
// write phase, reporting whether there is anything to send.
//
// The reply is staged rather than sent because the write may happen on another
// thread, later in the cycle. Where it is staged depends on how many commands
// there were, and both cases matter:
//
// A batch of several is appended to the cycle's shared arena, so the whole
// batch leaves as one write. One write per command caps a pipelined client at
// the rate the machine can issue syscalls - measured on Linux, a flat ~124k
// ops/second however deep the pipeline - and coalescing was worth 4.1x at P=8,
// 8.1x at P=16 and 13.9x at P=64.
//
// A batch of one is captured by reference instead. There is nothing to
// coalesce, and copying the reply into the arena would buy nothing and cost a
// copy of the whole value - which matters because a large value is nearly
// always a batch of one, since it fills a read on its own.
func executeRun(c *client, arena *replyArena) bool {
	c.out, c.inArena = nil, false
	c.outBytes = 0
	defer func() { c.cmds = nil }()

	switch len(c.cmds) {
	case 0:
		return false
	case 1:
		var w captureWriter
		c.respond(c.cmds[0], &w)
		if len(w.p) > maxOutputBuffer {
			c.err = fmt.Errorf("output buffer limit exceeded")
			return false
		}
		c.out = w.p
		return len(c.out) > 0
	default:
		c.outStart = len(arena.buf)
		for _, cmd := range c.cmds {
			before := len(arena.buf)
			c.respond(cmd, arenaWriter{arena})
			if WriteUnbuffered {
				c.frames = append(c.frames, len(arena.buf)-before)
			}
			if len(arena.buf)-c.outStart > maxOutputBuffer {
				arena.buf = arena.buf[:c.outStart]
				c.err = fmt.Errorf("output buffer limit exceeded")
				return false
			}
		}
		c.outEnd = len(arena.buf)
		c.inArena = c.outEnd > c.outStart
		return c.inArena
	}
}

func RunAsyncTCPServer(wg *sync.WaitGroup) error {
	defer wg.Done()
	defer func() {
		core.CancelRewrite()
		for _, c := range clients {
			closeClient(c)
		}
		setWaker(nil)
	}()
	log.Println("starting an asynchronous TCP server on", config.Host, config.Port)

	clientNumber := 0
	nextMaintenance := time.Now()
	var heldReplies []*client
	paused := make(map[int]*client)

	// The connections taking part in each phase of the current cycle, and the
	// arena their replies are staged in. Kept across cycles and truncated
	// rather than reallocated, so a busy loop does no allocation of its own.
	var (
		readable []*client
		writable []*client
		arena    replyArena
	)

	pool := newIOPool(ioThreadCount())
	defer pool.stop()

	serverFD, err := listenTCP(config.Host, config.Port, config.MaxConnection)
	if err != nil {
		log.Println(err)
		return err
	}
	defer syscall.Close(serverFD)

	// The multiplexer is what the loop parks in: it watches the listening
	// socket, every client socket and the wakeup pipe below, and returns
	// whichever of them are ready.
	ioMultiplexer, err := io_multiplexing.CreateIOMultiplexer()
	if err != nil {
		return err
	}
	defer ioMultiplexer.Close()
	if err = ioMultiplexer.Monitor(io_multiplexing.Event{Fd: serverFD, Op: io_multiplexing.OpRead}); err != nil {
		return err
	}

	// A pipe whose read end is monitored alongside the client sockets.
	//
	// Check blocks with no timeout, so an idle server is parked in a syscall
	// that no flag can interrupt. Writing one byte here makes the multiplexer
	// return immediately. The byte also persists in the pipe, so a signal that
	// arrives before the loop parks is not lost - it is reported on the next
	// Check rather than missed.
	var wakeupFDs [2]int
	if err = syscall.Pipe(wakeupFDs[:]); err != nil {
		return err
	}
	defer syscall.Close(wakeupFDs[0])
	defer syscall.Close(wakeupFDs[1])
	if err = ioMultiplexer.Monitor(io_multiplexing.Event{
		Fd: wakeupFDs[0],
		Op: io_multiplexing.OpRead,
	}); err != nil {
		return err
	}
	if err := syscall.SetNonblock(wakeupFDs[0], true); err != nil {
		return err
	}
	if err := syscall.SetNonblock(wakeupFDs[1], true); err != nil {
		return err
	}
	setWaker(func() { syscall.Write(wakeupFDs[1], []byte{0}) })
	defer setWaker(nil) // detach callbacks before closing/reusing the pipe descriptors
	replicaUpdates, stopReplica := startReplicaTransport()
	defer stopReplica()

	// A heartbeat, so work that is due because of the clock happens on a server
	// nobody is talking to.
	//
	// Check blocks with no timeout, so an idle loop is parked until a client
	// does something - and an idle server is exactly the one where expired keys
	// pile up unread. Poking the wakeup pipe at a fixed rate gives the loop a
	// turn to run the expiry cycle in. Redis calls its equivalent serverCron
	// and runs it at 10Hz; this is the same rate for the same reason.
	// Its own stop signal rather than the shared shutdown channel. Watching
	// that would mean this goroutine reading a package-level variable that
	// something else may replace, which is a data race whether or not it ever
	// bites - and closing a channel owned here stops the ticker at exactly the
	// moment this loop returns, which is what it should be tied to anyway.
	cronStop := make(chan struct{})
	defer close(cronStop)
	go func() {
		tick := time.NewTicker(time.Duration(config.CronIntervalMs) * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-cronStop:
				return
			case <-tick.C:
				wake()
			}
		}
	}()

	for !shuttingDown() {
		// Park until something is ready. Every other syscall in this loop is
		// non-blocking, so this is the only place it waits.
		events, err := ioMultiplexer.Check()
		if err != nil {
			if shuttingDown() {
				break
			}
			if err == syscall.EINTR {
				continue
			}
			log.Println("multiplexer:", err)
			continue
		}

		if config.AOFAsyncAppend && core.AppendPending() {
			ready, err := core.FlushAOFAsync(wake)
			if err != nil {
				requestShutdown()
				return err
			}
			if ready {
				live := heldReplies[:0]
				for _, c := range heldReplies {
					if clients[c.fd] == c {
						live = append(live, c)
					}
				}
				flushClientReplies(pool, ioMultiplexer, live)
				heldReplies = nil
				for fd, c := range paused {
					if clients[fd] == c {
						op := io_multiplexing.OpRead
						if len(c.out) > 0 {
							op = io_multiplexing.OpWrite
						}
						if err := ioMultiplexer.Monitor(io_multiplexing.Event{Fd: fd, Op: op}); err != nil {
							closeClient(c)
						}
					}
					delete(paused, fd)
				}
				if core.RewriteActive() {
					wake()
				}
				// Readiness is level-triggered; re-check after restoring interests.
				continue
			}
			for _, ev := range events {
				switch ev.Fd {
				case wakeupFDs[0]:
					var drain [64]byte
					syscall.Read(wakeupFDs[0], drain[:])
				case serverFD:
					if len(clients) < config.MaxConnection {
						if c, ok := acceptClient(serverFD, ioMultiplexer); ok {
							clients[c.fd] = c
						}
					} else {
						fd, _, err := syscall.Accept(serverFD)
						if err == nil {
							syscall.Close(fd)
						}
					}
				default:
					if c := clients[ev.Fd]; c != nil {
						if err := ioMultiplexer.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpNone}); err != nil {
							closeClient(c)
						} else {
							paused[c.fd] = c
						}
					}
				}
			}
			if time.Now().After(nextMaintenance) {
				nextMaintenance = time.Now().Add(time.Second)
				for _, c := range clients {
					if time.Since(c.lastProgress) > 30*time.Second && (len(c.out) > 0 || c.buf != nil) {
						closeClient(c)
						delete(paused, c.fd)
					}
				}
			}
			continue
		}

		select {
		case update := <-replicaUpdates:
			err := core.ApplyReplication(update.frame)
			update.applied <- err
			if err != nil {
				requestShutdown()
				return err
			}
		default:
		}

		// Set when the wakeup pipe fires. The batch in hand is served to
		// completion first, so a client that is mid-request gets its reply
		// rather than having the connection dropped underneath it.
		stop := false
		readable = readable[:0]
		writable = writable[:0]

		for _, ev := range events {
			switch ev.Fd {
			case wakeupFDs[0]:
				// The pipe carries two different meanings now: stop, and keep
				// turning because a rewrite has slices left. A byte cannot say
				// which, so the flag does - and it is set before the wake, so a
				// stop is never read as a poke.
				//
				// Reading it as a stop unconditionally is what the first
				// version did, and the first thing that poked the loop shut the
				// server down.
				if shuttingDown() {
					stop = true
					continue
				}
				// Drained, because it stays readable until it is emptied. A
				// stop only ever needs one byte and then the loop ends; a poke
				// happens every cycle of every rewrite, and an undrained pipe
				// would leave the loop spinning on it for the rest of the
				// server's life.
				var drain [64]byte
				syscall.Read(wakeupFDs[0], drain[:])
			case serverFD:
				// The listening socket is readable: a client is waiting to be
				// accepted. Counted only once the connection is fully set up,
				// so the id in the log matches a client that actually exists.
				if len(clients) >= config.MaxConnection {
					fd, _, err := syscall.Accept(serverFD)
					if err == nil {
						syscall.Close(fd)
					}
					continue
				}
				if c, ok := acceptClient(serverFD, ioMultiplexer); ok {
					clients[c.fd] = c
					clientNumber++
					log.Printf("new client: id=%d\n", clientNumber)
				}
			default:
				// An existing client is sending commands. Nothing is read yet:
				// the whole ready set is collected first so the read phase can
				// be handed out across threads in one go.
				if c := clients[ev.Fd]; c != nil {
					if len(c.out) > 0 {
						writable = append(writable, c)
					} else {
						readable = append(readable, c)
					}
				}
			}
		}

		// Phase one: read and parse, in parallel when there is enough of it.
		pool.run(readable, false)

		// Phase two: execute. On this thread only, and in the order the
		// multiplexer reported the connections, so the stores stay unsynchronised
		// and a client's commands still run in the order it sent them.
		arena.reset()
		for _, c := range readable {
			if !accountClient(c) {
				closeClient(c)
				continue
			}
			if c.err != nil {
				if errors.Is(c.err, core.ErrProtocol) {
					c.out = core.Encode(c.err, false)
					c.closeAfterWrite = true
					writable = append(writable, c)
				} else {
					closeClient(c)
				}
				continue
			}

			if executeRun(c, &arena) {
				if !accountClient(c) {
					if c.inArena {
						arena.buf = arena.buf[:c.outStart]
					}
					closeClient(c)
					continue
				}
				writable = append(writable, c)
			} else if c.err != nil {
				closeClient(c)
			}
		}

		// Reap idle keys before flushing their removal records.
		core.ExpireCycle()

		// The log is written and synced here, after every command has run and
		// before a single reply goes out. Under appendfsync always that
		// ordering is the whole guarantee: a client is told its write succeeded
		// only once the record of it is on disk. Flushing after the replies
		// would be faster and would be a lie.
		ready := true
		var flushErr error
		if config.AOFAsyncAppend {
			ready, flushErr = core.FlushAOFAsync(wake)
		} else {
			flushErr = core.FlushAOF()
		}
		if flushErr != nil {
			log.Println("appendonly: write failed, stopping:", flushErr)
			requestShutdown()
			return flushErr
		}
		if !ready {
			for _, c := range writable {
				if c.inArena {
					c.out = append([]byte(nil), arena.buf[c.outStart:c.outEnd]...)
					c.outBytes = cap(c.out)
					c.inArena = false
				}
				if !accountClient(c) {
					closeClient(c)
					continue
				}
				if err := ioMultiplexer.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpNone}); err != nil {
					closeClient(c)
					continue
				}
				paused[c.fd] = c
				heldReplies = append(heldReplies, c)
			}
			continue
		}

		// A rewrite advances one slice per cycle, inside the flush above, and
		// cycles only happen when the multiplexer returns. A server that goes
		// quiet the moment a rewrite starts would leave it part-finished until
		// the next client turned up, so the loop wakes itself until it is done.
		// Not deferred: a defer inside this loop would fire once the loop had
		// ended, which is exactly too late to be of use.
		if core.RewriteActive() {
			wake()
		}

		// Offsets into the arena become slices only now: until the last reply
		// was appended, another append could have moved the array underneath
		// any slice taken earlier.
		for _, c := range writable {
			if c.inArena {
				c.out = arena.buf[c.outStart:c.outEnd]
			}
		}

		// Every syscall remains nonblocking. A partial reply owns its remaining
		// bytes and resumes on writable readiness; no worker waits for a reader.
		flushClientReplies(pool, ioMultiplexer, writable)
		now := time.Now()
		if !now.Before(nextMaintenance) {
			nextMaintenance = now.Add(time.Second)
			for _, c := range clients {
				if (len(c.out) > 0 || c.buf != nil) && now.Sub(c.lastProgress) > 30*time.Second {
					closeClient(c)
				}
			}
		}

		if stop {
			break
		}
	}

	// Reached only on a requested stop, so the deferred Close calls run here
	// rather than being skipped by an os.Exit. The log is closed with them,
	// which flushes and syncs whatever the last cycle produced - without it a
	// clean shutdown would lose acknowledged writes, the one kind of loss a
	// client has no way to notice.
	if err := core.CloseAOF(); err != nil {
		return fmt.Errorf("appendonly: close failed: %w", err)
	}
	log.Println("event loop stopped")
	return nil
}

// StartAOF loads any existing append-only file and opens it for appending.
//
// Loading first and opening second is deliberate: opening installs the hook
// that records evictions, and replaying a log with that hook live would append
// what it is currently reading. A truncated final command is the ordinary shape
// of a crash, so it is logged and the server starts anyway on the commands
// before it; anything else is a refusal to start, because beginning from a
// partially understood log means quietly serving a keyspace that is missing
// whatever came after the part that failed.
func StartAOF() error {
	if !config.AOFEnabled {
		return nil
	}
	readFrom := aofReadPath(config.AOFFileName, config.LegacyAOFFileName)
	if readFrom != config.AOFFileName {
		log.Printf("appendonly: reading %s, written before the rename; "+
			"new records go to %s", readFrom, config.AOFFileName)
	}

	applied, err := core.LoadAOF(readFrom)
	switch {
	case core.IsTruncatedAOF(err):
		if repairErr := core.RepairAOFTail(err); repairErr != nil {
			return repairErr
		}
		log.Printf("appendonly: %v - starting from what was intact", err)
	case err != nil:
		return err
	case applied > 0:
		log.Printf("appendonly: replayed %d commands from %s", applied, readFrom)
	}
	if err := core.OpenAOF(config.AOFFileName); err != nil {
		return err
	}

	// Having replayed the old file, write the whole keyspace into the new one
	// before anything else appends to it.
	//
	// Without this the fallback loses the data it exists to save, one restart
	// later rather than immediately. The first start reads memkv-master.aof and
	// opens an empty keel-master.aof; the second start sees keel-master.aof
	// present, prefers it, and replays only what was written after the
	// migration. Everything that lived solely in the old log is gone, and the
	// old log is still sitting there looking like a backup.
	//
	// A rewrite is exactly the right thing here: the shortest log producing the
	// current state. It runs to completion before the server serves anyone,
	// which at startup costs one pass over a keyspace that was just built by
	// one pass over the same data.
	if readFrom != config.AOFFileName {
		if err := core.RewriteAOF(); err != nil {
			return fmt.Errorf("migrating %s to %s: %w", readFrom, config.AOFFileName, err)
		}
		log.Printf("appendonly: wrote the replayed keyspace to %s; %s is no longer read",
			config.AOFFileName, readFrom)
	}

	log.Printf("appendonly: on, %s, appendfsync %s", config.AOFFileName, config.AOFFsync)
	return nil
}

// aofReadPath chooses which log to replay at startup.
//
// The default log was ./memkv-master.aof before the server was renamed and is
// ./keel-master.aof now. Without this, the first restart after the rename finds
// nothing at the new name, replays nothing, and serves an empty keyspace
// beside a perfectly good log it never opened - no error and no warning, which
// is the worst shape a data loss can take.
//
// The old name is a fallback and not a merge: if both files are there, the
// current one is the live log and the old one is whatever was left behind. Only
// the current name is ever written.
func aofReadPath(current, legacy string) string {
	if _, err := os.Stat(current); err == nil {
		return current
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return current
}

// respond enforces connection-local authentication before dispatching commands.
func (c *client) respond(cmd *core.Command, w io.ReadWriter) {
	if strings.EqualFold(cmd.Cmd, "AUTH") {
		valid := len(cmd.Args) == 1 || (len(cmd.Args) == 2 && cmd.Args[0] == "default")
		if !valid {
			responseErrorRw(fmt.Errorf("ERR wrong number of arguments or unsupported user for AUTH"), w)
			return
		}
		if config.RequirePass == "" {
			responseErrorRw(fmt.Errorf("ERR AUTH called without a configured password"), w)
			return
		}
		got, want := sha256.Sum256([]byte(cmd.Args[len(cmd.Args)-1])), sha256.Sum256([]byte(config.RequirePass))
		c.authenticated = subtle.ConstantTimeCompare(got[:], want[:]) == 1
		if !c.authenticated {
			responseErrorRw(fmt.Errorf("WRONGPASS invalid username-password pair"), w)
			return
		}
		w.Write([]byte("+OK\r\n"))
		return
	}
	if config.RequirePass != "" && !c.authenticated {
		responseErrorRw(fmt.Errorf("NOAUTH Authentication required"), w)
		return
	}
	responseRw(cmd, w)
}

// accountClient bounds retained user-space request/reply storage across connections.
// Command decoding and response construction still need transient headroom.
func accountClient(c *client) bool {
	used := max(cap(c.out), c.outBytes)
	if c.inArena {
		used = c.outEnd - c.outStart
	}
	c.outBytes = used
	used += cap(c.frames) * 8
	if c.buf != nil {
		used += cap(c.buf.data)
	}
	retainedClientBytes += used - c.accounted
	c.accounted = used
	return retainedClientBytes <= maxRetainedClientBytes
}

func flushClientReplies(pool *ioPool, ioMultiplexer io_multiplexing.IOMultiplexer, writable []*client) {
	pool.run(writable, true)
	for _, c := range writable {
		if c.err != nil {
			closeClient(c)
			continue
		}
		if len(c.out) > 0 {
			if c.inArena {
				c.out = append([]byte(nil), c.out...)
				c.outBytes = cap(c.out)
				c.inArena = false
			}
			if err := ioMultiplexer.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpWrite}); err != nil {
				closeClient(c)
				continue
			}
		} else {
			c.out = nil
			c.outBytes = 0
			c.frames = nil
			c.inArena = false
			if c.closeAfterWrite {
				closeClient(c)
				continue
			}
			if err := ioMultiplexer.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpRead}); err != nil {
				closeClient(c)
				continue
			}
		}
		accountClient(c)
	}
}
