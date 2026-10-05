package core

import (
	"os"
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
// file; the rewrite, which walks e's keyspace and replaces e's log; and what
// e's rewrites came to and when the next may start. The I/O counters are
// package variables until the last part of step 2.3, and replication's until
// step 2.4; the code that owns them reaches the log and the stores through
// defaultEngine until then.
//
// Until callers open engines of their own (plan phase 3) the server and the
// tests run on defaultEngine, as the stores run on data_structure.DefaultSpace.
//
// An Engine is not safe for concurrent use. Like the stores in it, it belongs
// to whoever is executing commands: the event loop's thread today, and the
// holder of the engine's lock once there is one (plan phase 3).
type Engine struct {
	// space is what the engine's stores have in common: the registry eviction
	// draws from, its clock and its limits.
	space *data_structure.Space

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
	// ResetStores clears it with them.
	expiredKeys uint64
	// expireCursor and memoryCursor are where the expiry cycle and memory
	// maintenance resume, as positions in the space's registry, and
	// memoryFirstPhase is the compaction family maintenance starts with next.
	// They survive ResetStores, as they always have: the registry they index
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
	// of the same block; see transaction.go. The rest of replication is still
	// the server's, until plan step 2.4.
	replicationTransaction replicationBlock

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
}

// defaultEngine is the engine the server and the tests run on until each
// caller opens its own; plan step 2.7 removes it. It lives in DefaultSpace, so
// its limits are read from config, as the server's always have been.
//
// The pointer never changes. ResetStores rebuilds the stores inside it, so
// whatever has kept the engine keeps the keyspace a test began from empty.
var defaultEngine = engineIn(data_structure.DefaultSpace)

// engineIn returns an engine living in space, with no stores yet, and with
// its persistence I/O the real thing.
func engineIn(space *data_structure.Space) *Engine {
	return &Engine{
		space: space, replyCeiling: MaxReplyBytes,
		aofWrite: writeLog, aofSync: syncLog,
		rewriteFileWrite: writeLog, rewriteFileSync: syncLog, rewriteOpenLog: openRewrittenLog,
		rewriteRename: os.Rename, rewriteSyncDir: syncDir, keyCountForRewrite: space.TotalKeys,
		// No rewrite has ended yet, which Redis reports as -1.
		rewriteOutcome: rewriteOutcomeState{lastSeconds: -1},
	}
}
