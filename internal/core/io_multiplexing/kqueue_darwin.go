//go:build darwin

package io_multiplexing

import (
	"syscall"

	"github.com/brandopakel/keel/internal/config"
)

// KQueue is the darwin facility: one kernel queue, and the buffers one wait
// reports into.
type KQueue struct {
	fd     int
	native []syscall.Kevent_t
	ready  []Event
}

// CreateIOMultiplexer opens a kernel queue sized to report up to
// config.MaxConnection descriptors from one wait.
func CreateIOMultiplexer() (*KQueue, error) {
	fd, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	return &KQueue{
		fd:     fd,
		native: make([]syscall.Kevent_t, min(config.MaxConnection, 128)),
		ready:  make([]Event, 0, min(config.MaxConnection, 128)),
	}, nil
}

func (kq *KQueue) Monitor(event Event) error {
	if event.Op == OpNone {
		for _, filter := range []int16{syscall.EVFILT_READ, syscall.EVFILT_WRITE} {
			_, err := syscall.Kevent(kq.fd, []syscall.Kevent_t{{Ident: uint64(event.Fd), Filter: filter, Flags: syscall.EV_DELETE}}, nil, nil)
			if err != nil && err != syscall.ENOENT {
				return err
			}
		}
		return nil
	}
	filter := int16(syscall.EVFILT_READ)
	if event.Op == OpWrite {
		filter = syscall.EVFILT_WRITE
	}
	other := int16(syscall.EVFILT_WRITE)
	if filter == syscall.EVFILT_WRITE {
		other = syscall.EVFILT_READ
	}
	_, _ = syscall.Kevent(kq.fd, []syscall.Kevent_t{{Ident: uint64(event.Fd), Filter: other, Flags: syscall.EV_DELETE}}, nil, nil)
	change := syscall.Kevent_t{
		Ident:  uint64(event.Fd),
		Filter: filter,
		Flags:  syscall.EV_ADD | syscall.EV_ENABLE,
	}
	// A kevent call with changes and no room for results applies the changes
	// and returns; the registration is the whole of the work.
	_, err := syscall.Kevent(kq.fd, []syscall.Kevent_t{change}, nil, nil)
	return err
}

// Check waits with no timeout: the loop is woken by the descriptors it
// watches, and by a pipe it watches for the purpose.
func (kq *KQueue) Check() ([]Event, error) {
	n, err := syscall.Kevent(kq.fd, nil, kq.native, nil)
	if err != nil {
		return nil, err
	}
	kq.ready = kq.ready[:0]
	for _, native := range kq.native[:n] {
		op := OpRead
		if native.Filter == syscall.EVFILT_WRITE {
			op = OpWrite
		}
		// A peer that hangs up is reported on the read filter with EV_EOF
		// set, and reads as readable: the read that follows returns zero
		// bytes, which is how the loop learns it has gone.
		kq.ready = append(kq.ready, Event{Fd: int(native.Ident), Op: op})
	}
	return kq.ready, nil
}

func (kq *KQueue) Close() error {
	return syscall.Close(kq.fd)
}
