package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"memkv/internal/core/io_multiplexing"
	"net"
	"sync"
	"syscall"

	"memkv/internal/config"
	"memkv/internal/core"
)

// readChunkSize is how much we attempt to pull off a socket when we have no
// better idea, which is any read that does not continue a frame already in
// progress.
//
// 4096 was costing a syscall every 4KB: 64 of them to receive one 256KB value,
// and syscalls rather than copying are what dominates the large-value path -
// GET, which crosses the kernel boundary once, beats SET at every size despite
// doing more copying. Raising it to 64KB measured 1.66x at d=65536 and 1.83x at
// d=262144, and nothing either way below 4KB. It is close to free: readScratch
// is one array for the whole server, not one per connection.
const readChunkSize = 64 * 1024

// maxDirectRead caps a single sized read so that a frame header claiming a huge
// payload cannot turn into an equally huge allocation before any of that
// payload has actually arrived.
const maxDirectRead = 1 << 20

// maxQueryBuffer bounds the unparsed bytes held for one connection.
//
// Without a bound, a client can open a frame it never finishes - send "*3\r\n"
// and then trickle a byte an hour - and the server buffers for as long as the
// client cares to keep the socket open. Redis bounds the same thing with
// client-query-buffer-limit, and for the same reason sets it above
// proto-max-bulk-len rather than below: the buffer has to be able to hold the
// largest legal command, or valid traffic would be rejected mid-stream. Ours is
// the Redis default and sits above the 512MB maxBulkLength in resp.go.
//
// A var rather than a const so tests can lower it; nothing else assigns to it.
var maxQueryBuffer = 1024 * 1024 * 1024

// readScratch is the landing area for every socket read.
//
// The event loop is single-threaded and each read is fully dealt with - parsed,
// or copied into a connBuffer - before the next one starts, so one shared array
// is safe and saves an allocation per read. It also makes the common case
// copy-free: a read holding only whole commands is parsed straight out of here.
//
// That is only sound because ParseCmd converts payloads with string(data[...]),
// which allocates a copy. No command holds a reference back into this array. If
// the parser ever starts returning []byte views, this has to become a per-read
// allocation again.
var readScratch = make([]byte, readChunkSize)

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

// pendingReads holds the partial-frame buffer for the connections that have one.
//
// Entries are created only when a read ends mid-command, removed as soon as the
// command completes, and removed by closeClient when the connection goes away.
var pendingReads = make(map[int]*connBuffer)

// readCommandsFD reads once from fd and returns every complete command that is
// now available, leaving any trailing partial command buffered for next time.
func readCommandsFD(fd int) ([]*core.MemKVCmd, error) {
	b := pendingReads[fd]
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
		// Nothing half-parsed, so land in the shared scratch: no per-connection
		// memory, and no copy at all unless this read ends mid-frame.
		n, err = syscall.Read(fd, readScratch)
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
		b.reserve(want)
		n, err = syscall.Read(fd, b.spare(want))
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

	// Only one of these is valid: n can exceed len(readScratch) when the read
	// went into the connection's own buffer, so the scratch must not be
	// resliced by it.
	var src []byte
	if b != nil {
		b.commit(n)
		src = b.unparsed()
	} else {
		src = readScratch[:n]
	}

	var cmds []*core.MemKVCmd
	used := 0
	for used < len(src) {
		cmd, consumed, perr := core.ParseCmd(src[used:])
		if errors.Is(perr, core.ErrIncompleteFrame) {
			// The rest of this command has not arrived yet. Keep what we have.
			break
		}
		if perr != nil {
			return nil, perr
		}
		cmds = append(cmds, cmd)
		used += consumed
	}

	switch {
	case used == len(src):
		// Nothing partial left over, so the connection goes back to holding no
		// buffer at all.
		delete(pendingReads, fd)
	case b != nil:
		b.off += used
	default:
		// First partial frame on this connection: start buffering the remainder.
		nb := &connBuffer{}
		nb.add(src[used:])
		pendingReads[fd] = nb
	}
	return cmds, nil
}

// closeClient tears down a client connection and drops any buffered bytes for it.
func closeClient(fd int) {
	delete(pendingReads, fd)
	syscall.Close(fd)
}

func responseRw(cmd *core.MemKVCmd, rw io.ReadWriter) {
	err := core.EvalAndResponse(cmd, rw)
	if err != nil {
		responseErrorRw(err, rw)
	}
}

func responseErrorRw(err error, rw io.ReadWriter) {
	if _, werr := rw.Write([]byte(fmt.Sprintf("-%s%s", err, core.CRLF))); werr != nil {
		log.Println("failed to send error reply:", werr)
	}
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

// respondBatch evaluates every command and emits one write for the whole batch.
func respondBatch(comm core.FDComm, cmds []*core.MemKVCmd) {
	switch len(cmds) {
	case 0:
		return
	case 1:
		// Nothing to coalesce. Buffering one reply copies it in full and then
		// issues the same single write it would have issued anyway, so for a
		// batch of one the buffer is pure cost - and a large value is nearly
		// always a batch of one, since it fills a read on its own.
		responseRw(cmds[0], comm)
		return
	}

	var rb replyBuffer
	for _, cmd := range cmds {
		responseRw(cmd, &rb)
	}
	if rb.buf.Len() == 0 {
		return
	}
	if _, err := comm.Write(rb.buf.Bytes()); err != nil {
		log.Println("failed to send reply batch:", err)
	}
}

func RunAsyncTCPServer(wg *sync.WaitGroup) error {
	defer wg.Done()
	log.Println("starting an asynchronous TCP server on", config.Host, config.Port)

	var events = make([]io_multiplexing.Event, config.MaxConnection)
	clientNumber := 0

	// Create a server socket. A socket is an endpoint for communication between client and server
	serverFD, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		log.Println(err)
		return err
	}
	defer syscall.Close(serverFD)

	// Set the Socket operate in a non-blocking mode
	// Default mode is blocking mode: when you read from a FD, control isn't returned
	// until at least one byte of data is read.
	// Non-blocking mode: if the read buffer is empty, it will return immediately.
	// We want non-blocking mode because we will use epoll to monitor and then read from
	// multiple FD, so we want to ensure that none of them cause the program to "lock up."
	if err = syscall.SetNonblock(serverFD, true); err != nil {
		log.Println(err)
		return err
	}

	// Bind the IP and the port to the server socket FD.
	ip4 := net.ParseIP(config.Host)
	if err = syscall.Bind(serverFD, &syscall.SockaddrInet4{
		Port: config.Port,
		Addr: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
	}); err != nil {
		log.Println(err)
		return err
	}

	// Start listening
	if err = syscall.Listen(serverFD, config.MaxConnection); err != nil {
		log.Println(err)
		return err
	}

	// ioMultiplexer is an object that can monitor multiple file descriptor (FD) at the same time.
	// When one or more monitored FD(s) are ready for IO, it will notify our server.
	// Here, we use ioMultiplexer to monitor Server FD and Clients FD.
	ioMultiplexer, err := io_multiplexing.CreateIOMultiplexer()
	if err != nil {
		return err
	}
	defer ioMultiplexer.Close()

	// Monitor "read" events on the Server FD
	if err = ioMultiplexer.Monitor(io_multiplexing.Event{
		Fd: serverFD,
		Op: io_multiplexing.OpRead,
	}); err != nil {
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
	setWaker(func() { syscall.Write(wakeupFDs[1], []byte{0}) })

	for !shuttingDown() {
		// check if any FD is ready for an IO
		events, err = ioMultiplexer.Check()
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

		// Set when the wakeup pipe fires. The batch in hand is served to
		// completion first, so a client that is mid-request gets its reply
		// rather than having the connection dropped underneath it.
		stop := false

		for i := 0; i < len(events); i++ {
			if events[i].Fd == wakeupFDs[0] {
				stop = true
				continue
			}
			if events[i].Fd == serverFD {
				// the Server FD is ready for reading, means we have a new client.
				// accept the incoming connection from a client
				connFD, _, err := syscall.Accept(serverFD)
				if err != nil {
					log.Println("accept:", err)
					continue
				}

				// Everything from here to Monitor configures this one socket.
				// A failure is that connection's problem, not the server's, so
				// it costs the client its connection and nothing else. These
				// used to return, which unwound RunAsyncTCPServer and took every
				// other connected client down over one bad descriptor.
				if err = syscall.SetNonblock(connFD, true); err != nil {
					log.Println("set nonblock:", err)
					syscall.Close(connFD)
					continue
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
				if err = syscall.SetsockoptInt(connFD, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1); err != nil {
					log.Println("TCP_NODELAY:", err)
				}

				// add this new connection to be monitored
				if err = ioMultiplexer.Monitor(io_multiplexing.Event{
					Fd: connFD,
					Op: io_multiplexing.OpRead,
				}); err != nil {
					log.Println("monitor:", err)
					syscall.Close(connFD)
					continue
				}

				// Counted only once the connection is fully set up, so the id
				// in the log matches a client that actually exists.
				clientNumber++
				log.Printf("new client: id=%d\n", clientNumber)
			} else {
				// the Client FD is ready for reading, means an existing client is sending commands
				comm := core.FDComm{Fd: int(events[i].Fd)}
				cmds, err := readCommandsFD(comm.Fd)
				if err != nil {
					// A malformed frame is the client's fault, so tell it what went
					// wrong before hanging up. Either way only this connection dies.
					if errors.Is(err, core.ErrProtocol) {
						responseErrorRw(err, comm)
					}
					closeClient(events[i].Fd)
					clientNumber--
					log.Println("client quit")
					continue
				}
				if WriteUnbuffered {
					for _, cmd := range cmds {
						responseRw(cmd, comm)
					}
				} else {
					respondBatch(comm, cmds)
				}
			}
		}

		if stop {
			break
		}
	}

	// Reached only on a requested stop, so the deferred Close calls run here
	// rather than being skipped by an os.Exit.
	log.Println("event loop stopped")
	return nil
}
