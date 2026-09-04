package core

import "syscall"

// Command is one parsed request: the name, upper-cased, and its arguments.
type Command struct {
	Cmd  string
	Args []string
}

// FDComm reads and writes a raw socket descriptor, for the event loop, which
// holds descriptors rather than net.Conns.
type FDComm struct {
	Fd int
}

func (f FDComm) Read(p []byte) (int, error) {
	return syscall.Read(f.Fd, p)
}

// Write sends all of data, or reports why it could not.
//
// A bare syscall.Write is not enough for two reasons. It can accept fewer bytes
// than it was given, which on a reply stream means the client receives a
// truncated frame and then misparses everything that follows. And on a
// non-blocking socket it returns EAGAIN once the kernel send buffer is full,
// which under pipelining happens routinely - a run of 20k pipelined 32KB values
// produced 7386 EAGAIN results and 544 short writes - so treating it as "sent"
// silently drops replies.
//
// EAGAIN is handled by briefly putting the socket back into blocking mode for
// the remainder of this write rather than spinning on the syscall, which would
// burn a core and, in a single-threaded event loop, stall every other
// connection. This does mean a slow reader can hold up the loop for the length
// of one reply batch. Removing that entirely needs a per-connection output
// buffer plus write-readiness events from the multiplexer, which is a larger
// change than this fix.
func (f FDComm) Write(data []byte) (int, error) {
	total := 0
	for total < len(data) {
		n, err := syscall.Write(f.Fd, data[total:])
		if n > 0 {
			total += n
		}
		switch err {
		case nil:
			continue
		case syscall.EINTR:
			continue
		case syscall.EAGAIN:
			if berr := f.blockingWrite(data[total:]); berr != nil {
				return total, berr
			}
			return len(data), nil
		default:
			return total, err
		}
	}
	return total, nil
}

// blockingWrite drains the rest of a reply with the socket temporarily in
// blocking mode, restoring non-blocking mode before it returns.
func (f FDComm) blockingWrite(rest []byte) error {
	if err := syscall.SetNonblock(f.Fd, false); err != nil {
		return err
	}
	defer syscall.SetNonblock(f.Fd, true)

	for len(rest) > 0 {
		n, err := syscall.Write(f.Fd, rest)
		if n > 0 {
			rest = rest[n:]
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
