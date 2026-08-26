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

// This file implements the alternative asked for in the upstream performance
// issue: the same server built on net.Listener and Go's runtime netpoller
// instead of hand-rolled epoll/kqueue. It exists so the two designs can be
// benchmarked against each other; it is not wired in by default.
//
// The event-loop design gets one property for free that this one does not.
// A single-threaded loop can touch the command stores without synchronisation,
// but goroutine-per-connection cannot: the package-level maps in core/storage.go
// have no locking, and concurrent access to them is a data race that Go turns
// into a hard crash. Serialising execution behind one mutex keeps the exact
// execution semantics of the event loop, so what the benchmark compares is the
// I/O mechanism and nothing else. Sharding the stores would be faster and would
// also make the comparison meaningless.
var evalMu sync.Mutex

// EvalUnlocked drops the mutex around command execution. This is a diagnostic
// only, to separate the cost of the I/O mechanism from the cost of the
// synchronisation that goroutine-per-connection forces on the shared stores.
// It is only safe for commands that touch no store (PING), and must never be
// used to serve real traffic.
var EvalUnlocked bool

// connWriter adapts a buffered writer to the io.ReadWriter that EvalAndResponse
// expects. Reads never happen through it; the read side is handled by the loop.
type connWriter struct{ w *bufio.Writer }

func (c connWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (c connWriter) Write(p []byte) (int, error) { return c.w.Write(p) }

func handleConn(conn net.Conn) {
	defer conn.Close()

	r := bufio.NewReaderSize(conn, readChunkSize)
	w := bufio.NewWriterSize(conn, readChunkSize)
	out := connWriter{w: w}

	var pending []byte
	chunk := make([]byte, readChunkSize)

	for {
		n, err := r.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)

			for len(pending) > 0 {
				cmd, consumed, perr := core.ParseCmd(pending)
				if errors.Is(perr, core.ErrIncompleteFrame) {
					break
				}
				if perr != nil {
					responseErrorRw(perr, out)
					w.Flush()
					return
				}
				pending = pending[consumed:]

				if EvalUnlocked {
					responseRw(cmd, out)
				} else {
					evalMu.Lock()
					responseRw(cmd, out)
					evalMu.Unlock()
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
	addr := net.JoinHostPort(config.Host, itoa(config.Port))
	log.Println("starting a net.Listener TCP server on", config.Host, config.Port)

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
		go handleConn(conn)
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
