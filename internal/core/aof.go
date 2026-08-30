package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"memkv/internal/config"
	"memkv/internal/data_structure"
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
	"SET": true, "DEL": true, "EXPIRE": true, "PEXPIREAT": true, "INCR": true,
	"SADD": true, "SREM": true, "SPOP": true,
	"ZADD": true, "ZREM": true,
	"GEOADD":     true,
	"BF.RESERVE": true, "BF.MADD": true,
	"CMS.INITBYDIM": true, "CMS.INITBYPROB": true, "CMS.INCRBY": true,
	"MORRIS.INITBYDIM": true, "MORRIS.INITBYPROB": true, "MORRIS.INCRBY": true,
	"PFADD": true, "PFMERGE": true,
	"CF.RESERVE": true, "CF.ADD": true, "CF.ADDNX": true, "CF.DEL": true,
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
	aof.lastSync = time.Now()
	data_structure.OnRemove = func(keyspace, key string) {
		if aof.file != nil && !aof.replaying {
			aof.extra = append(aof.extra, []string{"DEL", key})
		}
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
	data_structure.OnRemove = nil
	return err
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
	aof.staged = aof.staged[:0]
	aof.extra = aof.extra[:0]
}

// aofCommit records what the command that just ran actually did.
func aofCommit(cmd *MemKVCmd, reply []byte) {
	if aof.file == nil || aof.replaying {
		return
	}

	switch {
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
		aof.buf = appendCommand(aof.buf, append([]string{cmd.Cmd}, cmd.Args...)...)
	}

	// After the command, because a key evicted to make room for a write has to
	// be dropped after that write, not before it - and under a policy that can
	// choose any key, the one evicted is occasionally the one just written.
	for _, parts := range aof.extra {
		aof.buf = appendCommand(aof.buf, parts...)
	}

	aof.staged = aof.staged[:0]
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
func FlushAOF() error { return flushAOF(false) }

func flushAOF(closing bool) error {
	if aof.file == nil || (len(aof.buf) == 0 && !closing) {
		return nil
	}
	if len(aof.buf) > 0 {
		if _, err := aof.file.Write(aof.buf); err != nil {
			return err
		}
		aof.buf = aof.buf[:0]
	}

	switch config.AOFFsync {
	case config.FsyncAlways:
		return aof.file.Sync()
	case config.FsyncEverySec:
		if closing || time.Since(aof.lastSync) >= time.Second {
			aof.lastSync = time.Now()
			return aof.file.Sync()
		}
	case config.FsyncNever:
		if closing {
			return aof.file.Sync()
		}
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
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	aof.replaying = true
	// Eviction is the log's business, not the replay's: every key the original
	// run evicted is in here as a DEL, and a replay that also evicted would
	// drop those keys twice over and a different set besides. The bound is
	// applied once at the end instead, so a log written under a larger limit
	// than the one configured now still ends up inside it.
	data_structure.SuspendEviction = true
	defer func() {
		aof.replaying = false
		data_structure.SuspendEviction = false
		data_structure.EnforceLimits()
	}()

	var sink discardWriter
	applied, used := 0, 0
	for used < len(data) {
		cmd, consumed, perr := ParseCmd(data[used:])
		if errors.Is(perr, ErrIncompleteFrame) {
			return applied, fmt.Errorf("truncated command %d bytes into %s, %d commands loaded: %w",
				used, path, applied, errTruncatedAOF)
		}
		if perr != nil {
			return applied, fmt.Errorf("malformed command %d bytes into %s: %w", used, path, perr)
		}
		if err := EvalAndResponse(cmd, sink); err != nil {
			return applied, fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, used, err)
		}
		used += consumed
		applied++
	}
	return applied, nil
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
	at, has := dictStore.ExpiryOf(key)
	if !has {
		return
	}
	aofRecord("PEXPIREAT", key, strconv.FormatUint(at, 10))
}
