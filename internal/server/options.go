package server

import "time"

// Options are the server's settings: where it listens, how many connections it
// holds, and how its transport runs. They replace the package variables of
// internal/config (plan step 2.5); cmd/keel maps its flags onto them and
// passes them to RunAsyncTCPServer or RunNetTCPServer. The engine the server
// drives has options of its own, core.Options, which cmd/keel gives it with
// core.Configure before the server starts.
//
// As with core.Options, the zero value of every field is its default.
type Options struct {
	// Host is the interface to bind. The event loop binds IPv4 only, so this
	// has to be an IPv4 address; 0.0.0.0 is every interface, which is the
	// Redis default and, like Redis, a reason to keep the server behind a
	// firewall. Zero is 127.0.0.1, the safe standalone default.
	Host string

	// Port is the TCP port to listen on. Zero is 8081.
	Port int

	// MaxClients is the most connections the server holds at once. It is also
	// the listen backlog, and bounds how many descriptors one turn of the
	// event loop can be handed. Zero is 20,000.
	MaxClients int

	// IOThreads is how many threads read, parse and write sockets, counting
	// the event loop's own thread as one of them. Zero is 1, which keeps
	// everything on the loop and is what Redis defaults to as well.
	//
	// Command execution is never threaded whatever this is set to. That is
	// the whole design: one thread touching the stores is what lets them be
	// plain maps with no locking, and it is not worth trading for throughput.
	IOThreads int

	// CronInterval is how often the event loop is woken to do work that is
	// due because of the clock rather than because a client asked. Zero is
	// 100 milliseconds.
	//
	// The loop blocks in epoll or kqueue with no timeout, so without a poke
	// an idle server never runs anything time-based at all - which is
	// precisely the server on which unread expired keys pile up. Redis calls
	// its equivalent serverCron and runs it at 10Hz by default; this is the
	// same rate for the same reason.
	CronInterval time.Duration

	// RequirePass is the password the default user authenticates with.
	// Empty, every connection is authenticated, as with Redis's nopass default
	// user. cmd/keel reads it from an environment variable, never from the
	// command line.
	RequirePass string

	// ConcurrentAppend overlaps bounded string commands with the engine's
	// worker appends, which the event loop orders (aof_ordered.go). It needs
	// the engine to append on a worker (core.Options.AsyncAppend).
	ConcurrentAppend bool

	// PrimaryPassword is the password a replica authenticates to its primary
	// with, and PrimaryTLS whether it verifies TLS to it. Which primary it
	// follows is the engine's (core.Options.ReplicaOf).
	PrimaryPassword string
	PrimaryTLS      bool
}

// The defaults of the fields whose zero is not taken as written.
const (
	defaultHost         = "127.0.0.1"
	defaultPort         = 8081
	defaultMaxClients   = 20000
	defaultIOThreads    = 1
	defaultCronInterval = 100 * time.Millisecond
)

// WithDefaults returns o with every field that is zero, and has a default, set
// to that default. A count or interval below zero, which no setting means,
// takes the default too.
func (o Options) WithDefaults() Options {
	if o.Host == "" {
		o.Host = defaultHost
	}
	if o.Port <= 0 {
		o.Port = defaultPort
	}
	if o.MaxClients <= 0 {
		o.MaxClients = defaultMaxClients
	}
	if o.IOThreads <= 0 {
		o.IOThreads = defaultIOThreads
	}
	if o.CronInterval <= 0 {
		o.CronInterval = defaultCronInterval
	}
	return o
}
