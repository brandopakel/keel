package core_test

import (
	"bytes"
	"syscall"
	"testing"

	"memkv/internal/core"

	"github.com/stretchr/testify/assert"
)

// TestFDCommWriteCompletesLargePayload drives a reply that cannot fit in the
// socket send buffer, which is the case a bare syscall.Write gets wrong: it
// either accepts only part of the payload or returns EAGAIN, and reporting
// either as success truncates the reply stream.
func TestFDCommWriteCompletesLargePayload(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	defer syscall.Close(fds[1])

	// A small send buffer plus a non-blocking socket is what the server does,
	// and is what makes short writes and EAGAIN happen in the first place.
	if err := syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096); err != nil {
		t.Skipf("cannot size send buffer: %v", err)
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatalf("set nonblock: %v", err)
	}

	payload := bytes.Repeat([]byte("abcdefgh"), 64*1024) // 512 KB
	done := make(chan []byte, 1)
	go func() {
		var got []byte
		buf := make([]byte, 32*1024)
		for len(got) < len(payload) {
			n, rerr := syscall.Read(fds[1], buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if rerr != nil && rerr != syscall.EINTR {
				break
			}
			if n == 0 && rerr == nil {
				break
			}
		}
		done <- got
	}()

	n, err := core.FDComm{Fd: fds[0]}.Write(payload)
	syscall.Close(fds[0])

	assert.NoError(t, err)
	assert.Equal(t, len(payload), n, "Write must report every byte it sent")
	assert.True(t, bytes.Equal(payload, <-done), "receiver must see the whole payload intact")
}

// TestFDCommWriteReportsClosedPeer checks that a genuinely broken socket still
// surfaces an error rather than being mistaken for a completed write.
func TestFDCommWriteReportsClosedPeer(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	syscall.Close(fds[1])
	defer syscall.Close(fds[0])

	_, err = core.FDComm{Fd: fds[0]}.Write(bytes.Repeat([]byte("x"), 1<<20))
	assert.Error(t, err, "writing to a closed peer must report an error")
}
