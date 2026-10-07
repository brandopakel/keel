package core

import (
	"bufio"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// The append-only file.
//
// Everything else in this server is in memory and stays there, so a restart has
// always been a flush. The append-only file is the smallest honest way out of
// that: every command that changes the dataset is written to the end of a file,
// and starting up means replaying it. No index, no pages, no btree - the
// durability comes entirely from the fact that appending is the one file
// operation that is cheap and hard to get wrong.
//
// The log records commands rather than the data they produce, which is what
// makes it cheap to write and expensive to load: a key written a million times
// is a million lines, and replaying is as slow as the original run was. That is
// the trade Redis makes too, and it is why an AOF eventually needs rewriting
// into a shorter log that produces the same state. Incremental rewrites do that.
//
// # What has to be recorded, and what has to be rewritten
//
// A log is only worth having if replaying it produces what was there before, so
// what goes into it is not simply "the commands that arrived".
//
//   - Reads are not recorded. Nothing else would be wrong if they were, but the
//     file would grow with traffic rather than with changes.
//   - A command that fails is not recorded, on the reasoning that a reply
//     beginning with '-' changed nothing. That is a heuristic and it is stated
//     here rather than hidden: a command that half-succeeded and then errored
//     would be missed by it. None of the commands here do that today.
//   - SPOP is recorded as the SREM it turned out to be. It removes members
//     chosen at random, so replaying the command itself would remove different
//     ones and the set would diverge from the first restart onwards.
//   - EXPIRE and SET with a TTL are recorded as PEXPIREAT, which names an
//     instant instead of a duration. Replaying "expire in ten seconds" a day
//     later grants ten fresh seconds, so every restart would renew every TTL in
//     the keyspace.
//   - Expiry and eviction are recorded as DEL, through the OnRemove hook of
//     the engine's space. Neither has a command behind it, and a log
//     that omits them replays into a keyspace holding keys the original had
//     already dropped.
//
// Redis arrives at all five of these rules, by the same route.
//
// Each Engine holds one aofState, as e.aof, for the log it records in.
type aofState struct {
	file        *os.File
	digest      hash.Hash
	digestBytes int64
	// path is the file the descriptor above was opened on.
	//
	// Kept here rather than read from config when needed. A rewrite that took
	// the path from config would write the new log wherever the setting
	// currently points, which is not necessarily the file being appended to -
	// so a caller that opened one path would have its rewrite land in another,
	// leaving the first to grow forever and the second to be overwritten by
	// something that never belonged to it.
	path string
	// buf coalesces ordinary commands until the loop flushes. Large transcripts
	// drain bounded fragments without syncing or advancing a rewrite mid-record.
	buf []byte
	// staged is what the command currently executing wants recorded in its
	// place, for the commands that must not be replayed as they arrived.
	staged       [][]string
	commandStart int
	// A scope preserves the unpublished replication prefix across bounded
	// buffer drains. Opaque commands publish their final image instead.
	commandActive, commandOpaque, commandChanged bool
	// transaction is set while EXEC runs its commands, and transactionLogged
	// once MULTI has been written ahead of the block's first record. A
	// transaction that records nothing writes no frame; see transaction.go.
	transaction, transactionLogged bool
	// replaying suppresses recording, so loading a log does not write it back
	// into itself.
	recovered   []string
	replaying   bool
	lastSync    time.Time
	syncPending chan error
	syncOffset  uint64
	dirty       bool
	failed      error
	skip        bool

	// baseSize is the file's size after the last rewrite, and written what has
	// been appended since. Their ratio is what the automatic rewrite triggers
	// on: what matters is how much of the file is superseded, not how big it
	// is, and only a comparison against the size the data actually needs can
	// tell those apart.
	baseSize    int64
	rewriteBase int64
	written     int64
	rewrites    int
	lastKeys    int
}

// writeCommands are the commands that change the dataset. A command absent from
// here is a read, and a read is never recorded.
//
// Listed explicitly rather than derived, because the cost of the two mistakes
// is not symmetric: forgetting to add a write command loses data silently at
// the next restart, while a read listed by accident only makes the file bigger.
// An explicit list is the one that can be read against eval.go and checked.
var writeCommands = map[string]bool{
	"SET": true, "SETNX": true, "MSET": true, "DEL": true, "UNLINK": true, "FLUSHDB": true,
	"SETEX": true, "PSETEX": true,
	"EXPIRE": true, "PEXPIREAT": true, "PEXPIRE": true, "EXPIREAT": true, "PERSIST": true, "INCR": true, "INCRBY": true, "DECR": true, "DECRBY": true,
	"HSET": true, "HSETNX": true, "HDEL": true, "HINCRBY": true,
	"LPUSH": true, "RPUSH": true, "LPOP": true, "RPOP": true, "LTRIM": true, "LSET": true,
	"SADD": true, "SREM": true, "SPOP": true,
	"ZADD": true, "ZREM": true, "ZINCRBY": true, "ZPOPMIN": true, "ZPOPMAX": true,
	"GEOADD":     true,
	"BF.RESERVE": true, "BF.ADD": true, "BF.MADD": true,
	"CMS.INITBYDIM": true, "CMS.INITBYPROB": true, "CMS.INCRBY": true,
	"MORRIS.INITBYDIM": true, "MORRIS.INITBYPROB": true, "MORRIS.INCRBY": true,
	"PFADD": true, "PFMERGE": true,
	"CF.RESERVE": true, "CF.ADD": true, "CF.ADDNX": true, "CF.DEL": true,
	"KEEL.RESTORE": true, "MEMKV.RESTORE": true,
}

// persistedName is the name a command is recorded under, which is not always
// the name it arrived under.
//
// MEMKV.RESTORE is accepted so that logs written before the rename replay, but
// a command is appended to the log as it was received - so a client sending the
// old name would write the old name into a brand new file, and the log would
// carry the alias forward forever. Read both, write one.
//
// UNLINK is recorded as the DEL it is, so a log written by this build still
// replays on one from before UNLINK existed - which is what a rollback needs.
func persistedName(cmd string) string {
	switch cmd {
	case "MEMKV.RESTORE":
		return "KEEL.RESTORE"
	case "UNLINK":
		return "DEL"
	}
	return cmd
}

// AOFEnabled reports whether the log is on.
func (e *Engine) AOFEnabled() bool { return e.aof.file != nil }

// OpenAOF opens the log for appending and installs the removal hook. Call after
// LoadAOF, so replaying does not append what it is reading.
//
// The log is e's, and the removal hook is installed on e's space, so only e's
// expiry and eviction are recorded in it.
func (e *Engine) OpenAOF(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		f.Close()
		return err
	}

	e.aof.file = f
	e.aof.path = path
	e.aof.lastSync = time.Now()
	e.aof.dirty = false
	e.aof.failed = nil
	// Counters describe this open file, not whatever the last one did, so they
	// start again with it. Carrying them over would make a fresh log report
	// rewrites it has never had.
	e.aof.rewrites = 0
	e.rewriteBudgetAborts = 0
	e.nextAutoRewrite = time.Time{}
	e.resetRewriteOutcome()
	e.aof.lastKeys = 0
	// Whatever is already on disk is the base the growth trigger measures
	// against, so a server restarted onto an existing log does not immediately
	// decide the log has grown infinitely.
	if info, err := f.Stat(); err == nil {
		e.aof.baseSize = info.Size()
	}
	if err := e.openAOFDigest(path); err != nil {
		f.Close()
		e.aof.file = nil
		return err
	}
	e.aof.rewriteBase = e.aof.baseSize
	// Frequent restarts must not reset compaction's growth target to the
	// ever-growing log. Use a conservative live-state estimate until the next
	// actual rewrite provides an exact baseline. HLL's wire registers remain
	// dense even when its in-memory representation is compact.
	live := e.space.TotalMemUsed()
	const maxInt64 = uint64(1<<63 - 1)
	hllWire := uint64(e.hllStore.Len()) * (16 << 10)
	if live <= (maxInt64-hllWire)/3 {
		if estimate := int64(live*3 + hllWire); estimate < e.aof.rewriteBase {
			e.aof.rewriteBase = estimate
		}
	}
	e.aof.written = 0
	e.appendStarted, e.appendCompleted = 0, 0
	e.appendWritten, e.appendSynced = 0, 0
	for _, key := range e.aof.recovered {
		e.appendAOFCommand("DEL", key)
	}
	e.aof.recovered = nil
	e.space.OnRemove = func(keyspace, key string) {
		if e.aof.file == nil || e.aof.replaying {
			return
		}
		outsideCommand := !e.aof.commandActive
		if outsideCommand {
			e.aofBegin("")
			defer e.aofEnd()
		}
		e.appendAOFCommand("DEL", key)
		// Eviction and expiry remove keys no command named, so a rewrite has to
		// hear about them here or it would carry a key forward that the server
		// had already dropped.
		e.noteRewriteDirty(key)
		e.noteReplicationDirty(key)
	}
	return nil
}

// RenameAOF gives e's open log the name path, in the same directory, and
// syncs the directory, so that the new name survives a crash. A log written
// under a temporary name is published this way only once it holds what it
// must: the server's migration of a legacy log writes the replayed keyspace
// into a log under a temporary name and then renames it onto the name it is
// kept under, so that a crash before then leaves no log under that name, and
// the next start replays the legacy log again.
//
// The descriptor stays open across the rename, and everything e writes after
// it, a rewrite included, goes to the log at its new name.
func (e *Engine) RenameAOF(path string) error {
	if e.aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if e.rewrite.active {
		return fmt.Errorf("a rewrite of %s is in progress", e.aof.path)
	}
	if err := os.Rename(e.aof.path, path); err != nil {
		return err
	}
	e.aof.path = path
	if err := syncDir(filepath.Dir(path)); err != nil {
		// As after a rewrite's rename: the next sync of the log tries the
		// directory again before it counts the log as durable.
		e.unsyncedLogDir = filepath.Dir(path)
		return err
	}
	e.unsyncedLogDir = "" // this sync covers an earlier rename's entry too
	return nil
}

// CloseAOF flushes what is buffered and closes the file. A stop that skipped
// this would lose up to a cycle's worth of acknowledged writes, which is the
// one kind of loss a client has no way to detect.
func (e *Engine) CloseAOF() error {
	e.closeReplicationSnapshot()
	e.CancelRewrite()
	_, _ = e.pollRewriteIO(true)
	if e.aof.file == nil {
		return nil
	}
	// Join both workers even after a failure before closing their descriptor.
	e.pollAppend(true)
	e.pollAOFSync(true)
	err := e.flushAOF(true)
	if cerr := e.aof.file.Close(); err == nil {
		err = cerr
	}
	e.aof.file = nil
	e.aof.path = ""
	e.unsyncedLogDir = ""
	e.aof.buf, e.aof.staged = nil, nil
	e.space.OnRemove = nil
	return err
}

// aofLog reports something about the log that a client did not ask for.
func aofLog(format string, args ...interface{}) {
	log.Printf("appendonly: "+format, args...)
}

// aofRecord stages one command to be written for the command being executed.
func (e *Engine) aofRecord(parts ...string) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}
	e.aof.staged = append(e.aof.staged, parts)
}

// aofBegin resets the staging areas before a command runs.
func (e *Engine) aofBegin(name string) {
	e.aof.commandActive, e.aof.commandChanged = true, false
	e.aof.commandOpaque = isOpaqueReplicationCommand(name)
	e.aof.commandStart = len(e.aof.buf)
	e.aof.skip = false
	clear(e.aof.staged)
	e.aof.staged = e.aof.staged[:0]
}

// aofCommit records what the command that just ran actually did.
func (e *Engine) aofCommit(cmd *Command, reply []byte) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}

	// A key written while a rewrite is walking may already have been recorded
	// at an older value, or not yet reached. Either way the rewrite will write
	// it again at the end from whatever it holds then, so it only has to know
	// which keys those are. Recorded whether or not the log itself takes the
	// command, because a rewrite is a separate question from durability: a read
	// that reaps an expired key changes the keyspace without being logged.
	if e.rewrite.active {
		for _, key := range writtenKeys(cmd) {
			e.noteRewriteDirty(key)
		}
	}

	if writeCommands[cmd.Cmd] {
		for _, key := range writtenKeys(cmd) {
			e.noteReplicationDirty(key)
		}
	}

	switch {
	case e.aof.skip:
	case len(e.aof.staged) > 0:
		// Staged because the command as it arrived would not replay to the
		// same state, so the replacement is what goes in the log.
		for _, parts := range e.aof.staged {
			e.appendAOFCommand(parts[0], parts[1:]...)
		}
	case !writeCommands[cmd.Cmd]:
		// A read. Nothing of the command itself is recorded, but it may still
		// have reaped an expired key, already recorded by the removal hook.
	case len(reply) > 0 && reply[0] == '-':
		// Failed, so by the heuristic in the file comment it changed nothing.
	default:
		e.appendAOFCommand(persistedName(cmd.Cmd), cmd.Args...)
	}

	clear(e.aof.staged)
	e.aof.staged = e.aof.staged[:0]
}

// aofEnd publishes the complete command after its eviction decisions. The
// event loop cannot serve a replica pull in the middle of this serial scope.
func (e *Engine) aofEnd() {
	// A failed drain reslices the retained buffer. Its old commandStart no
	// longer names this slice, and none of that failed suffix may be published.
	if e.aof.file != nil && !e.aof.replaying && e.aof.failed == nil {
		e.recordReplicationV2Commit()
	}
	e.aof.commandActive = false
}

// appendCommand writes one command in the same RESP a client would have sent,
// so that loading the log is exactly the parser that already exists rather than
// a second format to keep in step with the first.
func appendCommand(dst []byte, parts ...string) []byte {
	dst = appendArrayHeader(dst, len(parts))
	for _, p := range parts {
		dst = appendBulkString(dst, p)
	}
	return dst
}

// FlushAOF writes the cycle's commands and syncs according to the policy.
//
// The event loop calls this after executing and before replying, which is the
// ordering that makes appendfsync always mean what it says: a client is told
// its write succeeded only once the write is on disk. Doing it after the
// replies would be faster and would be lying.
func (e *Engine) FlushAOF() error {
	if err := e.flushAOF(false); err != nil {
		return err
	}

	// A rewrite in progress gets one slice per cycle, which is what keeps it
	// from being a stall. This is the right place for it because it is already
	// the once-a-cycle hook: doing it per command would slice a pipelined batch
	// in the middle for no reason.
	if e.rewrite.active {
		return e.AdvanceRewrite()
	}
	e.maybeRewrite()
	return nil
}

// Only the engine's command thread - the event loop, for the server's engine -
// touches its log state. The worker owns one Sync call and publishes its
// result through a buffered channel; there is never a work queue.
func (e *Engine) pollAOFSync(wait bool) {
	if e.aof.syncPending == nil {
		return
	}
	var err error
	if wait {
		err = <-e.aof.syncPending
	} else {
		select {
		case err = <-e.aof.syncPending:
		default:
			return
		}
	}
	e.aof.syncPending = nil
	if err == nil {
		e.appendSynced = max(e.appendSynced, e.aof.syncOffset)
	}
	if err != nil && e.aof.failed == nil {
		e.aof.failed = err
	}
}

func (e *Engine) flushAOF(closing bool) error {
	fsync := e.settings.fsync
	e.pollAOFSync(closing || fsync == FsyncAlways)
	if e.aof.file == nil {
		return nil
	}
	if e.aof.failed != nil {
		return e.aof.failed
	}
	if err := e.writeAOFBuffer(); err != nil {
		return err
	}
	syncDue := closing || fsync == FsyncAlways ||
		(fsync == FsyncEverySec && time.Since(e.aof.lastSync) >= time.Second)
	if e.aof.dirty && syncDue && e.aof.syncPending == nil {
		// A rewrite's rename whose directory sync failed is finished first:
		// what is about to be synced is in the file that rename named.
		if err := e.syncPendingLogDir(); err != nil {
			e.aof.failed = err
			return err
		}
		if !closing && fsync == FsyncEverySec {
			result := make(chan error, 1)
			file, syncFile, stats := e.aof.file, e.aofSync, &e.appendSyncStats
			wake := e.rewriteWake
			e.aof.syncPending = result
			e.aof.syncOffset = e.appendWritten
			e.aof.lastSync = time.Now()
			// Writes during this Sync stay dirty and require another sync.
			e.aof.dirty = false
			go func() {
				result <- timedPersistenceSync(stats, file, syncFile)
				if wake != nil {
					wake()
				}
			}()
			e.appendCompleted = e.appendWritten
			return nil
		}
		if err := timedPersistenceSync(&e.appendSyncStats, e.aof.file, e.aofSync); err != nil {
			e.aof.failed = err
			return err
		}
		e.aof.lastSync = time.Now()
		e.aof.dirty = false
		e.appendSynced = e.appendWritten
	}
	e.appendCompleted = e.appendWritten
	return nil
}

// LoadAOF replays a log into the keyspace.
//
// A crash can leave a half-written command at the end - the process died
// between two write syscalls, or the filesystem kept only part of the last one.
// That tail is dropped rather than treated as corruption, because it is the
// expected shape of an unclean stop and the commands before it are perfectly
// good. Anything malformed earlier in the file is a real error and is reported
// as one.
//
// A transaction's block is held until its EXEC has been read and only then
// replayed, so a crash part way through writing one leaves none of it applied.
// Its MULTI is where the torn tail starts: what was intact of the block goes to
// the backup with the rest, which is Redis's rule for an AOF that ends inside
// MULTI.
//
// The log replays into e's keyspace, with eviction and expiry held off in e's
// space until it has.
func (e *Engine) LoadAOF(path string) (int, error) {
	e.aof.recovered = nil
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	e.aof.replaying = true
	e.space.SuspendEviction = true
	e.space.SuspendExpiry = true
	defer func() {
		priorRemovalHook := e.space.OnRemove
		e.space.OnRemove = func(_, key string) { e.aof.recovered = append(e.aof.recovered, key) }
		defer func() { e.space.OnRemove = priorRemovalHook }()
		if e.replicaOf() != "" && e.replicationProtocol() == 2 {
			e.aof.replaying = false
			return // preserve the exact primary-decided prefix for checkpoints
		}
		e.space.SuspendExpiry = false
		e.space.EachKeyspace(func(ks data_structure.Keyspace) { ks.ActiveExpire(ks.KeysWithExpiry()) })
		e.aof.replaying = false
		e.space.SuspendEviction = false
		e.space.EnforceLimits()
	}()
	reader := bufio.NewReaderSize(f, 64*1024)
	applied, used := 0, int64(0)
	// block is the open transaction's commands and where each began; begun is
	// where its MULTI began, or -1 outside one.
	var block []replayedCommand
	begun := int64(-1)
	for {
		cmd, n, err := readAOFCommand(reader)
		if err == io.EOF && n == 0 {
			if begun >= 0 {
				return applied, &truncatedAOF{path, begun, true}
			}
			return applied, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if begun >= 0 {
				return applied, &truncatedAOF{path, begun, true}
			}
			return applied, &truncatedAOF{path, used, false}
		}
		if err != nil {
			return applied, fmt.Errorf("malformed command at byte %d: %w", used, err)
		}
		frame := cmd.Cmd == "MULTI" || cmd.Cmd == "EXEC"
		switch {
		case frame && len(cmd.Args) != 0:
			return applied, fmt.Errorf("malformed %s frame at byte %d", cmd.Cmd, used)
		case cmd.Cmd == "MULTI" && begun >= 0:
			return applied, fmt.Errorf("MULTI at byte %d inside the transaction begun at byte %d", used, begun)
		case cmd.Cmd == "MULTI":
			begun = used
		case cmd.Cmd == "EXEC" && begun < 0:
			return applied, fmt.Errorf("EXEC without MULTI at byte %d", used)
		case cmd.Cmd == "EXEC":
			for _, queued := range block {
				if err := e.replayAOFCommand(queued.cmd, queued.at); err != nil {
					return applied, err
				}
				applied++
			}
			clear(block)
			block, begun = block[:0], -1
		case begun >= 0:
			block = append(block, replayedCommand{cmd, used})
		default:
			if err := e.replayAOFCommand(cmd, used); err != nil {
				return applied, err
			}
			applied++
		}
		used += n
	}
}

type replayedCommand struct {
	cmd *Command
	at  int64
}

func (e *Engine) replayAOFCommand(cmd *Command, at int64) error {
	sink := &replayWriter{}
	if err := e.EvalAndResponse(cmd, sink); err != nil {
		return fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, at, err)
	}
	if sink.err != nil {
		return fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, at, sink.err)
	}
	return nil
}

// Read one canonical AOF frame; memory is proportional to one command, not the log.
func readAOFCommand(r *bufio.Reader) (*Command, int64, error) {
	var consumed int64
	length := func(prefix byte) (int64, error) {
		line, err := r.ReadSlice('\n')
		consumed += int64(len(line))
		if err != nil {
			return 0, err
		}
		if len(line) < 4 || line[0] != prefix || line[len(line)-2] != '\r' {
			return 0, ErrProtocol
		}
		n, ok := parseDecimal(line[1 : len(line)-2])
		if !ok || n < 0 {
			return 0, ErrProtocol
		}
		return n, nil
	}
	count, err := length('*')
	if err != nil {
		return nil, consumed, err
	}
	if count == 0 || count > maxMultiBulkLength {
		return nil, consumed, ErrProtocol
	}
	parts := make([]string, 0, min(int(count), 16))
	for i := int64(0); i < count; i++ {
		size, err := length('$')
		if err != nil {
			return nil, consumed, err
		}
		if size > maxBulkLength {
			return nil, consumed, ErrProtocol
		}
		b := make([]byte, size+2)
		n, err := io.ReadFull(r, b)
		consumed += int64(n)
		if err != nil {
			return nil, consumed, err
		}
		if b[size] != '\r' || b[size+1] != '\n' {
			return nil, consumed, ErrProtocol
		}
		parts = append(parts, string(b[:size]))
	}
	return &Command{Cmd: strings.ToUpper(parts[0]), Args: parts[1:]}, consumed, nil
}

type replayWriter struct{ err error }

func (w *replayWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *replayWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '-' {
		w.err = errors.New(strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

type truncatedAOF struct {
	path   string
	offset int64
	// transaction says the tail begins with a transaction whose EXEC was
	// never written, rather than with a command cut short.
	transaction bool
}

func (e *truncatedAOF) Error() string {
	if e.transaction {
		return fmt.Sprintf("incomplete final transaction in %s from byte %d", e.path, e.offset)
	}
	return fmt.Sprintf("truncated final command in %s at byte %d", e.path, e.offset)
}
func (e *truncatedAOF) Unwrap() error { return errTruncatedAOF }

// RepairAOFTail preserves the torn suffix before truncating to the last
// complete command, or to before the MULTI of a transaction that never ended.
func RepairAOFTail(cause error) error {
	var tail *truncatedAOF
	if !errors.As(cause, &tail) {
		return cause
	}
	f, err := os.OpenFile(tail.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	backup, err := os.CreateTemp(filepath.Dir(tail.path), ".keel-torn-tail-*")
	if err != nil {
		return err
	}
	_, err = f.Seek(tail.offset, io.SeekStart)
	if err == nil {
		_, err = io.Copy(backup, f)
	}
	if err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(tail.path)); err != nil {
		return err
	}
	if err := f.Truncate(tail.offset); err != nil {
		return err
	}
	return f.Sync()
}

// errTruncatedAOF marks the one failure that is not a failure: a log whose last
// command did not finish being written.
var errTruncatedAOF = errors.New("truncated final command")

// IsTruncatedAOF reports whether a LoadAOF error is only a half-written tail,
// which a server should log and carry on from rather than refuse to start over.
func IsTruncatedAOF(err error) bool { return errors.Is(err, errTruncatedAOF) }

// discardWriter throws replies away. Replaying produces one per command and
// there is nobody to send them to.
type discardWriter struct{}

func (discardWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// aofExpireAt stages a PEXPIREAT for a key whose TTL was just set relative to
// now, so the log names an instant rather than a duration.
func (e *Engine) aofExpireAt(key string) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}
	owner, ok := e.space.OwnerOf(key)
	if !ok {
		return
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return
	}
	e.aofRecord("PEXPIREAT", key, strconv.FormatUint(at, 10))
}
