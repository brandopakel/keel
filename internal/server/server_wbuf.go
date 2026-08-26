package server

import (
	"bytes"
	"io"
	"syscall"

	"memkv/internal/core"
)

// WriteBuffered makes the event loop coalesce all replies produced from one
// read into a single write syscall.
//
// It exists to keep the epoll/kqueue-versus-netpoller comparison honest. The
// net.Listener implementation writes through a bufio.Writer and therefore gets
// reply coalescing for free, while the event loop as written issues one write
// per command. Comparing the two directly would mostly measure that difference
// rather than the I/O mechanism, so this flag lets the event loop adopt the same
// write policy and isolates the variable under test.
var WriteBuffered bool

// replyBuf collects replies so a batch can be sent as one write.
type replyBuf struct{ b bytes.Buffer }

func (r *replyBuf) Read([]byte) (int, error)    { return 0, io.EOF }
func (r *replyBuf) Write(p []byte) (int, error) { return r.b.Write(p) }

type bufComm struct{ buf *bytes.Buffer }

func (b bufComm) Read([]byte) (int, error)    { return 0, io.EOF }
func (b bufComm) Write(p []byte) (int, error) { return b.buf.Write(p) }

// writeAll drains p to fd, tolerating short writes and a full socket buffer.
func writeAll(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := syscall.Write(fd, p)
		if err == syscall.EAGAIN || err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// respondBuffered evaluates every command and emits one write for the batch.
func respondBuffered(fd int, cmds []*core.MemKVCmd) {
	var buf bytes.Buffer
	out := bufComm{buf: &buf}
	for _, cmd := range cmds {
		responseRw(cmd, out)
	}
	if buf.Len() > 0 {
		writeAll(fd, buf.Bytes())
	}
}
