//go:build linux

package io_multiplexing

import (
	"syscall"
	"time"

	"github.com/brandopakel/keel/internal/config"
)

// Epoll is the Linux facility: one epoll instance, and the buffers one wait
// reports into.
type Epoll struct {
	fd     int
	native []syscall.EpollEvent
	ready  []Event
}

// CreateIOMultiplexer opens an epoll instance sized to report up to
// config.MaxConnection descriptors from one wait.
func CreateIOMultiplexer() (*Epoll, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	return &Epoll{
		fd:     fd,
		native: make([]syscall.EpollEvent, min(config.MaxConnection, 128)),
		ready:  make([]Event, 0, min(config.MaxConnection, 128)),
	}, nil
}

func (ep *Epoll) Monitor(event Event) error {
	if event.Op == OpNone {
		err := syscall.EpollCtl(ep.fd, syscall.EPOLL_CTL_DEL, event.Fd, nil)
		if err == syscall.ENOENT {
			return nil
		}
		return err
	}
	native := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(event.Fd)}
	if event.Op == OpWrite {
		native.Events = syscall.EPOLLOUT
	}
	err := syscall.EpollCtl(ep.fd, syscall.EPOLL_CTL_ADD, event.Fd, &native)
	if err == syscall.EEXIST {
		err = syscall.EpollCtl(ep.fd, syscall.EPOLL_CTL_MOD, event.Fd, &native)
	}
	return err
}

// Check waits for a bounded interval rather than indefinitely.
//
// An untimed wait makes the loop depend on being woken for everything it has to
// do, and two things then go wrong. The loop's own periodic work - idle-client
// sweeps and the ordered-append maintenance tick - cannot run on a server with
// no traffic, because nothing is coming to wake it. And a descriptor that is
// left unregistered while a reply is still owed to it strands that client
// permanently: there is no event to wait for, so the loop never turns again.
//
// The second is not hypothetical. A 48-hour soak on b9a97e0 stopped after
// eleven hours with every socket ESTABLISHED, both servers at zero CPU, and the
// event loop parked in this call: a client was waiting for a reply that the
// registration needed to produce it had been removed for. Bounding the wait
// does not remove the underlying mistake, but it turns a permanent hang into
// one interval of extra latency, and lets the loop notice on the next turn.
//
// The cost is a wakeup per interval on an idle server, which is what the
// periodic work needed anyway.
func (ep *Epoll) Check() ([]Event, error) {
	n, err := syscall.EpollWait(ep.fd, ep.native, int(CheckInterval/time.Millisecond))
	if err != nil {
		return nil, err
	}
	ep.ready = ep.ready[:0]
	for _, native := range ep.native[:n] {
		op := OpRead
		if native.Events&syscall.EPOLLOUT != 0 {
			op = OpWrite
		}
		// An error or hang-up is reported whether or not it was asked for,
		// and reads as readable: the read that follows returns the error or
		// zero bytes, which is how the loop learns a peer has gone.
		ep.ready = append(ep.ready, Event{Fd: int(native.Fd), Op: op})
	}
	return ep.ready, nil
}

func (ep *Epoll) Close() error {
	return syscall.Close(ep.fd)
}
