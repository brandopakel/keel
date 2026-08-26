package server

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"sync"

	"memkv/internal/config"
	"memkv/internal/core"
)

// Alternative implementations of the server on net.Listener and Go's runtime
// netpoller, for the comparison the upstream performance issue asks for.
//
// The event-loop design gets one property for free that these do not. A single
// thread can touch the command stores without synchronisation, but
// goroutine-per-connection cannot: the package-level maps in core/storage.go
// have no locking and concurrent access to them is a data race Go turns into a
// hard crash. NetVariantMutex and NetVariantChannel both preserve the event
// loop's execution semantics - one command at a time, in arrival order - so
// what the benchmark compares is the I/O mechanism. Sharding the stores would
// be faster and would measure a different program.
type NetVariant int

const (
	// NetVariantMutex is goroutine-per-connection with execution serialised
	// behind one mutex. bufio both directions.
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
	// rewrite, and it replaces lock contention with channel handoff.
	NetVariantChannel
)

var (
	ActiveNetVariant = NetVariantMutex
	evalMu           sync.Mutex
	EvalUnlocked     bool
)

type connWriter struct{ w *bufio.Writer }

func (c connWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (c connWriter) Write(p []byte) (int, error) { return c.w.Write(p) }

// --- single-executor plumbing for NetVariantChannel ---

type execReq struct {
	cmds []*core.MemKVCmd
	done chan []byte
}

var execCh chan execReq

func startExecutor() {
	execCh = make(chan execReq, 1024)
	go func() {
		for req := range execCh {
			var rb replyBuf
			for _, cmd := range req.cmds {
				responseRw(cmd, &rb)
			}
			out := make([]byte, rb.b.Len())
			copy(out, rb.b.Bytes())
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

func handleConn(conn net.Conn, variant NetVariant) {
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

			var batch []*core.MemKVCmd
			bad := false
			for len(pending) > 0 {
				cmd, consumed, perr := core.ParseCmd(pending)
				if errors.Is(perr, core.ErrIncompleteFrame) {
					break
				}
				if perr != nil {
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
							responseRw(cmd, out)
						} else {
							evalMu.Lock()
							responseRw(cmd, out)
							evalMu.Unlock()
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

// RunNetTCPServer serves on net.Listener with one goroutine per connection.
func RunNetTCPServer(wg *sync.WaitGroup) error {
	defer wg.Done()
	if ActiveNetVariant == NetVariantChannel {
		startExecutor()
	}
	addr := net.JoinHostPort(config.Host, itoa(config.Port))
	log.Println("starting a net.Listener TCP server on", config.Host, config.Port, "variant", ActiveNetVariant)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Println(err)
		return err
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		go handleConn(conn, ActiveNetVariant)
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
