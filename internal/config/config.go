// Package config holds the server's settings: defaults here, overridden by the
// flags in cmd/keel before anything reads them.
//
// Plan step 2.5 (docs/embedding-plan.md) is moving them into options, so that
// each engine has settings of its own: the keyspace's limits are core.Options
// now, and the rest follow.
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

// The append-only file. Off by default, as it is in Redis: it costs a write
// syscall per event-loop cycle and, under FsyncAlways, a disk flush before
// every reply.
var (
	AOFEnabled          = false
	AOFAsyncAppend      = false
	AOFConcurrentAppend = false
	AOFFileName         = "./keel-master.aof"
	AOFFsync            = FsyncEverySec
)

// LegacyAOFFileName is what the default log was called while the server was
// called memkv.
//
// It is still looked for, because the alternative is the worst failure this
// file has: a server started after the rename finds no log at the new default,
// replays nothing, and comes up empty next to a perfectly good log it did not
// look at. Nothing errors and nothing warns - the keyspace is just gone. The
// old name is read if it is there and the new one is not; it is never written.
var LegacyAOFFileName = "./memkv-master.aof"

// Active expiry: how hard the server looks for keys whose TTL has passed
// rather than waiting for something to read them.
//
// The sampling is Redis's. Twenty keys with a TTL are examined; if more than a
// quarter had fallen due, the keyspace probably holds many more and another
// round is drawn. Rounds are capped so one pass cannot become a long stall on a
// keyspace that is mostly expired - what is left over is found on the next
// pass, a tenth of a second later.
//
// Zero samples turns it off, leaving expiry lazy as it was.
var (
	ActiveExpireSamples = 20
	ActiveExpirePercent = 25
	ActiveExpireRounds  = 16
)

// CronIntervalMs is how often the event loop is woken to do work that is due
// because of the clock rather than because a client asked.
//
// The loop blocks in epoll or kqueue with no timeout, so without a poke an idle
// server never runs anything time-based at all - which is precisely the server
// on which unread expired keys pile up. Redis calls its equivalent serverCron
// and runs it at 10Hz by default; this is the same rate for the same reason.
var CronIntervalMs = 100

// When the log is rewritten automatically.
//
// The percentage is measured against the size the log was after the last
// rewrite, which is roughly the size the data needs. Growth past that is
// history: commands superseded by later ones, and keys since deleted. 100 means
// rewrite once the log has doubled, which is Redis's default and the same
// reasoning - half the file being dead weight is worth one pass to be rid of.
//
// The minimum stops a small server rewriting constantly. A 64MB log takes a
// moment to replay and costs nothing to keep, so doubling from 1KB to 2KB is
// not worth a rewrite even though it is 100% growth. Zero percentage turns
// automatic rewriting off; BGREWRITEAOF still works.
var (
	AOFAutoRewritePercentage = 100
	AOFAutoRewriteMinSize    = int64(64 * 1024 * 1024)
)

// How often the log is flushed to disk.
//
//	FsyncAlways   before replying, so an acknowledged write is a durable one
//	FsyncEverySec at most once a second; a crash loses up to a second
//	FsyncNever    when the operating system feels like it
//
// EverySec is the default for the reason Redis chose it: Always turns every
// cycle into a disk round trip, and on a single-threaded server that is a stall
// every other connection shares.
const (
	FsyncAlways   = "always"
	FsyncEverySec = "everysec"
	FsyncNever    = "no"
)

// RequirePass is configured from an environment variable, never a command-line secret.
var RequirePass string

// Experimental replication is opt-in and requires authenticated AOF servers.
var ReplicationFeed bool
var ReplicationProtocol = 1
var ReplicaOf string
var ReplicaPassword string
var ReplicaTLS bool
