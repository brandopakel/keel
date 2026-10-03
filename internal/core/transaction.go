package core

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Transactions: MULTI, EXEC and DISCARD.
//
// A connection that sends MULTI stops executing what it sends and queues it
// instead, answering QUEUED for each command, until EXEC runs the queue in one
// go or DISCARD throws it away. That much is what every client library's
// transaction API expects. The rest of Redis's contract is kept as Redis words
// it, because clients match on the error strings:
//
//   - A command refused while it is being queued - unknown, given the wrong
//     number of arguments, not allowed in a transaction, or refused by a
//     replica - is answered with that error and dooms the transaction. EXEC
//     then answers EXECABORT and runs nothing.
//   - A command that fails while EXEC runs it is an error inside EXEC's reply,
//     and the commands after it still run. There is no rollback, in Redis or
//     here.
//   - MULTI inside MULTI is an error that leaves the transaction open. EXEC or
//     DISCARD outside one is an error.
//
// # Atomic because execution is serial
//
// EXEC is a single command to the event loop, and it runs every queued command
// before it returns, on the loop's own thread. Nothing another client sends can
// run between two of them: I/O threads only read, parse and write, and the
// concurrent-append mode lets a run overlap a pending append only when it can
// bound every command in the run, which it cannot do for EXEC - so EXEC waits
// at the drained barrier like every other command it does not model.
//
// # Atomic on disk and on a replica
//
// Serial execution keeps other clients out. It does nothing about a crash part
// way through writing the block, or a replica that has received half of one.
// So the block is framed: MULTI before its first record and EXEC after its
// last, in the log and in the protocol 2 stream, which is what Redis writes to
// its AOF too. Loading the log replays a block only once its EXEC has been
// read, and a crash that leaves one open is a torn tail like any other, cut back
// to before its MULTI. A replica holds a block back until its EXEC arrives and
// then applies it in one turn of its loop. A transaction that recorded nothing
// - one made only of reads - writes no frame at all, because the log grows with
// changes, not with traffic.
//
// WATCH is not implemented and stays an unknown command, so a client's
// optimistic-locking API fails loudly instead of quietly not watching. Inside
// MULTI it gets Redis's own refusal, which leaves the transaction open. UNWATCH
// answers as Redis answers it when nothing is watched, which is always here.

// maxTransactionBytes bounds what one connection may hold queued, measured the
// way parsed input is measured for the buffer limits. It is the per-client
// limit on incomplete input as well: a queued command is input that has arrived
// and not yet run. A command that would pass it is refused like any other
// command refused while queueing, which aborts the transaction, so a client
// that keeps sending learns why at EXEC rather than growing the queue.
//
// It also keeps a transaction's replication block well inside the history a
// replica could receive it from; see replicationTransactionBytes.
const maxTransactionBytes = 16 << 20

// The replies clients match on, worded as Redis words them.
var (
	queuedReply    = []byte("+QUEUED\r\n")
	execAbortReply = []byte("-EXECABORT Transaction discarded because of previous errors.\r\n")

	errNestedMulti         = errors.New("ERR MULTI calls can not be nested")
	errExecNoMulti         = errors.New("ERR EXEC without MULTI")
	errDiscardNoMulti      = errors.New("ERR DISCARD without MULTI")
	errNotInTransaction    = errors.New("ERR Command not allowed inside a transaction")
	errWatchInMulti        = errors.New("ERR WATCH inside MULTI is not allowed")
	errTransactionTooLarge = fmt.Errorf("ERR transaction exceeds the %d MiB queued command limit", maxTransactionBytes>>20)
	errNoReply             = errors.New("ERR command produced no reply")
	// ErrTransactionReplyTooLarge is returned to the transport, not to the
	// client: the transaction has run, and its reply cannot be delivered.
	ErrTransactionReplyTooLarge = errors.New("output buffer limit exceeded")
)

// notInTransaction are commands with no meaning as part of a transaction, and
// refused inside one with Redis's error for that. A replication pull serves the
// stream as it stands between commands, can start a snapshot rewrite and can
// fence the node, and Redis refuses its own replication commands, SYNC and
// PSYNC, inside MULTI the same way. Changing terms has no Redis counterpart:
// it acts on the node rather than the data, and is made durable outside the
// log. Everything else Redis queues is queued here, BGREWRITEAOF and AUTH
// included.
var notInTransaction = map[string]bool{
	"KEEL.PROMOTE": true, "KEEL.FENCE": true,
	"KEEL.REPL.PULL": true, "KEEL.REPL.PULL2": true,
}

// commandArity is how many words, the name included, each command accepts, as
// Redis counts them: n means exactly n, -n means at least n. A transaction
// needs it because Redis refuses a command with the wrong number of arguments
// while queueing it, which aborts the transaction, rather than when EXEC runs
// it - and the two outcomes differ to a client.
//
// The counts are Redis's for Redis's commands and the handler's own for the
// rest. Every handler still checks its arguments, so a count looser than its
// handler only moves the error into EXEC's reply; a count stricter than its
// handler would refuse a valid command, which TestCommandArityIsNeverStricterThanTheHandler
// rules out. Every command in commandTable needs an entry here, apart from the
// ones refused in a transaction anyway.
var commandArity = map[string]int{
	"PING": -1, "ECHO": 2, "SELECT": 2, "UNWATCH": 1,

	"SET": -3, "SETNX": 3, "GET": 2, "INCR": 2, "INCRBY": 3, "DECR": 2, "DECRBY": 3, "MGET": -2, "MSET": -3,
	"SETEX": 4, "PSETEX": 4,
	"LCS": -3,

	"DEL": -2, "UNLINK": -2, "EXISTS": -2, "TYPE": 2, "KEYS": 2, "SCAN": -2,
	"TTL": 2, "PTTL": 2, "EXPIRE": -3, "PEXPIREAT": -3,
	"PEXPIRE": -3, "EXPIREAT": -3, "PERSIST": 2,

	"DBSIZE": 1, "FLUSHDB": -1, "MEMORY": -2, "INFO": -1, "BGREWRITEAOF": 1,
	"KEEL.DUMP": 2, "KEEL.RESTORE": 3, "MEMKV.DUMP": 2, "MEMKV.RESTORE": 3,

	"HSET": -4, "HSETNX": 4, "HGET": 3, "HMGET": -3,
	"HDEL": -3, "HEXISTS": 3, "HLEN": 2, "HKEYS": 2,
	"HVALS": 2, "HGETALL": 2, "HINCRBY": 4,

	"LPUSH": -3, "RPUSH": -3, "LPOP": -2, "RPOP": -2,
	"LTRIM": 4, "LLEN": 2, "LINDEX": 3, "LSET": 4, "LRANGE": 4,

	"SADD": -3, "SREM": -3, "SCARD": 2, "SMEMBERS": 2,
	"SISMEMBER": 3, "SMISMEMBER": -3, "SPOP": -2,
	"SRANDMEMBER": -2, "SRAND": -2,

	"ZCOUNT": 4, "ZRANGEBYSCORE": -4, "ZREVRANGEBYSCORE": -4,
	"ZINCRBY": 4, "ZPOPMIN": -2, "ZPOPMAX": -2,
	"ZRANGE": -4, "ZADD": -4, "ZRANK": -3, "ZREM": -3, "ZSCORE": 3, "ZCARD": 2,
	"GEOADD": -5, "GEODIST": -4, "GEOHASH": -2,
	"GEOSEARCH": -7, "GEOPOS": -2,

	"BF.RESERVE": -4, "BF.INFO": 2, "BF.ADD": 3,
	"BF.MADD": -3, "BF.EXISTS": 3, "BF.MEXISTS": -3,
	"CMS.INITBYDIM": 4, "CMS.INITBYPROB": 4,
	"CMS.INCRBY": -4, "CMS.QUERY": -3,
	"MORRIS.INITBYDIM": 4, "MORRIS.INITBYPROB": 4,
	"MORRIS.INCRBY": -4, "MORRIS.QUERY": -3, "MORRIS.INFO": 2,
	"PFADD": -2, "PFCOUNT": -2, "PFMERGE": -2,
	"CF.RESERVE": 3, "CF.ADD": 3, "CF.ADDNX": 3,
	"CF.EXISTS": 3, "CF.MEXISTS": -3, "CF.DEL": 3,
	"CF.COUNT": 3, "CF.INFO": 2,
}

func arityAccepts(arity, args int) bool {
	if arity >= 0 {
		return args+1 == arity
	}
	return args+1 >= -arity
}

// wrongArguments is Redis's refusal, which names the command in lower case.
// cmdUNWATCH forgets every watched key. Nothing is ever watched, so it only
// answers, as Redis answers it then. Clients send it when they finish with a
// connection that may have watched something: go-redis's Tx.Close and
// redis-py's pipeline reset both do.
func cmdUNWATCH(args []string) []byte {
	if len(args) != 0 {
		return Encode(wrongArguments("UNWATCH"), false)
	}
	return constant.RespOk
}

func wrongArguments(name string) error {
	return fmt.Errorf("ERR wrong number of arguments for '%s' command", strings.ToLower(name))
}

// Connection is the transport's part in a transaction: the commands it answers
// itself, about the connection rather than the data, which the command table
// does not hold. Redis queues AUTH and its kind like any other command and runs
// each in its place at EXEC, and so does this.
type Connection interface {
	// ConnectionArity reports the arity of a command the transport answers
	// itself, and false for any other command.
	ConnectionArity(name string) (int, bool)
	// AnswerConnection runs one such command and writes its reply.
	AnswerConnection(cmd *Command, w io.ReadWriter)
	// RESP3 reports whether the connection speaks RESP3 at this moment, which
	// a HELLO run earlier in the same EXEC may have just changed.
	RESP3() bool
}

// answers reports whether conn, which may be nil, answers name itself.
func answers(conn Connection, name string) (int, bool) {
	if conn == nil {
		return 0, false
	}
	return conn.ConnectionArity(name)
}

// Transaction is one connection's open MULTI. The connection keeps a pointer
// to it that is nil the rest of the time, so a connection outside a
// transaction carries nothing for it.
type Transaction struct {
	commands []*Command
	bytes    int
	// aborted is set once a command has been refused while queueing. Nothing
	// queued after that will ever run, so nothing more is kept.
	aborted bool
}

// RetainedBytes is what the queue holds, for the transport's buffer accounting.
func (tx *Transaction) RetainedBytes() int {
	if tx == nil {
		return 0
	}
	return tx.bytes
}

// queueSlotBytes is charged per queued command for its place in the queue: a
// pointer, and as much again for the slack a growing slice keeps.
const queueSlotBytes = 16

// CommandRetainedBytes estimates what one parsed command holds: the struct,
// its argument headers and their bytes. Parsed input and queued commands are
// charged by the same estimate, so moving a command from one to the other
// neither creates nor hides memory.
func CommandRetainedBytes(cmd *Command) int {
	used := 64 + len(cmd.Cmd) + cap(cmd.Args)*16
	for _, arg := range cmd.Args {
		used += len(arg)
	}
	return used
}

// IsTransactionCommand reports whether name is MULTI, EXEC or DISCARD, which a
// connection must hand to Transact whether or not it has a transaction open.
func IsTransactionCommand(name string) bool {
	return name == "MULTI" || name == "EXEC" || name == "DISCARD"
}

// Transact answers cmd for a connection whose open transaction is tx, nil if it
// has none, and returns the transaction that is open afterwards. The transport
// calls it for every command while one is open, and for MULTI, EXEC and
// DISCARD always; conn, which may be nil, answers the commands it handles
// itself when EXEC reaches them.
//
// Like EvalAndResponse, the error is the connection's rather than the
// command's: ErrTransactionReplyTooLarge means a transaction ran and its reply
// cannot be delivered, so the connection has to be closed, which is what any
// reply over the output limit already costs.
func Transact(tx *Transaction, cmd *Command, w io.ReadWriter, conn Connection) (*Transaction, error) {
	if !IsTransactionCommand(cmd.Cmd) {
		if tx == nil {
			return nil, EvalAndResponse(cmd, w)
		}
		if cmd.Cmd == "WATCH" {
			// WATCH is not implemented, and outside a transaction it is an
			// unknown command. Inside one Redis refuses it without aborting
			// the transaction, after counting its arguments as it counts any
			// command's, and clients get the same answers here.
			reply := Encode(errWatchInMulti, false)
			if len(cmd.Args) == 0 {
				reply = tx.refuse(wrongArguments(cmd.Cmd))
			}
			_, err := w.Write(reply)
			return tx, err
		}
		_, err := w.Write(tx.queue(cmd, conn))
		return tx, err
	}
	if len(cmd.Args) != 0 {
		// Redis counts arguments before it looks at what a command does, so a
		// malformed MULTI, EXEC or DISCARD is refused as any command would be:
		// inside a transaction that aborts it, and a malformed EXEC discards it
		// there and then, naming the reason.
		refused := wrongArguments(cmd.Cmd)
		if tx != nil && cmd.Cmd == "EXEC" {
			return nil, writeExecAbort(w, refused)
		}
		tx.abort()
		_, err := w.Write(Encode(refused, false))
		return tx, err
	}
	var reply []byte
	switch {
	case cmd.Cmd == "MULTI" && tx != nil:
		reply = Encode(errNestedMulti, false)
	case cmd.Cmd == "MULTI":
		tx, reply = &Transaction{}, constant.RespOk
	case tx == nil:
		reply = Encode(errExecNoMulti, false)
		if cmd.Cmd == "DISCARD" {
			reply = Encode(errDiscardNoMulti, false)
		}
	case cmd.Cmd == "DISCARD":
		tx, reply = nil, constant.RespOk
	default:
		return nil, tx.exec(w, conn)
	}
	_, err := w.Write(reply)
	return tx, err
}

// refuse answers a command refused while queueing, which aborts the transaction.
func (tx *Transaction) refuse(err error) []byte {
	tx.abort()
	return Encode(err, false)
}

func (tx *Transaction) abort() {
	if tx == nil {
		return
	}
	tx.aborted = true
	clear(tx.commands)
	tx.commands, tx.bytes = nil, 0
}

// queue checks cmd as Redis does before queueing it, and holds it if it passes.
func (tx *Transaction) queue(cmd *Command, conn Connection) []byte {
	if err := queueRefusal(cmd, conn); err != nil {
		return tx.refuse(err)
	}
	if tx.aborted {
		return queuedReply
	}
	size := CommandRetainedBytes(cmd) + queueSlotBytes
	if size > maxTransactionBytes-tx.bytes {
		return tx.refuse(errTransactionTooLarge)
	}
	tx.commands = append(tx.commands, cmd)
	tx.bytes += size
	return queuedReply
}

func queueRefusal(cmd *Command, conn Connection) error {
	if arity, own := answers(conn, cmd.Cmd); own {
		// The transport's own commands are not about the data, so neither a
		// replica nor a fenced primary refuses them; only their count matters.
		if !arityAccepts(arity, len(cmd.Args)) {
			return wrongArguments(cmd.Cmd)
		}
		return nil
	}
	if _, known := commandTable[cmd.Cmd]; !known {
		return unknownCommand(cmd.Cmd)
	}
	if notInTransaction[cmd.Cmd] {
		return errNotInTransaction
	}
	if arity, counted := commandArity[cmd.Cmd]; counted && !arityAccepts(arity, len(cmd.Args)) {
		return wrongArguments(cmd.Cmd)
	}
	// A replica refuses writes, and reads once it has lost its primary, as it
	// would outside a transaction; and so does a primary that has been fenced.
	return replicaCommandError(cmd.Cmd)
}

// writeExecAbort discards a transaction at EXEC and names the reason. Redis
// drops the generic ERR prefix from the reason and keeps any other class, such
// as READONLY or MASTERDOWN, which is the part a client can act on.
func writeExecAbort(w io.Writer, cause error) error {
	reason := strings.TrimPrefix(cause.Error(), "ERR ")
	_, err := w.Write(Encode(fmt.Errorf("EXECABORT Transaction discarded because of: %s", reason), false))
	return err
}

// transactionReplySlack is kept back for each command still to run when sizing
// what a large reply inside EXEC may use, so that a large read early in a
// transaction does not leave the small replies after it with nowhere to go. It
// covers an integer, a status or a typical error.
const transactionReplySlack = 128

func (tx *Transaction) exec(w io.Writer, conn Connection) error {
	if tx.aborted {
		_, err := w.Write(execAbortReply)
		return err
	}
	// What was allowed when a command was queued may not be allowed now: the
	// node may have been fenced by a higher term since, or a replica may have
	// lost its primary. Either all of a transaction runs or none of it does, so
	// every command is checked before the first one runs, as Redis checks EXEC.
	for _, cmd := range tx.commands {
		if _, own := answers(conn, cmd.Cmd); own {
			continue
		}
		if err := replicaCommandError(cmd.Cmd); err != nil {
			return writeExecAbort(w, err)
		}
	}

	// The reply is one array of every command's reply, and it is bound by the
	// same output limit as any other reply. Amplifying reads are told how much
	// of it the replies before them left, through replyCeiling, so one that
	// would not fit is refused before it is built. A reply that still does not
	// fit - many small ones - cannot be delivered, and the transaction runs on
	// regardless, because half of one must never run: the replies are dropped
	// and the transport closes the connection.
	n := len(tx.commands)
	total := decimalDigits(n) + 3
	replies := make([][]byte, 0, n)
	overflow := false
	ceiling := func(ran int) int {
		return max(0, MaxReplyBytes-total-transactionReplySlack*max(0, n-ran-1))
	}
	// Execution inside EXEC grows the log and holds replies just as a
	// pipelined run grows the arena; later commands' reservations must see both.
	budget := CommandAllocations
	var retained, logged int
	if budget != nil {
		retained, logged = budget.Retained, AppendRetainedBytes()
	}
	replyCeiling = ceiling(0)
	defer func() { replyCeiling = MaxReplyBytes }()
	run := func(cmd *Command, sink io.ReadWriter) error {
		if _, own := answers(conn, cmd.Cmd); own {
			conn.AnswerConnection(cmd, sink)
			return nil
		}
		// Each reply is framed in the protocol the connection has when the
		// command runs, not when it was queued: Redis runs a queued HELLO in
		// its place, and frames every reply after it in the new protocol.
		cmd.RESP3 = conn != nil && conn.RESP3()
		return EvalAndResponse(cmd, sink)
	}
	runTransaction(tx.commands, run, func(i int, reply []byte, err error) {
		if err == nil && len(reply) == 0 {
			// An empty element would shift every reply after it, and the
			// client would read each as the answer to the command before.
			err = errNoReply
		}
		if err != nil {
			reply = Encode(err, false)
		}
		if overflow || len(reply) > MaxReplyBytes-total {
			overflow, replies = true, nil
		} else {
			total += len(reply)
			replies = append(replies, reply)
		}
		if budget != nil {
			budget.ObserveRetained(retained + max(0, AppendRetainedBytes()-logged) + total)
		}
		replyCeiling = ceiling(i + 1)
	})
	if overflow {
		return ErrTransactionReplyTooLarge
	}
	out := appendArrayHeader(make([]byte, 0, total), n)
	for _, reply := range replies {
		out = append(out, reply...)
	}
	_, err := w.Write(out)
	return err
}

// runTransaction runs commands as one unit through run, for EXEC and for a
// block a replica has received. The unit is framed in the log and in the protocol 2 stream, and
// eviction waits until the block is closed, so a block holds what its
// transaction did and not the removal of whatever unrelated key the budget
// chose. Redis evicts before EXEC rather than between its commands, for the
// same reason; the budget can be passed by up to one transaction meanwhile,
// which the queue limit bounds. reply hears each command's answer in order.
func runTransaction(commands []*Command, run func(*Command, io.ReadWriter) error, reply func(int, []byte, error)) {
	aof.transaction, aof.transactionLogged = true, false
	replicationTransaction = replicationBlock{active: true}
	suspended := data_structure.SuspendEviction
	data_structure.SuspendEviction = true
	for i, cmd := range commands {
		var sink transactionSink
		err := run(cmd, &sink)
		reply(i, sink.p, err)
	}
	closeAOFTransaction()
	closeReplicationTransaction()
	data_structure.SuspendEviction = suspended
	data_structure.EnforceLimits()
}

// transactionSink keeps the reply it is handed rather than copying it, as the
// transport's own capture does; a command writes its reply once.
type transactionSink struct{ p []byte }

func (s *transactionSink) Read([]byte) (int, error) { return 0, io.EOF }
func (s *transactionSink) Write(p []byte) (int, error) {
	if s.p == nil {
		s.p = p
	} else {
		s.p = append(append(make([]byte, 0, len(s.p)+len(p)), s.p...), p...)
	}
	return len(p), nil
}

// The frames around a transaction's block, in the log and the protocol 2
// stream alike.
var (
	transactionOpenFrame  = appendCommand(nil, "MULTI")
	transactionCloseFrame = appendCommand(nil, "EXEC")
)

// openAOFTransaction writes MULTI ahead of a transaction's first record. It is
// the log's frame, not part of the command whose record follows it: protocol 2
// publishes its own MULTI when the block's first body reaches the history, and
// an opaque command publishes an image rather than its log bytes at all. So the
// frame is written as if outside a command, where a bounded drain does not
// publish it, and the command's published range starts after it. It is the
// first record of the block, so nothing of this command precedes it.
func openAOFTransaction() {
	active := aof.commandActive
	aof.commandActive = false
	appendAOFCommand("MULTI")
	aof.commandActive = active
	aof.commandStart = len(aof.buf)
}

// closeAOFTransaction writes EXEC after the last record of a block that has
// one, between commands.
func closeAOFTransaction() {
	logged := aof.transactionLogged
	aof.transaction, aof.transactionLogged = false, false
	if logged && aof.file != nil && !aof.replaying {
		appendAOFCommand("EXEC")
	}
}

// replicationBlock is the protocol 2 framing of the transaction running now.
//
// A replica has to hold a block until its EXEC arrives, and can only be sent
// it from the history, so a block the history cannot hold whole could never
// arrive whole: the stream is invalidated instead, and every replica takes a
// snapshot that already contains the transaction. dropped records that, and
// keeps the rest of the block out of the new epoch's history, which no replica
// is served from before the snapshot is taken.
type replicationBlock struct {
	active, opened, dropped bool
	bytes                   int
}

var replicationTransaction replicationBlock

// replicationTransactionBytes is the most of one block the stream will carry.
// Anything larger is past the history's own limit before it has ended.
const replicationTransactionBytes = replicationHistoryLimit

// admitReplicationTransaction frames a body published inside a transaction.
func admitReplicationTransaction(n int) bool {
	block := &replicationTransaction
	if block.dropped {
		return false
	}
	if !block.opened {
		block.opened, block.bytes = true, len(transactionOpenFrame)
		appendReplicationV2History(transactionOpenFrame)
	}
	if n > replicationTransactionBytes-block.bytes-len(transactionCloseFrame) {
		invalidateReplicationV2()
		return false
	}
	block.bytes += n
	return true
}

func closeReplicationTransaction() {
	block := replicationTransaction
	replicationTransaction = replicationBlock{}
	if !block.opened || block.dropped || !replicationV2Enabled() {
		return
	}
	if aof.failed != nil {
		// The bodies after the failure were never published. Ending the block
		// would deliver part of a transaction as all of it.
		invalidateReplicationV2()
		return
	}
	appendReplicationV2History(transactionCloseFrame)
}
