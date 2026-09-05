//go:build linux

package io_multiplexing

import (
	"syscall"

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

// Check waits with no timeout: the loop is woken by the descriptors it
// watches, and by a pipe it watches for the purpose.
func (ep *Epoll) Check() ([]Event, error) {
	n, err := syscall.EpollWait(ep.fd, ep.native, -1)
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
