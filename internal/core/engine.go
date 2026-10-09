package core

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Engine is one keyspace and the state that serves it.
//
// Everything this package keeps between commands used to be a package
// variable, which made the server a singleton: one keyspace per process, and no
// two tests able to run side by side. The embedding plan
// (docs/embedding-plan.md) gives all of it an owner, so that a process can hold
// several independent instances and the server becomes one caller of the
// engine among others. Step 2.1 moved the stores here, with the expiry and
// memory-maintenance cursors over them. Every command handler is an Engine
// method, which reads the stores, and asks the space who holds a key, on the
// engine dispatching it - see commandTable. Step 2.2 moved the command scope
// here: what the engine holds for the command it is running, the reply's
// protocol included, and the budget the transport running it reserves from.
// Step 2.3 moves the persistence state here, a part at a time: the log and its
// append worker, whose methods record, flush and replay e's writes in e's own
// file; the rewrite, which walks e's keyspace and replaces e's log; what e's
// rewrites came to and when the next may start; and the counters that time
// e's I/O. Step 2.4 moved replication and failover here: the replica, which
// applies its primary's stream to e's keyspace and e's log; the primary's
// stream, which carries e's writes to e's replicas, with the role that says
// which of them e is; and the term that says whether e may write. Step 2.6
// gave every test an engine of its own, and step 2.7 the server: cmd/keel
// makes it with NewEngine and hands it to the server, and no engine is the
// package's.
//
// An Engine belongs to whoever holds its lock (Lock): whoever holds it may
// run commands on it and drive its work, and nobody else may touch it. No
// method takes the lock itself - a command runs under its caller's - so a
// command's path is the same with or without it. The server's event loop holds
// it for every cycle, and lets go only while it waits for something to be
// ready (plan phase 3). Until an engine is handed to whatever drives it, the
// goroutine that made it is its only user, and needs no lock: that is how the
// server's log is replayed and opened before the loop starts.
type Engine struct {
	// space is what the engine's stores have in common: the registry eviction
	// draws from, its clock and its limits.
	space *data_structure.Space

	// options are e's settings, as they were given to it; see options.go.
	// What the code reads is resolved from them when they are given, and held
	// where it is read: the keyspace's limits in the space, the replication
	// role in role, and expiry's and the log's in settings.
	options Options

	// The stores, in the order resetStores registers them, under the names
	// they had as package variables.
	dictStore   *data_structure.Dict
	zsetStore   *data_structure.Keyed[*data_structure.ZSet]
	setStore    *data_structure.Keyed[*data_structure.Set]
	hashStore   *data_structure.Keyed[*data_structure.Hash]
	listStore   *data_structure.Keyed[*data_structure.List]
	sbStore     *data_structure.Keyed[*data_structure.SBChain]
	cmsStore    *data_structure.Keyed[*data_structure.CMS]
	morrisStore *data_structure.Keyed[*data_structure.Morris]
	hllStore    *data_structure.Keyed[*data_structure.HLL]
	cfStore     *data_structure.Keyed[*data_structure.CuckooFilter]

	// expiredKeys counts what active expiry has reclaimed, for INFO. Keys
	// reaped lazily by a read are not counted here, because the number is here
	// to answer whether the cycle is keeping up. It describes the stores, so
	// resetStores clears it with them.
	expiredKeys uint64
	// expireCursor and memoryCursor are where the expiry cycle and memory
	// maintenance resume, as positions in the space's registry, and
	// memoryFirstPhase is the compaction family maintenance starts with next.
	// They survive resetStores, as they always have: the registry they index
	// is rebuilt in the same order.
	expireCursor     int
	memoryCursor     int
	memoryFirstPhase int

	// The command scope: what the engine holds for the command running on it,
	// under the names it had as package variables. One command runs on an
	// engine at a time, so one of each is enough, as one of each was for the
	// process while there was one engine.
	//
	// framing is the protocol the running command's reply is built in, held
	// for exactly that command. Its methods are promoted, so a handler writes
	// e.nullReply() or e.encode(...), and reads e.replyRESP3 - see resp3.go.
	framing
	// runningName is the name GEOSEARCH, the one command whose errors repeat
	// the name it was sent as, was last sent as. It is set only for that
	// command, before it runs, and read only while it runs.
	runningName string
	// replyCeiling is the most one reply may come to. It is the output limit,
	// except while EXEC runs: a transaction answers with one array holding
	// every reply, so each command it runs may use only what the replies
	// before it left.
	replyCeiling int
	// commandAllocations is the transport's budget, which a run of commands
	// reserves its large replies and workspaces from before building them. The
	// server installs it on the engine it drives (SetCommandAllocations). An
	// engine with none - one no transport drives, and log replay before the
	// server starts - keeps each command's own limits and reserves nothing.
	commandAllocations *CommandAllocationBudget

	// The log: the append-only file this engine records its writes in, and
	// the worker that appends a batch of them, under the names they had as
	// package variables - see aof.go and aof_async.go. Each engine's log is
	// its own, so two engines in one process write two logs, and a test that
	// fails one engine's disk fails no other's.
	//
	// aof is the file, its buffer and staging, and its sync and size state.
	aof aofState
	// appendPending is the worker's result while a batch is out, and
	// appendBytes and appendRetained that batch's length and capacity.
	appendPending  chan appendResult
	appendBytes    int
	appendRetained int
	// The log's logical offsets since it was opened: appendStarted has been
	// handed to a write, appendWritten written, appendSynced synced, and
	// appendCompleted may have its replies released.
	appendStarted, appendCompleted uint64
	appendWritten, appendSynced    uint64
	// aofWrite and aofSync are the log's I/O: writeLog and syncLog, unless a
	// test has replaced them.
	aofWrite func(*os.File, []byte) (int, error)
	aofSync  func(*os.File) error
	// replicationTransaction is the protocol 2 framing of the transaction
	// running on the engine, which EXEC opens and closes with the log's frame
	// of the same block; see transaction.go.
	replicationTransaction replicationBlock

	// The replica: what e has applied of its primary's stream, under the
	// names it had as package variables - see replication.go,
	// replication_v2_apply.go and replica_checkpoint.go. A replica applies
	// its primary's frames to e's keyspace and e's log, and resumes from the
	// checkpoint beside e's log, so two replicas in one process follow two
	// primaries.
	//
	// replicaApplying is set while e runs commands its primary decided, which
	// are not client writes: a replica's refusal, the stream's publication
	// and RESP3 framing all stand aside for them, as for log replay.
	replicaApplying bool
	// replicaReady says e's keyspace is a prefix of its primary's stream that
	// may be read, as of replicaUpdated; replicaEpoch and replicaOffset are
	// where in the stream that prefix ends.
	replicaReady   bool
	replicaEpoch   string
	replicaOffset  uint64
	replicaUpdated time.Time
	// replicaV2 is what a protocol 2 replica holds between frames.
	replicaV2 replicaV2State
	// The checkpoint's I/O, which a test replaces on the engine it is
	// failing: syncFile, os.Rename and syncDir, unless it has.
	checkpointSync    func(*os.File) error
	checkpointRename  func(oldPath, newPath string) error
	checkpointSyncDir func(string) error

	// The primary: the stream e feeds its replicas, under the names it had
	// as package variables - see replication.go, replication_v2.go and
	// replication_ack.go. It carries e's writes alone, in an epoch of e's
	// own, and a snapshot is a rewrite of e's log.
	//
	// replication is protocol 1's stream and what protocol 2's shares with
	// it, the epoch and the keys the running command changed;
	// replicationV2 is protocol 2's history and snapshot; and replicaAck is
	// the furthest cursor e's replicas have reported.
	replication   replicationState
	replicationV2 replicationV2State
	replicaAck    replicaAckState

	// failover is e's term, which says whether e may write, and the file
	// beside e's log that keeps it - see failover.go. termSync, termRename
	// and termSyncDir are that file's I/O, which a test replaces on the
	// engine it is failing: syncFile, os.Rename and syncDir, unless it has.
	failover    failoverState
	termSync    func(*os.File) error
	termRename  func(oldPath, newPath string) error
	termSyncDir func(string) error

	// role is what e is in replication - a replica of a primary, a primary
	// feeding replicas, or neither, and in which protocol - as its options
	// say, read through replicaOf, feedsReplicas and replicationProtocol.
	role replicationRole

	// The rewrite, under the names it had as package variables - see
	// aof_rewrite.go and aof_rewrite_io.go. It walks e's keyspace and replaces
	// e's log, and nothing else.
	//
	// rewrite is the walk in progress, if there is one, and pendingRewriteIO
	// the write or sync of its file a worker owns.
	rewrite          rewriteState
	pendingRewriteIO *rewriteIOJob
	// rewriteWake is how a worker that finishes wakes the loop driving e,
	// which installs it with SetRewriteWaker.
	rewriteWake func()
	// The rewrite's I/O and the steps of its handoff, which a test replaces on
	// the engine it is failing: writeLog, syncLog, openRewrittenLog, os.Rename
	// and syncDir, unless it has. keyCountForRewrite is the space's key count,
	// unless a test needs the ceiling without building a keyspace that large.
	rewriteFileWrite   func(*os.File, []byte) (int, error)
	rewriteFileSync    func(*os.File) error
	rewriteOpenLog     func(string) (*os.File, error)
	rewriteRename      func(oldPath, newPath string) error
	rewriteSyncDir     func(string) error
	keyCountForRewrite func() int

	// What outlives one rewrite of e's log - see aof_rewrite_status.go - under
	// the names it had as package variables, so one engine's failed rewrites,
	// the waits they earn and a rewrite scheduled inside its EXEC are its own.
	// rewriteOutcome is what INFO reports about the rewrites so far, the
	// retry limit's state and a scheduled BGREWRITEAOF; nextAutoRewrite is
	// when an automatic one may start, and rewriteBudgetAborts how many were
	// abandoned on their budgets. snapshotRetryAt holds back the rewrite a
	// protocol 2 pull would start after one whose snapshot could not be
	// opened. unsyncedLogDir names the directory whose entry for e's log is
	// not yet known to be durable, because the sync after a rewrite's rename
	// failed.
	rewriteOutcome      rewriteOutcomeState
	nextAutoRewrite     time.Time
	rewriteBudgetAborts uint64
	snapshotRetryAt     time.Time
	unsyncedLogDir      string

	// The I/O counters INFO persistence reports, for e's log and e's rewrite;
	// see persistence_io_stats.go.
	appendWriteStats, appendSyncStats           persistenceIOStats
	rewriteWriteStats, rewriteSyncStats         persistenceIOStats
	rewriteFinalSyncStats, rewriteFinalizeStats persistenceIOStats

	// settings are e's options as the code that is not the space's or the
	// role's reads them: expiry's and the log's, resolved when the options
	// are given. They are read once a cycle, not once a command, so they
	// come last.
	settings settings

	// What INFO server and memory report of e's life: when e was made, the
	// random run_id it was given then, and the most memory its stores have
	// held, with when, sampled once a turn of the event loop
	// (NoteMemoryPeak) and whenever INFO runs. Nothing a command does reads
	// them, so they go after settings.
	started      time.Time
	runID        string
	memoryPeak   uint64
	memoryPeakAt time.Time

	// clientBuffers is how INFO reads the connections of the transport
	// driving e, which installs it with SetClientBuffers; nil, INFO reports
	// none. Only INFO reads it, so it goes after everything a command reads.
	clientBuffers func() ClientBufferStats
	// serverInfo is what INFO reports of that transport's own settings, which
	// it installs with SetServerInfo; nil, INFO leaves those fields out.
	serverInfo *ServerInfo

	// mu is e's lock, which Lock and Unlock take and release. No command
	// reads it, so it goes last, and every field a command reads keeps its
	// offset.
	mu sync.Mutex
	// closed is set by Close, under the lock; see open.go.
	closed bool
	// logLock is the lock beside e's log that StartAOF took, which Close
	// releases; nil, e holds none. See loglock.go.
	logLock *logLock
	// driver is e's maintenance goroutine, which Open starts and Close stops;
	// nil when something else drives e, as the server's loop does. It is set
	// before e is shared and never again, so it is read without the lock.
	driver *driver
	// aofTruncate cuts the log back to a size, removing a short write: a
	// file's Truncate, unless a test has replaced it. Last, so that adding it
	// moved no field before it.
	aofTruncate func(*os.File, int64) error
	// logFailure is the log's write and sync statuses while a failure of
	// either is retried (log_failures.go). Last, so that adding it moved no
	// field before it.
	logFailure logFailures
}

// Lock takes e's lock, waiting until whoever holds it lets go. Its holder is
// the one goroutine that may run commands on e and drive e's work: the
// expiry cycle, the log's flushes, the rewrite's slices, memory maintenance
// and replica apply. The disk workers e starts own only the bytes and the
// file they were handed, and hand their results back through channels that
// the holder polls.
func (e *Engine) Lock() { e.mu.Lock() }

// Unlock releases e's lock.
func (e *Engine) Unlock() { e.mu.Unlock() }

// engineIn returns an engine living in space, with no stores yet, and with
// its persistence I/O the real thing.
func engineIn(space *data_structure.Space) *Engine {
	now := time.Now()
	e := &Engine{
		space: space, replyCeiling: MaxReplyBytes,
		aofWrite: writeLog, aofSync: syncLog, aofTruncate: truncateLog,
		rewriteFileWrite: writeLog, rewriteFileSync: syncLog, rewriteOpenLog: openRewrittenLog,
		rewriteRename: os.Rename, rewriteSyncDir: syncDir, keyCountForRewrite: space.TotalKeys,
		checkpointSync: syncFile, checkpointRename: os.Rename, checkpointSyncDir: syncDir,
		termSync: syncFile, termRename: os.Rename, termSyncDir: syncDir,
		// No rewrite has ended yet, which Redis reports as -1.
		rewriteOutcome: rewriteOutcomeState{lastSeconds: -1},
		// The default options' settings, and their role: neither a replica
		// nor a feed, in protocol 1.
		settings: Options{}.settings(), role: Options{}.role(),
		// Never nil, so noteReplicationDirty need not test it on every write.
		replication: replicationState{dirty: map[string]struct{}{}},
		started:     now, runID: newRunID(), memoryPeakAt: now,
	}
	return e
}

// newRunID is a run_id as Redis makes one at startup: 40 random hexadecimal
// characters, which tell one run of a server from the next.
func newRunID() string {
	var b [20]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
