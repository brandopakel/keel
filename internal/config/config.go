// Package config holds the server's settings: defaults here, overridden by the
// flags in cmd/keel before anything reads them.
//
// Plan step 2.5 (docs/embedding-plan.md) is moving them into options, so that
// each engine has settings of its own: the keyspace's limits are core.Options
// now, and so are active expiry, the log's settings and the replication role.
// What is left here is the listener's and the transport's, which become the
// server's options.
package config

// Where the server listens, and how many connections it will hold.
var (
	// Host is the interface to bind. The event loop binds IPv4 only, so this
	// has to be an IPv4 address; 0.0.0.0 is every interface, which is the
	// Redis default and, like Redis, a reason to keep the server behind a
	// firewall. Localhost is the safe standalone default.
	Host = "127.0.0.1"
	Port = 8081
	// MaxConnection is the listen backlog, and the most descriptors one turn
	// of the event loop can be handed at once.
	MaxConnection = 20000
)

// IOThreads is how many threads read, parse and write sockets, counting the
// event loop's own thread as one of them. 1 keeps everything on the loop, which
// is the default and what Redis defaults to as well.
//
// Command execution is never threaded whatever this is set to. That is the
// whole design: one thread touching the stores is what lets them be plain maps
// with no locking, and it is not worth trading for throughput.
var IOThreads = 1

// CronIntervalMs is how often the event loop is woken to do work that is due
// because of the clock rather than because a client asked.
//
// The loop blocks in epoll or kqueue with no timeout, so without a poke an idle
// server never runs anything time-based at all - which is precisely the server
// on which unread expired keys pile up. Redis calls its equivalent serverCron
// and runs it at 10Hz by default; this is the same rate for the same reason.
var CronIntervalMs = 100

// AOFConcurrentAppend overlaps bounded string commands with the log's worker
// appends, which the event loop orders (server/aof_ordered.go). It requires
// the engine to append on a worker (core.Options.AsyncAppend).
var AOFConcurrentAppend = false

// RequirePass is configured from an environment variable, never a command-line secret.
var RequirePass string

// How a replica reaches its primary: the password it authenticates with,
// read from the environment variable -primary-password-env names, and
// whether it verifies TLS. Which primary it follows is the engine's
// (core.Options.ReplicaOf).
var ReplicaPassword string
var ReplicaTLS bool
