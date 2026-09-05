// Package io_multiplexing waits on many socket descriptors at once, over
// whichever facility the operating system offers: epoll on Linux, kqueue on
// darwin. The event loop asks it which descriptors are ready and touches only
// those, which is what lets one thread serve thousands of connections without
// blocking on any of them.
package io_multiplexing

// Operation is the readiness a caller wants to hear about.
type Operation uint32

const (
	// OpRead means the descriptor has bytes to read, or has been closed by
	// the peer, which reads as zero bytes.
	OpRead Operation = iota
	// OpWrite means the descriptor can take bytes without blocking.
	OpWrite
	// OpNone removes readiness registration until Monitor re-enables it.
	OpNone
)

// Event pairs a descriptor with what it is ready for.
type Event struct {
	Fd int
	Op Operation
}

// IOMultiplexer is one of the platform facilities behind a common face.
type IOMultiplexer interface {
	// Monitor asks to be told when the descriptor is ready for the
	// operation. Registration is level-triggered: a descriptor stays
	// reported for as long as it stays ready.
	Monitor(event Event) error
	// Check blocks until at least one monitored descriptor is ready and
	// returns those that are. The slice belongs to the multiplexer and is
	// overwritten by the next call, so a caller keeps nothing from it.
	Check() ([]Event, error)
	Close() error
}
