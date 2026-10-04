package main

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// growLoopbackSendBuffers makes the kernel allocate, before the server starts,
// the socket send buffers a slow-reader test is about to pin, then frees them.
//
// A connection that never reads holds whatever its send buffer took: about
// half a megabyte on macOS, in 16 KB mbuf clusters. A slow-reader test is a
// deliberate pile of those - TestCommandReservationsRecoverAfterSlowReaders
// leaves 24 of them full, roughly 14 MB - and the kernel has to find that
// memory the moment the server writes the replies.
//
// On a macos-15-intel runner the kernel cannot always find it in time. The
// pool starts small (5 clusters of 16 KB in one fresh runner's netstat -mm),
// and each cluster is physically contiguous memory. Growing the pool right
// after the disk-heavy tests before this one, with four packages compiling,
// means the kernel's mbuf worker has to assemble contiguous pages: relocating
// pages, flushing TLBs and writing back dirty file buffers to free them. That
// took longer than the test's five-second read deadline, and every socket
// send on the machine waited for it. A spindump of one failure (diagnostic
// run 37164486352, leg 12) has the worker busy in cpm_allocate for the whole
// second sampled, and the server's event loop, a TCP timer thread, launchd,
// trustd and the runner's agent all asleep in the mbuf allocator, the
// server's thread in state U. A non-blocking write(2) cannot refuse that
// wait, so the loop served nobody and the test's next INFO timed out. The
// server did nothing wrong: Redis's loop would wait in the same call.
//
// It reproduces without Keel (diagnostic run 37180560378). On fresh runners
// under compile load, after writing 1.2 GB of files, filling 24 never-read
// loopback connections from Python left one non-blocking send in the kernel
// for 5.5 s in one leg and 14.7 s in another. A second fill straight after,
// from the pool the first had grown, had no send over 50 ms. Under compile
// load alone, with no file writes first, the pool grew in under a second
// (run 37179318052).
//
// What the slow-reader tests measure is the server's accounting and fairness,
// not how quickly the kernel finds contiguous memory. So the pool is grown
// here, by connections of the same shape the test is about to make, before
// the server is started and with nothing timed. The kernel keeps the clusters
// after the connections are reset, so the server's writes come from a pool
// that already has room. Every deadline the tests set stays as it was.
func growLoopbackSendBuffers(t *testing.T, connections int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var held []*net.TCPConn
	defer func() {
		for _, c := range held {
			// Reset rather than linger: the unsent bytes are dropped and their
			// clusters go back to the pool at once.
			_ = c.SetLinger(0)
			_ = c.Close()
		}
	}()
	chunk := make([]byte, 8<<20)
	started := time.Now()
	for i := 0; i < connections; i++ {
		reader, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, reader.(*net.TCPConn))
		if err := reader.(*net.TCPConn).SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		writer := accepted.(*net.TCPConn)
		held = append(held, writer)
		// Write until the kernel takes no more, as the server's non-blocking
		// writes do, without waiting for writability in between: the time
		// spent is the kernel's alone.
		raw, err := writer.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var werr error
		if err := raw.Write(func(fd uintptr) bool {
			for {
				_, werr = syscall.Write(int(fd), chunk)
				if werr != nil {
					if errors.Is(werr, syscall.EAGAIN) {
						werr = nil
					}
					return true
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		if werr != nil {
			t.Fatal(werr)
		}
	}
	if took := time.Since(started); took > time.Second {
		t.Logf("the kernel took %s to buffer %d never-read loopback connections", took.Round(time.Millisecond), connections)
	}
}
