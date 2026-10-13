package server

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"sync"

	"github.com/brandopakel/keel/internal/core"
)

// Alternative implementations of the server on net.Listener and Go's runtime
// netpoller, for the comparison the upstream performance issue asks for.
//
// The event-loop design gets one property for free that these do not. A single
// thread can touch the engine without contention, but goroutine-per-connection
// cannot: the stores have no locking of their own, and concurrent access to
// them is a data race Go turns into a hard crash. So every command here runs
// under the engine's lock (core.Engine.Lock), as the event loop's do.
// NetVariantMutex and NetVariantChannel both preserve the event loop's
// execution semantics - one command at a time, in arrival order - so what the
// benchmark compares is the I/O mechanism. Sharding the stores would be faster
// and would measure a different program.
type NetVariant int

const (
	// NetVariantMutex is goroutine-per-connection with execution serialised
	// behind the engine's lock. bufio both directions.
	NetVariantMutex NetVariant = iota
	// NetVariantSmallBuf is the same with 512-byte buffers instead of 4096,
	// to test how much of the per-connection memory cost is tunable.
	NetVariantSmallBuf
	// NetVariantDirect drops bufio.Reader. The read path already accumulates
	// into a per-connection slice, so bufio on top of that is a second copy of
	// the same bytes for no benefit.
	NetVariantDirect
	// NetVariantChannel keeps one goroutine per connection for I/O but funnels
	// every command through a single executor goroutine. This is the faithful
	// "use the standard library for I/O, keep the single-threaded core"
	// rewrite, and it replaces lock contention with channel handoff: the
	// executor takes the engine's lock once a batch, and nothing contends
	// for it.
	NetVariantChannel
)

// ActiveNetVariant is the variant RunNetTCPServer serves. EvalUnlocked has
// NetVariantMutex and its siblings run each command without the engine's
// lock, to measure what the lock costs them.
var (
	ActiveNetVariant = NetVariantMutex
	EvalUnlocked     bool
)

type connWriter struct{ w *bufio.Writer }

func (c connWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (c connWriter) Write(p []byte) (int, error) { return c.w.Write(p) }

// --- single-executor plumbing for NetVariantChannel ---

type execReq struct {
	cmds []*core.Command
	done chan []byte
}

var execCh chan execReq

func startExecutor(e *core.Engine) {
	execCh = make(chan execReq, 1024)
	go func() {
		for req := range execCh {
			var rb replyBuffer
			e.Lock()
			for _, cmd := range req.cmds {
				responseRw(e, cmd, &rb)
			}
			e.Unlock()
			out := make([]byte, rb.buf.Len())
			copy(out, rb.buf.Bytes())
			req.done <- out
		}
	}()
}

func bufSizeFor(v NetVariant) int {
	if v == NetVariantSmallBuf {
		return 512
	}
	return readChunkSize
}

func handleConn(conn net.Conn, variant NetVariant, e *core.Engine) {
	defer conn.Close()

	size := bufSizeFor(variant)
	w := bufio.NewWriterSize(conn, size)
	out := connWriter{w: w}

	var src io.Reader = conn
	if variant != NetVariantDirect {
		src = bufio.NewReaderSize(conn, size)
	}

	var pending []byte
	chunk := make([]byte, size)
	var done chan []byte
	if variant == NetVariantChannel {
		done = make(chan []byte, 1)
	}

	for {
		n, err := src.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)

			var batch []*core.Command
			bad := false
			for len(pending) > 0 {
				cmd, consumed, perr := core.ParseCmd(pending)
				if errors.Is(perr, core.ErrIncompleteFrame) {
					break
				}
				if perr != nil {
					e.NoteErrorReply(core.Encode(perr, false))
					responseErrorRw(perr, out)
					w.Flush()
					bad = true
					break
				}
				pending = pending[consumed:]
				batch = append(batch, cmd)
			}
			if bad {
				return
			}

			if len(batch) > 0 {
				if variant == NetVariantChannel {
					execCh <- execReq{cmds: batch, done: done}
					if _, werr := w.Write(<-done); werr != nil {
						return
					}
				} else {
					for _, cmd := range batch {
						if EvalUnlocked {
							responseRw(e, cmd, out)
						} else {
							e.Lock()
							responseRw(e, cmd, out)
							e.Unlock()
						}
					}
				}
			}

			// One flush per read, so a pipelined batch costs one write syscall
			// rather than one per command.
			if ferr := w.Flush(); ferr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// RunNetTCPServer serves e on net.Listener with one goroutine per connection,
// at the address o gives. It is a benchmark mode: the rest of o is the event
// loop's.
func RunNetTCPServer(wg *sync.WaitGroup, e *core.Engine, o Options) error {
	defer wg.Done()
	o = o.WithDefaults()
	if ActiveNetVariant == NetVariantChannel {
		startExecutor(e)
	}
	addr := net.JoinHostPort(o.Host, itoa(o.Port))
	log.Println("starting a net.Listener TCP server on", o.Host, o.Port, "variant", ActiveNetVariant)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Println(err)
		return err
	}
	defer ln.Close()

	// Accept blocks until a client arrives, so the only way to interrupt it is
	// to close the listener out from under it.
	setWaker(func() { ln.Close() })

	for {
		conn, err := ln.Accept()
		if err != nil {
			if shuttingDown() {
				log.Println("accept loop stopped")
				return nil
			}
			// Not a shutdown, so this is one failed accept and the listener is
			// still good. Previously this branch also caught a closed listener,
			// where Accept fails immediately and forever - a `continue` there
			// spins the loop at 100% CPU rather than ending it.
			log.Println("accept:", err)
			continue
		}
		go handleConn(conn, ActiveNetVariant, e)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
