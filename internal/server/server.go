package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"memkv/internal/core/io_multiplexing"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"memkv/internal/config"
	"memkv/internal/constant"
	"memkv/internal/core"
)

var eStatus int32 = constant.EngineStatusWaiting

func WaitForSignal(wg *sync.WaitGroup, signals chan os.Signal) {
	defer wg.Done()
	<-signals
	for atomic.LoadInt32(&eStatus) == constant.EngineStatusBusy {
	}
	log.Println("Shutting down gracefully")
	os.Exit(0)
}

// readChunkSize is how much we attempt to pull off a socket per syscall.
const readChunkSize = 4096

// pendingReads holds the bytes that have arrived on a connection but do not yet
// form a complete command.
//
// A single syscall.Read gives us whatever the kernel happens to have buffered,
// which is not necessarily one command: a large SET can be split across several
// reads, and a pipelining client can pack many commands into one. Parsing the
// raw result of a single read therefore fails in both directions - it truncates
// big commands and it silently discards every command after the first. We keep a
// per-connection buffer instead and only ever hand whole frames to the parser.
//
// Entries are removed by closeClient when the connection goes away.
var pendingReads = make(map[int][]byte)

// readCommandsFD reads once from fd and returns every complete command that is
// now available, leaving any trailing partial command buffered for next time.
func readCommandsFD(fd int) ([]*core.MemKVCmd, error) {
	var chunk = make([]byte, readChunkSize)
	n, err := syscall.Read(fd, chunk)
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

	buf := append(pendingReads[fd], chunk[:n]...)

	var cmds []*core.MemKVCmd
	for len(buf) > 0 {
		cmd, consumed, err := core.ParseCmd(buf)
		if errors.Is(err, core.ErrIncompleteFrame) {
			// The rest of this command has not arrived yet. Keep what we have.
			break
		}
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, cmd)
		buf = buf[consumed:]
	}

	if len(buf) == 0 {
		delete(pendingReads, fd)
	} else {
		// Copy the remainder so we do not pin the whole read chunk in memory.
		rest := make([]byte, len(buf))
		copy(rest, buf)
		pendingReads[fd] = rest
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
	if len(cmds) == 0 {
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

	for atomic.LoadInt32(&eStatus) != constant.EngineStatusShuttingDown {
		// check if any FD is ready for an IO
		events, err = ioMultiplexer.Check()
		if err != nil {
			continue
		}

		if !atomic.CompareAndSwapInt32(&eStatus, constant.EngineStatusWaiting, constant.EngineStatusBusy) {
			if eStatus == constant.EngineStatusShuttingDown {
				return nil
			}
		}
		for i := 0; i < len(events); i++ {
			if events[i].Fd == serverFD {
				// the Server FD is ready for reading, means we have a new client.
				clientNumber++
				log.Printf("new client: id=%d\n", clientNumber)
				// accept the incoming connection from a client
				connFD, _, err := syscall.Accept(serverFD)
				if err != nil {
					log.Println("err", err)
					continue
				}

				if err = syscall.SetNonblock(connFD, true); err != nil {
					return err
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
					return err
				}
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
					atomic.SwapInt32(&eStatus, constant.EngineStatusWaiting)
					continue
				}
				if WriteBuffered {
					respondBatch(comm, cmds)
				} else {
					for _, cmd := range cmds {
						responseRw(cmd, comm)
					}
				}
			}
			atomic.SwapInt32(&eStatus, constant.EngineStatusWaiting)
		}
	}

	return nil
}
