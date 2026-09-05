package core

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/config"
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
// into a shorter log that produces the same state. That is not built yet.
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
//   - Expiry and eviction are recorded as DEL, through data_structure.OnRemove.
//     Neither has a command behind it, and a log that omits them replays into a
//     keyspace holding keys the original had already dropped.
//
// Redis arrives at all five of these rules, by the same route.
type aofState struct {
	file *os.File
	// path is the file the descriptor above was opened on.
	//
	// Kept here rather than read from config when needed. A rewrite that took
	// the path from config would write the new log wherever the setting
	// currently points, which is not necessarily the file being appended to -
	// so a caller that opened one path would have its rewrite land in another,
	// leaving the first to grow forever and the second to be overwritten by
	// something that never belonged to it.
	path string
	// buf holds what this cycle produced. Writes go to the file once per loop
	// cycle rather than once per command: a command is a handful of bytes and a
	// write syscall each would put the log in the same position the reply path
	// was in before it started coalescing.
	buf []byte
	// staged is what the command currently executing wants recorded in its
	// place, for the commands that must not be replayed as they arrived.
	staged [][]string
	// extra is what the server decided on its own while the command ran: keys
	// dropped by eviction, or reaped because their expiry had passed. These are
	// additional to the command rather than instead of it - an eviction happens
	// because of a write, and losing the write to record the eviction would be
	// a straight swap of one bug for a worse one. They are also recorded when
	// the command was only a read, since a GET is what reaps an expired key.
	extra [][]string
	// replaying suppresses recording, so loading a log does not write it back
	// into itself.
	replaying bool
	lastSync  time.Time
	dirty     bool
	failed    error
	skip      bool

	// baseSize is the file's size after the last rewrite, and written what has
	// been appended since. Their ratio is what the automatic rewrite triggers
	// on: what matters is how much of the file is superseded, not how big it
	// is, and only a comparison against the size the data actually needs can
	// tell those apart.
	baseSize int64
	written  int64
	rewrites int
	lastKeys int
}

var aof aofState

// writeCommands are the commands that change the dataset. A command absent from
// here is a read, and a read is never recorded.
//
// Listed explicitly rather than derived, because the cost of the two mistakes
// is not symmetric: forgetting to add a write command loses data silently at
// the next restart, while a read listed by accident only makes the file bigger.
// An explicit list is the one that can be read against eval.go and checked.
var writeCommands = map[string]bool{
	"SET": true, "MSET": true, "DEL": true, "FLUSHDB": true,
	"EXPIRE": true, "PEXPIREAT": true, "PEXPIRE": true, "EXPIREAT": true, "PERSIST": true, "INCR": true, "INCRBY": true, "DECR": true, "DECRBY": true,
	"HSET": true, "HSETNX": true, "HDEL": true, "HINCRBY": true,
	"LPUSH": true, "RPUSH": true, "LPOP": true, "RPOP": true, "LTRIM": true, "LSET": true,
	"SADD": true, "SREM": true, "SPOP": true,
	"ZADD": true, "ZREM": true,
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
func persistedName(cmd string) string {
	if cmd == "MEMKV.RESTORE" {
		return "KEEL.RESTORE"
	}
	return cmd
}

// AOFEnabled reports whether the log is on.
func AOFEnabled() bool { return aof.file != nil }

// OpenAOF opens the log for appending and installs the removal hook. Call after
// LoadAOF, so replaying does not append what it is reading.
func OpenAOF(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	aof.file = f
	aof.path = path
	aof.lastSync = time.Now()
	aof.dirty = false
	aof.failed = nil
	// Counters describe this open file, not whatever the last one did, so they
	// start again with it. Carrying them over would make a fresh log report
	// rewrites it has never had.
	aof.rewrites = 0
	nextAutoRewrite = time.Time{}
	aof.lastKeys = 0
	// Whatever is already on disk is the base the growth trigger measures
	// against, so a server restarted onto an existing log does not immediately
	// decide the log has grown infinitely.
	if info, err := f.Stat(); err == nil {
		aof.baseSize = info.Size()
	}
	aof.written = 0
	data_structure.OnRemove = func(keyspace, key string) {
		if aof.file == nil || aof.replaying {
			return
		}
		aof.extra = append(aof.extra, []string{"DEL", key})
		// Eviction and expiry remove keys no command named, so a rewrite has to
		// hear about them here or it would carry a key forward that the server
		// had already dropped.
		noteRewriteDirty(key)
	}
	return nil
}

// CloseAOF flushes what is buffered and closes the file. A stop that skipped
// this would lose up to a cycle's worth of acknowledged writes, which is the
// one kind of loss a client has no way to detect.
func CloseAOF() error {
	if aof.file == nil {
		return nil
	}
	err := flushAOF(true)
	if cerr := aof.file.Close(); err == nil {
		err = cerr
	}
	aof.file = nil
	aof.path = ""
	data_structure.OnRemove = nil
	return err
}

// aofLog reports something about the log that a client did not ask for.
func aofLog(format string, args ...interface{}) {
	log.Printf("appendonly: "+format, args...)
}

// aofRecord stages one command to be written for the command being executed.
func aofRecord(parts ...string) {
	if aof.file == nil || aof.replaying {
		return
	}
	aof.staged = append(aof.staged, parts)
}

// aofBegin resets the staging areas before a command runs.
func aofBegin() {
	aof.skip = false
	aof.staged = aof.staged[:0]
	aof.extra = aof.extra[:0]
}

// aofCommit records what the command that just ran actually did.
func aofCommit(cmd *Command, reply []byte) {
	if aof.file == nil || aof.replaying {
		return
	}

	// A key written while a rewrite is walking may already have been recorded
	// at an older value, or not yet reached. Either way the rewrite will write
	// it again at the end from whatever it holds then, so it only has to know
	// which keys those are. Recorded whether or not the log itself takes the
	// command, because a rewrite is a separate question from durability: a read
	// that reaps an expired key changes the keyspace without being logged.
	if rewrite.active {
		for _, key := range writtenKeys(cmd) {
			noteRewriteDirty(key)
		}
	}

	switch {
	case aof.skip:
	case len(aof.staged) > 0:
		// Staged because the command as it arrived would not replay to the
		// same state, so the replacement is what goes in the log.
		for _, parts := range aof.staged {
			aof.buf = appendCommand(aof.buf, parts...)
		}
	case !writeCommands[cmd.Cmd]:
		// A read. Nothing of the command itself is recorded, but it may still
		// have reaped an expired key on the way past, which is in extra.
	case len(reply) > 0 && reply[0] == '-':
		// Failed, so by the heuristic in the file comment it changed nothing.
	default:
		aof.buf = appendCommand(aof.buf, append([]string{persistedName(cmd.Cmd)}, cmd.Args...)...)
	}

	// After the command, because a key evicted to make room for a write has to
	// be dropped after that write, not before it - and under a policy that can
	// choose any key, the one evicted is occasionally the one just written.
	aofCommitExtras()
	aof.staged = aof.staged[:0]
}

// aofCommitExtras writes the removals the server decided on by itself.
//
// Separate from aofCommit because active expiry runs between commands rather
// than inside one: the keys it reaps are recorded by the same hook, and without
// this nothing would ever write them out - and worse, the next command's
// aofBegin would clear them, so a restart would bring back keys the server had
// already expired.
func aofCommitExtras() {
	if aof.file == nil || aof.replaying {
		aof.extra = aof.extra[:0]
		return
	}
	for _, parts := range aof.extra {
		aof.buf = appendCommand(aof.buf, parts...)
	}
	aof.extra = aof.extra[:0]
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
func FlushAOF() error {
	if err := flushAOF(false); err != nil {
		return err
	}

	// A rewrite in progress gets one slice per cycle, which is what keeps it
	// from being a stall. This is the right place for it because it is already
	// the once-a-cycle hook: doing it per command would slice a pipelined batch
	// in the middle for no reason.
	if rewrite.active {
		return AdvanceRewrite()
	}
	maybeRewrite()
	return nil
}

// aofSync is injectable so tests can exercise disk failures without relying on hardware.
var aofSync = func(f *os.File) error { return f.Sync() }

func flushAOF(closing bool) error {
	if aof.file == nil {
		return nil
	}
	if aof.failed != nil {
		return aof.failed
	}
	if len(aof.buf) > 0 {
		n, err := aof.file.Write(aof.buf)
		aof.written += int64(n)
		if n > 0 {
			aof.dirty = true
		}
		if err == nil && n != len(aof.buf) {
			err = io.ErrShortWrite
		}
		if err != nil {
			aof.buf = aof.buf[n:]
			aof.failed = err
			return err
		}
		aof.buf = aof.buf[:0]
	}
	syncDue := closing || config.AOFFsync == config.FsyncAlways ||
		(config.AOFFsync == config.FsyncEverySec && time.Since(aof.lastSync) >= time.Second)
	if aof.dirty && syncDue {
		if err := aofSync(aof.file); err != nil {
			aof.failed = err
			return err
		}
		aof.lastSync = time.Now()
		aof.dirty = false
	}
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
func LoadAOF(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	aof.replaying = true
	data_structure.SuspendEviction = true
	data_structure.SuspendExpiry = true
	defer func() {
		data_structure.SuspendExpiry = false
		data_structure.EachKeyspace(func(ks data_structure.Keyspace) { ks.ActiveExpire(ks.KeysWithExpiry()) })
		aof.replaying = false
		data_structure.SuspendEviction = false
		data_structure.EnforceLimits()
	}()
	reader := bufio.NewReaderSize(f, 64*1024)
	applied, used := 0, int64(0)
	for {
		cmd, n, err := readAOFCommand(reader)
		if err == io.EOF && n == 0 {
			return applied, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return applied, &truncatedAOF{path, used}
		}
		if err != nil {
			return applied, fmt.Errorf("malformed command at byte %d: %w", used, err)
		}
		sink := &replayWriter{}
		if err := EvalAndResponse(cmd, sink); err != nil {
			return applied, fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, used, err)
		}
		if sink.err != nil {
			return applied, fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, used, sink.err)
		}
		applied++
		used += n
	}
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
}

func (e *truncatedAOF) Error() string {
	return fmt.Sprintf("truncated final command in %s at byte %d", e.path, e.offset)
}
func (e *truncatedAOF) Unwrap() error { return errTruncatedAOF }

// RepairAOFTail preserves the torn suffix before truncating to the last complete command.
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
func aofExpireAt(key string) {
	if aof.file == nil || aof.replaying {
		return
	}
	owner, ok := data_structure.OwnerOf(key)
	if !ok {
		return
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return
	}
	aofRecord("PEXPIREAT", key, strconv.FormatUint(at, 10))
}
