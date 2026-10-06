package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/data_structure"
)

// session drives Transact the way a connection does, keeping the open
// transaction between commands.
type session struct {
	t    *testing.T
	tx   *Transaction
	conn Connection
}

// send answers one command and returns its raw reply.
func (s *session) send(parts ...string) string {
	s.t.Helper()
	var w replyWriter
	var err error
	cmd := &Command{Cmd: strings.ToUpper(parts[0]), Args: parts[1:]}
	if s.tx != nil || IsTransactionCommand(cmd.Cmd) {
		s.tx, err = Transact(s.tx, cmd, &w, s.conn)
	} else {
		err = EvalAndResponse(cmd, &w)
	}
	require.NoError(s.t, err)
	return string(w.b)
}

func newSession(t *testing.T) *session {
	ResetStores()
	return &session{t: t}
}

func TestTransactionQueuesThenRunsInOrder(t *testing.T) {
	s := newSession(t)
	require.Equal(t, "+OK\r\n", s.send("MULTI"))
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "k", "1"))
	require.Equal(t, "+QUEUED\r\n", s.send("INCR", "k"))
	require.Equal(t, "+QUEUED\r\n", s.send("GET", "k"))
	require.Nil(t, defaultEngine.dictStore.Peek("k"), "nothing runs before EXEC")
	require.Equal(t, "*3\r\n+OK\r\n:2\r\n$1\r\n2\r\n", s.send("EXEC"))
	require.Nil(t, s.tx)
	require.Equal(t, "2", run(t, "GET", "k"))
	require.Equal(t, "*0\r\n", func() string { s.send("MULTI"); return s.send("EXEC") }(),
		"an empty transaction answers an empty array")
}

func TestTransactionRefusalWhileQueueingAbortsIt(t *testing.T) {
	for name, refused := range map[string][]string{
		"unknown":     {"NOSUCHCOMMAND", "k"},
		"arity":       {"GET"},
		"exact arity": {"GET", "k", "extra"},
		"replication": {"KEEL.REPL.PULL", "", "0"},
		"term":        {"KEEL.FENCE", "9"},
		"keyless":     {"WATCH"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSession(t)
			s.send("MULTI")
			require.Equal(t, "+QUEUED\r\n", s.send("SET", "before", "v"))
			reply := s.send(refused...)
			require.True(t, strings.HasPrefix(reply, "-ERR "), reply)
			require.Equal(t, "+QUEUED\r\n", s.send("SET", "after", "v"), "a later valid command is still answered QUEUED")
			require.Zero(t, s.tx.RetainedBytes(), "a doomed transaction keeps nothing")
			require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
			require.Nil(t, s.tx)
			require.Nil(t, defaultEngine.dictStore.Peek("before"))
			require.Nil(t, defaultEngine.dictStore.Peek("after"))
		})
	}
	s := newSession(t)
	s.send("MULTI")
	require.Equal(t, "-ERR unknown command 'NOSUCHCOMMAND'\r\n", s.send("NOSUCHCOMMAND"))
	require.Equal(t, "-ERR wrong number of arguments for 'get' command\r\n", s.send("GET"))
	require.Equal(t, "-ERR Command not allowed inside a transaction\r\n", s.send("KEEL.PROMOTE", "1"))
}

func TestTransactionRuntimeErrorsDoNotStopTheRest(t *testing.T) {
	s := newSession(t)
	run(t, "SET", "text", "not a number")
	s.send("MULTI")
	s.send("INCR", "text")
	s.send("SET", "applied", "yes")
	s.send("LPUSH", "text", "x")
	s.send("GET", "applied")
	require.Equal(t, "*4\r\n-ERR value is not an integer or out of range\r\n+OK\r\n"+
		"-WRONGTYPE Operation against a key holding the wrong kind of value\r\n$3\r\nyes\r\n", s.send("EXEC"))
	require.Equal(t, "yes", run(t, "GET", "applied"))
	require.Equal(t, "not a number", run(t, "GET", "text"))
}

func TestTransactionControlCommands(t *testing.T) {
	s := newSession(t)
	require.Equal(t, "-ERR EXEC without MULTI\r\n", s.send("EXEC"))
	require.Equal(t, "-ERR DISCARD without MULTI\r\n", s.send("DISCARD"))
	require.Equal(t, "-ERR wrong number of arguments for 'multi' command\r\n", s.send("MULTI", "now"))
	require.Nil(t, s.tx)

	// A nested MULTI is an error that leaves the transaction open and intact.
	s.send("MULTI")
	s.send("SET", "k", "1")
	require.Equal(t, "-ERR MULTI calls can not be nested\r\n", s.send("MULTI"))
	require.Equal(t, "*1\r\n+OK\r\n", s.send("EXEC"))
	require.Equal(t, "1", run(t, "GET", "k"))

	s.send("MULTI")
	s.send("SET", "k", "2")
	require.Equal(t, "+OK\r\n", s.send("DISCARD"))
	require.Nil(t, s.tx)
	require.Equal(t, "1", run(t, "GET", "k"))
	require.Equal(t, "-ERR EXEC without MULTI\r\n", s.send("EXEC"), "DISCARD ends the transaction")

	// A malformed EXEC inside a transaction discards it and says why.
	s.send("MULTI")
	s.send("SET", "k", "3")
	require.Equal(t, "-EXECABORT Transaction discarded because of: wrong number of arguments for 'exec' command\r\n", s.send("EXEC", "now"))
	require.Nil(t, s.tx)
	require.Equal(t, "1", run(t, "GET", "k"))

	// A malformed DISCARD is refused like any command, and aborts.
	s.send("MULTI")
	s.send("SET", "k", "4")
	require.Equal(t, "-ERR wrong number of arguments for 'discard' command\r\n", s.send("DISCARD", "now"))
	require.NotNil(t, s.tx)
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
	require.Equal(t, "1", run(t, "GET", "k"))
}

// fakeConnection answers one command the way a transport answers AUTH.
type fakeConnection struct{ answered []string }

func (f *fakeConnection) RESP3() bool { return false }
func (f *fakeConnection) AnswerConnection(cmd *Command, w io.ReadWriter) {
	f.answered = append(f.answered, cmd.Args[0])
	if cmd.Args[0] != "silent" {
		w.Write([]byte("+OK\r\n"))
	}
}

// The transport's own commands are queued and run in their place, as Redis
// queues AUTH; their count is checked while queueing like any other command's.
func TestTransactionQueuesTheTransportsOwnCommands(t *testing.T) {
	conn := &fakeConnection{}
	s := newSession(t)
	s.conn = conn
	s.send("MULTI")
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "k", "1"))
	require.Equal(t, "+QUEUED\r\n", s.send("AUTH", "secret"))
	require.Equal(t, "+QUEUED\r\n", s.send("AUTH", "silent"))
	require.Equal(t, "+QUEUED\r\n", s.send("GET", "k"))
	require.Empty(t, conn.answered, "nothing runs before EXEC")
	require.Equal(t, "*4\r\n+OK\r\n+OK\r\n-ERR command produced no reply\r\n$1\r\n1\r\n", s.send("EXEC"),
		"an answer missing from the array would shift every reply after it")
	require.Equal(t, []string{"secret", "silent"}, conn.answered)

	s.send("MULTI")
	require.Equal(t, "-ERR wrong number of arguments for 'auth' command\r\n", s.send("AUTH"))
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
	require.Len(t, conn.answered, 2)
}

// BGREWRITEAOF inside a transaction is scheduled, as Redis 8.10.1 schedules it
// (bgrewriteaofCommand with server.in_exec): EXEC answers "scheduled" in its
// place, INFO reports aof_rewrite_scheduled:1, and the rewrite starts once the
// transaction is over. So the block reaches the old log whole before any
// rewrite begins, and the rewritten log holds its effects without its frames.
func TestTransactionSchedulesARewriteAsRedisDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rewrite.aof")
	ResetStores()
	require.NoError(t, OpenAOF(path))
	// Closing cancels a rewrite still running without counting it as a
	// failed one, which would outlive this test on the default engine.
	t.Cleanup(func() { CloseAOF() })
	s := &session{t: t}
	run(t, "SET", "before", "0")
	s.send("MULTI")
	s.send("SET", "a", "1")
	s.send("BGREWRITEAOF")
	s.send("SET", "b", "2")
	require.Equal(t, "*3\r\n+OK\r\n+Background append only file rewriting scheduled\r\n+OK\r\n", s.send("EXEC"))
	require.False(t, RewriteActive(), "nothing starts in the middle of EXEC")
	require.Contains(t, infoPersistence(t), "aof_rewrite_scheduled:1\r\n")
	old, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(old), "MULTI", "nothing of the block was flushed while it ran")
	// The loop's flush writes the block, then starts the scheduled rewrite.
	require.NoError(t, FlushAOF())
	require.True(t, RewriteActive())
	require.Contains(t, infoPersistence(t), "aof_rewrite_scheduled:0\r\n")
	old, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(old), "*1\r\n$5\r\nMULTI\r\n"+string(appendCommand(nil, "SET", "a", "1"))+
		string(appendCommand(nil, "SET", "b", "2"))+"*1\r\n$4\r\nEXEC\r\n", "the old log holds the block whole")
	for n := 0; RewriteActive() && n < 1000; n++ {
		require.NoError(t, FlushAOF())
		waitForRewriteSync(t)
	}
	require.False(t, RewriteActive())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(body), "MULTI", "a rewrite writes state, not frames")
	require.NoError(t, CloseAOF())
	restart(t, path)
	require.Equal(t, "1", run(t, "GET", "a"))
	require.Equal(t, "2", run(t, "GET", "b"))
	require.Equal(t, "0", run(t, "GET", "before"))
}

// UNWATCH answers as Redis does with nothing watched: OK, and inside MULTI it
// is queued and answers OK in its slot.
func TestUnwatchAnswersOKAsWithNothingWatched(t *testing.T) {
	s := newSession(t)
	require.Equal(t, "+OK\r\n", s.send("UNWATCH"))
	require.Equal(t, "-ERR wrong number of arguments for 'unwatch' command\r\n", s.send("UNWATCH", "k"))
	s.send("MULTI")
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "k", "1"))
	require.Equal(t, "+QUEUED\r\n", s.send("UNWATCH"))
	require.Equal(t, "*2\r\n+OK\r\n+OK\r\n", s.send("EXEC"))
	s.send("MULTI")
	require.Equal(t, "-ERR wrong number of arguments for 'unwatch' command\r\n", s.send("UNWATCH", "k"))
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
}

// WATCH is not implemented. Outside a transaction it is an unknown command;
// inside one it gets Redis's refusal, which leaves the transaction open.
func TestWatchIsRefusedInsideMultiAsRedisRefusesIt(t *testing.T) {
	s := newSession(t)
	require.Equal(t, "ERR unknown command 'WATCH', with args beginning with: 'k' ", run(t, "WATCH", "k"))
	s.send("MULTI")
	s.send("SET", "k", "1")
	require.Equal(t, "-ERR WATCH inside MULTI is not allowed\r\n", s.send("WATCH", "k"))
	require.Equal(t, "*1\r\n+OK\r\n", s.send("EXEC"))
	require.Equal(t, "1", run(t, "GET", "k"))
	s.send("MULTI")
	require.Equal(t, "-ERR wrong number of arguments for 'watch' command\r\n", s.send("WATCH"))
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
}

func TestTransactionQueueLimitRefusesAndReleases(t *testing.T) {
	s := newSession(t)
	s.send("MULTI")
	value := strings.Repeat("v", 1<<20)
	refused := ""
	for i := 0; i < 32 && refused == ""; i++ {
		if reply := s.send("SET", "k"+strconv.Itoa(i), value); reply != "+QUEUED\r\n" {
			refused = reply
		}
		require.LessOrEqual(t, s.tx.RetainedBytes(), maxTransactionBytes+1024)
	}
	require.Equal(t, "-ERR transaction exceeds the 16 MiB queued command limit\r\n", refused)
	require.Zero(t, s.tx.RetainedBytes())
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "small", "v"))
	require.Zero(t, s.tx.RetainedBytes())
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
	require.Zero(t, data_structure.TotalKeys())
}

// EXEC cannot be bounded before it runs, so concurrent appends must never let a
// run holding it overlap a pending append: it waits at the drained barrier.
func TestTransactionCommandsTakeTheAppendBarrier(t *testing.T) {
	ResetStores()
	for _, name := range []string{"MULTI", "EXEC", "DISCARD"} {
		_, _, bounded := AppendAdmission([]*Command{{Cmd: "SET", Args: []string{"k", "v"}}, {Cmd: name}})
		require.False(t, bounded, name)
	}
}

// TestEveryCommandHasAnArity: the count is checked from the table before any
// command runs, so a command the table did not hold could never run, and an
// entry for a command nothing answers would be a name that is never unknown.
func TestEveryCommandHasAnArity(t *testing.T) {
	for name := range commandTable {
		_, counted := commandArity[name]
		require.True(t, counted, "%s has no arity", name)
	}
	for name := range commandArity {
		_, table := commandTable[name]
		require.True(t, table || connectionCommands[name] || IsTransactionCommand(name), "arity for unknown command %s", name)
	}
	for name, subcommands := range containerCommands {
		require.Contains(t, commandArity, name)
		for _, sub := range subcommands {
			require.Equal(t, strings.ToLower(sub.name), sub.name, "%s|%s is named as Redis names it", name, sub.name)
			require.True(t, sub.arity >= 2 || sub.arity <= -2, "%s|%s counts its container and itself", name, sub.name)
		}
	}
}

// TestCommandArityIsNeverStricterThanTheHandler: a count that refused what the
// handler would have accepted would refuse a valid command, and abort the
// transaction it was queued in. Wherever the table refuses, the handler run
// directly, past the table, must refuse too - in the same words, apart from
// a handler shared with an old name, which names the current one, and the
// replication pulls, which refuse a node not serving them first.
func TestCommandArityIsNeverStricterThanTheHandler(t *testing.T) {
	ownWords := map[string]bool{"MEMKV.DUMP": true, "MEMKV.RESTORE": true, "SRAND": true,
		"KEEL.REPL.PULL": true, "KEEL.REPL.PULL2": true}
	for name, arity := range commandArity {
		handler, table := commandTable[name]
		if !table || containerCommands[name] != nil {
			continue
		}
		limit := arity
		if limit < 0 {
			limit = -limit
		}
		for args := 0; args <= limit+2; args++ {
			if arityAccepts(arity, args) {
				continue
			}
			ResetStores()
			parts := make([]string, args)
			for i := range parts {
				parts[i] = "1"
			}
			reply := handler(defaultEngine, parts)
			require.True(t, bytes.HasPrefix(reply, []byte("-")), "%s with %d arguments: table refuses, handler answered %q", name, args, reply)
			if !ownWords[name] {
				require.Equal(t, string(Encode(wrongArguments(name), false)), string(reply), "%s with %d arguments", name, args)
			}
		}
	}
}

// TestDispatchCountsArgumentsFromTheTable: a command is counted before it runs,
// and before a replica or a type check looks at it, in Redis's words.
func TestDispatchCountsArgumentsFromTheTable(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "list", "a")
	require.Equal(t, "-ERR wrong number of arguments for 'get' command\r\n", string(rawReply(t, "GET")))
	require.Equal(t, "-ERR wrong number of arguments for 'hset' command\r\n", string(rawReply(t, "HSET", "list", "f")),
		"counted before the key's type is looked at")
	run(t, "SADD", "set", "a")
	require.Equal(t, "-ERR wrong number of arguments for 'lpop' command\r\n", string(rawReply(t, "LPOP", "set", "1", "2")),
		"LPOP's own upper bound comes before the key's type too")
	require.Equal(t, "-ERR wrong number of arguments for 'memory|usage' command\r\n", string(rawReply(t, "MEMORY", "usage")))
	require.Equal(t, "-ERR unknown subcommand 'nosuch'. Try MEMORY HELP.\r\n", string(rawReply(t, "MEMORY", "nosuch", "x")))
	require.Equal(t, "-ERR wrong number of arguments for 'memkv.dump' command\r\n", string(rawReply(t, "MEMKV.DUMP")),
		"an old name is counted under the name it was sent as")
}

func TestTransactionReplyCeiling(t *testing.T) {
	s := newSession(t)
	run(t, "SET", "large", strings.Repeat("x", 40<<20))
	s.send("MULTI")
	s.send("GET", "large")
	s.send("GET", "large")
	s.send("SET", "after", "v")
	reply := s.send("EXEC")
	require.True(t, strings.HasPrefix(reply, "*3\r\n$41943040\r\n"))
	require.True(t, strings.HasSuffix(reply, "\r\n-ERR reply exceeds the 64 MiB output limit\r\n+OK\r\n"),
		"the second read is refused before it is built, and the write after it still runs and answers")
	require.Equal(t, "v", run(t, "GET", "after"))
	require.Equal(t, MaxReplyBytes, defaultEngine.replyCeiling, "the ceiling is only lowered inside EXEC")
}

func TestTransactionReplyBeyondTheLimitClosesAfterRunning(t *testing.T) {
	// Replies no admission sees - many small ones - can still add up past the
	// output limit. The transaction runs whole and the connection is told so.
	commandTable["TEST.MEGABYTE"] = func(*Engine, []string) []byte { return bytes.Repeat([]byte("+"), 1<<20) }
	commandArity["TEST.MEGABYTE"] = 1
	indexCommands()
	t.Cleanup(func() { delete(commandTable, "TEST.MEGABYTE"); delete(commandArity, "TEST.MEGABYTE"); indexCommands() })
	ResetStores()
	tx, _ := Transact(nil, &Command{Cmd: "MULTI"}, &replyWriter{}, nil)
	for i := 0; i < 70; i++ {
		Transact(tx, &Command{Cmd: "TEST.MEGABYTE"}, &replyWriter{}, nil)
	}
	Transact(tx, &Command{Cmd: "SET", Args: []string{"last", "ran"}}, &replyWriter{}, nil)
	var w replyWriter
	tx, err := Transact(tx, &Command{Cmd: "EXEC"}, &w, nil)
	require.ErrorIs(t, err, ErrTransactionReplyTooLarge)
	require.Nil(t, tx)
	require.Empty(t, w.b)
	require.Equal(t, "ran", run(t, "GET", "last"))
}

func TestTransactionOnAReplica(t *testing.T) {
	oldReady, oldUpdated := defaultEngine.replicaReady, defaultEngine.replicaUpdated
	t.Cleanup(func() {
		defaultEngine.replicaReady, defaultEngine.replicaUpdated = oldReady, oldUpdated
	})
	s := newSession(t)
	withOptions(t, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
	defaultEngine.replicaReady, defaultEngine.replicaUpdated = true, time.Now()
	defaultEngine.replicaApplying = true
	run(t, "SET", "k", "from-primary")
	defaultEngine.replicaApplying = false

	s.send("MULTI")
	require.Equal(t, "-READONLY You can't write against a read only replica.\r\n", s.send("SET", "k", "local"))
	s.send("GET", "k")
	require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))

	s.send("MULTI")
	s.send("GET", "k")
	s.send("EXISTS", "k")
	require.Equal(t, "*2\r\n$12\r\nfrom-primary\r\n:1\r\n", s.send("EXEC"), "a replica runs a transaction of reads")

	// Losing the primary between queueing and EXEC refuses the whole of it.
	s.send("MULTI")
	s.send("GET", "k")
	defaultEngine.replicaUpdated = time.Now().Add(-time.Minute)
	require.Equal(t, "-EXECABORT Transaction discarded because of: MASTERDOWN replica has no recent primary state\r\n", s.send("EXEC"))
}

// TestReplicaAndFencedPrimaryNameAndCountFirst: a replica, even one without
// its primary, and a fenced primary refuse a command they do not have, or one
// with the wrong number of arguments, in those words before their own
// refusals, as Redis orders them - outside a transaction and while queueing.
func TestReplicaAndFencedPrimaryNameAndCountFirst(t *testing.T) {
	check := func(t *testing.T, refusal string) {
		t.Helper()
		var w replyWriter
		require.EqualError(t, EvalAndResponse(&Command{Cmd: "NOSUCH", Name: "nosuch", Args: []string{"x"}}, &w),
			"ERR unknown command 'nosuch', with args beginning with: 'x' ")
		require.Equal(t, "-ERR wrong number of arguments for 'set' command\r\n", string(rawReply(t, "SET", "k")))
		require.Equal(t, refusal, string(rawReply(t, "SET", "k", "v")))
		s := &session{t: t}
		s.send("MULTI")
		require.Equal(t, "-ERR unknown command 'NOSUCH'\r\n", s.send("NOSUCH"))
		require.Equal(t, "-ERR wrong number of arguments for 'set' command\r\n", s.send("SET", "k"))
		require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))
	}
	t.Run("replica without its primary", func(t *testing.T) {
		oldReady := defaultEngine.replicaReady
		t.Cleanup(func() { defaultEngine.replicaReady = oldReady })
		ResetStores()
		withOptions(t, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
		defaultEngine.replicaReady = false
		check(t, "-READONLY You can't write against a read only replica.\r\n")
		require.Equal(t, "-MASTERDOWN replica has no recent primary state\r\n", string(rawReply(t, "GET", "k")))
	})
	t.Run("fenced primary", func(t *testing.T) {
		setupFailover(t)
		require.Equal(t, "OK", run(t, "KEEL.PROMOTE", "2"))
		require.Equal(t, "OK", run(t, "KEEL.FENCE", "3"))
		check(t, "-"+errFenced.Error()+"\r\n")
	})
}

func TestTransactionFencedBeforeExecRunsNothing(t *testing.T) {
	setupFailover(t)
	s := &session{t: t}
	s.send("MULTI")
	s.send("SET", "a", "1")
	s.send("SET", "b", "2")
	require.NoError(t, defaultEngine.observeTerm(7))
	require.Equal(t, "-EXECABORT Transaction discarded because of: "+errFenced.Error()+"\r\n", s.send("EXEC"))
	require.Nil(t, defaultEngine.dictStore.Peek("a"))
	require.Nil(t, defaultEngine.dictStore.Peek("b"))
}

// aofBody is the log's bytes once flushed.
func aofBody(t *testing.T) string {
	t.Helper()
	require.NoError(t, FlushAOF())
	body, err := os.ReadFile(defaultEngine.aof.path)
	require.NoError(t, err)
	return string(body)
}

func TestTransactionIsFramedInTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tx.aof")
	ResetStores()
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { CloseAOF() })
	s := &session{t: t}

	run(t, "SET", "outside", "1")
	s.send("MULTI")
	s.send("SET", "a", "1")
	s.send("GET", "a")
	s.send("INCR", "a")
	s.send("EXEC")
	want := string(appendCommand(nil, "SET", "outside", "1")) + "*1\r\n$5\r\nMULTI\r\n" +
		string(appendCommand(nil, "SET", "a", "1")) + string(appendCommand(nil, "INCR", "a")) + "*1\r\n$4\r\nEXEC\r\n"
	require.Equal(t, want, aofBody(t))

	// Reads, and writes that all fail, record nothing - and so frame nothing.
	run(t, "SET", "text", "x")
	before := aofBody(t)
	s.send("MULTI")
	s.send("GET", "a")
	s.send("INCR", "text")
	s.send("EXEC")
	require.Equal(t, before, aofBody(t))

	// A key reaped by a read inside the transaction is removed inside its block.
	run(t, "SET", "short", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	before = aofBody(t)
	s.send("MULTI")
	s.send("GET", "short")
	s.send("SET", "b", "2")
	s.send("EXEC")
	require.Equal(t, before+"*1\r\n$5\r\nMULTI\r\n"+string(appendCommand(nil, "DEL", "short"))+
		string(appendCommand(nil, "SET", "b", "2"))+"*1\r\n$4\r\nEXEC\r\n", aofBody(t))

	require.NoError(t, CloseAOF())
	restart(t, path)
	require.Equal(t, "2", run(t, "GET", "a"))
	require.Equal(t, "2", run(t, "GET", "b"))
	require.Equal(t, "1", run(t, "GET", "outside"))
	require.Nil(t, defaultEngine.dictStore.Peek("short"))
}

// A SET or MSET over a key another type holds replaces it, as Redis's does,
// and the DEL that replacement is logged as stays inside the transaction's
// block, in the log and in the protocol 2 stream alike.
func TestTransactionReplacingWritesStayInsideTheirBlock(t *testing.T) {
	setupReplicationV2(t)
	run(t, "HSET", "hash", "f", "v")
	run(t, "RPUSH", "list", "a")
	frames := snapshotV2(t)
	base, epoch := frames[0].To, frames[0].Epoch
	before := aofBody(t)
	s := &session{t: t}
	s.send("MULTI")
	s.send("SET", "hash", "string")
	s.send("MSET", "list", "x", "other", "y")
	require.Equal(t, "*2\r\n+OK\r\n+OK\r\n", s.send("EXEC"))
	block := "*1\r\n$5\r\nMULTI\r\n" + string(appendCommand(nil, "DEL", "hash")) +
		string(appendCommand(nil, "SET", "hash", "string")) + string(appendCommand(nil, "DEL", "list")) +
		string(appendCommand(nil, "MSET", "list", "x", "other", "y")) + "*1\r\n$4\r\nEXEC\r\n"
	require.Equal(t, before+block, aofBody(t))
	require.Equal(t, block, string(pullV2(t, epoch, base, "", 0).Body))
	path := defaultEngine.aof.path
	require.NoError(t, CloseAOF())
	restart(t, path)
	require.Equal(t, "string", run(t, "GET", "hash"))
	require.Equal(t, "x", run(t, "GET", "list"))
	require.Equal(t, "string", run(t, "TYPE", "list"))
}

func TestTransactionEvictsAfterItsBlock(t *testing.T) {
	t.Cleanup(func() { CloseAOF() })
	path := filepath.Join(t.TempDir(), "evict.aof")
	ResetStores()
	require.NoError(t, OpenAOF(path))
	value := strings.Repeat("v", 64<<10)
	for i := 0; i < 8; i++ {
		run(t, "SET", "old"+strconv.Itoa(i), value)
	}
	withOptions(t, func(o *Options) { o.MaxMemory = data_structure.TotalMemUsed() + 32<<10 })
	before := len(aofBody(t))
	s := &session{t: t}
	s.send("MULTI")
	s.send("SET", "new1", value)
	s.send("SET", "new2", value)
	s.send("EXEC")
	tail := aofBody(t)[before:]
	exec := strings.Index(tail, "*1\r\n$4\r\nEXEC\r\n")
	require.Positive(t, exec)
	require.NotContains(t, tail[:exec], "$3\r\nDEL\r\n", "no eviction inside the block")
	require.Contains(t, tail[exec:], "$3\r\nDEL\r\n", "the budget is enforced once the block is closed")
	require.LessOrEqual(t, data_structure.TotalMemUsed(), Configuration().MaxMemory)
}

func TestTransactionTornTailDropsTheWholeBlock(t *testing.T) {
	complete := string(appendCommand(nil, "SET", "kept", "1")) + "*1\r\n$5\r\nMULTI\r\n" +
		string(appendCommand(nil, "SET", "a", "1")) + string(appendCommand(nil, "SET", "b", "1")) + "*1\r\n$4\r\nEXEC\r\n"
	open := "*1\r\n$5\r\nMULTI\r\n" + string(appendCommand(nil, "SET", "a", "2")) + string(appendCommand(nil, "SET", "b", "2"))
	for name, tail := range map[string]string{
		"only MULTI":     "*1\r\n$5\r\nMULTI\r\n",
		"torn MULTI":     "*1\r\n$5\r\nMUL",
		"no EXEC":        open,
		"torn command":   open[:len(open)-6],
		"torn EXEC":      open + "*1\r\n$4\r\nEX",
		"EXEC unwritten": open + "*1\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "torn.aof")
			require.NoError(t, os.WriteFile(path, []byte(complete+tail), 0o644))
			ResetStores()
			_, err := LoadAOF(path)
			require.True(t, IsTruncatedAOF(err), "%v", err)
			if strings.HasPrefix(tail, "*1\r\n$5\r\nMULTI\r\n") {
				require.ErrorContains(t, err, "incomplete final transaction")
			}
			require.Equal(t, "1", run(t, "GET", "a"), "only the complete block was replayed")
			require.Equal(t, "1", run(t, "GET", "b"))
			require.NoError(t, RepairAOFTail(err))
			body, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, complete, string(body), "truncated to before the open block's MULTI")
			backups, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".keel-torn-tail-*"))
			require.NoError(t, globErr)
			require.Len(t, backups, 1)
			saved, readErr := os.ReadFile(backups[0])
			require.NoError(t, readErr)
			require.Equal(t, tail, string(saved), "the whole open block is preserved beside the log")

			ResetStores()
			applied, err := LoadAOF(path)
			require.NoError(t, err)
			require.Equal(t, 3, applied)
		})
	}
}

func TestTransactionFramesThatCannotBeTornRefuseStartup(t *testing.T) {
	set := string(appendCommand(nil, "SET", "a", "1"))
	for name, body := range map[string]string{
		"EXEC without MULTI": set + "*1\r\n$4\r\nEXEC\r\n" + set,
		"nested MULTI":       "*1\r\n$5\r\nMULTI\r\n" + set + "*1\r\n$5\r\nMULTI\r\n" + set + "*1\r\n$4\r\nEXEC\r\n",
		"MULTI with args":    "*2\r\n$5\r\nMULTI\r\n$1\r\nx\r\n" + set,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.aof")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			ResetStores()
			_, err := LoadAOF(path)
			require.Error(t, err)
			require.False(t, IsTruncatedAOF(err), "a frame out of place is damage, not a torn tail")
		})
	}
}

func TestReplicationV2DeliversATransactionWhole(t *testing.T) {
	setupReplicationV2(t)
	run(t, "SET", "seed", "v")
	frames := snapshotV2(t)
	base, epoch := frames[0].To, frames[0].Epoch

	// Reads publish nothing.
	s := &session{t: t}
	s.send("MULTI")
	s.send("GET", "seed")
	s.send("EXEC")
	require.Equal(t, base, defaultEngine.replicationV2.end)

	// A block larger than one frame, so a replica receives it in pieces.
	large := strings.Repeat("x", 300<<10)
	s.send("MULTI")
	s.send("SET", "first", large)
	s.send("BF.ADD", "filter", "member")
	s.send("SET", "last", large)
	s.send("EXEC")
	var deltas []ReplicationFrame
	for offset := base; offset < defaultEngine.replicationV2.end; {
		f := pullV2(t, epoch, offset, "", 0)
		deltas = append(deltas, f)
		offset = f.To
	}
	require.Greater(t, len(deltas), 2)
	stream := ""
	for _, f := range deltas {
		stream += string(f.Body)
	}
	require.True(t, strings.HasPrefix(stream, "*1\r\n$5\r\nMULTI\r\n"))
	require.True(t, strings.HasSuffix(stream, "*1\r\n$4\r\nEXEC\r\n"))
	require.Contains(t, stream, "KEEL.RESTORE", "an opaque command publishes its image inside the block")

	path := becomeReplicaV2(t)
	for _, f := range frames {
		require.NoError(t, ApplyReplication(f))
	}
	require.NoError(t, ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: base, To: base, CaughtUp: true})))
	for _, f := range deltas[:len(deltas)-1] {
		require.NoError(t, ApplyReplication(f))
		require.Nil(t, defaultEngine.dictStore.Peek("first"), "no part of a transaction is visible before all of it")
		require.Equal(t, int64(0), run(t, "EXISTS", "filter"))
	}
	require.NoError(t, ApplyReplication(deltas[len(deltas)-1]))
	require.Equal(t, large, run(t, "GET", "first"))
	require.Equal(t, large, run(t, "GET", "last"))
	require.Equal(t, int64(1), run(t, "BF.EXISTS", "filter", "member"))
	require.True(t, defaultEngine.replicaReady)

	// The replica frames the block in its own log, and replays it.
	require.NoError(t, CloseAOF())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(body), "*1\r\n$5\r\nMULTI\r\n")
	require.Contains(t, string(body), "*1\r\n$4\r\nEXEC\r\n")
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, large, run(t, "GET", "last"))
}

func TestReplicationV2OversizedTransactionTakesASnapshot(t *testing.T) {
	setupReplicationV2(t)
	// A filter publishes its whole image for every command that changes it, so
	// two small commands can make a block larger than the history.
	run(t, "BF.RESERVE", "filter", "0.001", "7000000")
	frames := snapshotV2(t)
	epoch := frames[0].Epoch
	s := &session{t: t}
	s.send("MULTI")
	s.send("SET", "before", "v")
	s.send("BF.ADD", "filter", "a")
	s.send("BF.ADD", "filter", "b")
	s.send("SET", "after", "v")
	require.Equal(t, "*4\r\n+OK\r\n:1\r\n:1\r\n+OK\r\n", s.send("EXEC"))
	require.NotEqual(t, epoch, defaultEngine.replication.epoch, "a block the history cannot hold whole invalidates the stream")
	require.Zero(t, defaultEngine.replicationV2.end, "and none of it reaches the new epoch's history")
	run(t, "SET", "later", "v")
	require.Equal(t, string(appendCommand(nil, "SET", "later", "v")), string(defaultEngine.replicationV2.history[0].body))
	require.True(t, pullV2(t, epoch, frames[0].To, "", 0).Pending)
}

func TestReplicationV2RefusesMalformedTransactions(t *testing.T) {
	set := string(appendCommand(nil, "SET", "k", "v"))
	for name, body := range map[string]string{
		"EXEC without MULTI": set + "*1\r\n$4\r\nEXEC\r\n",
		"nested MULTI":       "*1\r\n$5\r\nMULTI\r\n*1\r\n$5\r\nMULTI\r\n" + set + "*1\r\n$4\r\nEXEC\r\n",
		"caught up inside":   "*1\r\n$5\r\nMULTI\r\n" + set,
	} {
		t.Run(name, func(t *testing.T) {
			setupReplicationV2(t)
			frames := snapshotV2(t)
			becomeReplicaV2(t)
			for _, f := range frames {
				require.NoError(t, ApplyReplication(f))
			}
			base := frames[0].To
			f := signedV2(ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: base, To: base + uint64(len(body)), Body: []byte(body), CaughtUp: true})
			require.Error(t, ApplyReplication(f))
			require.False(t, defaultEngine.replicaReady)
			require.Nil(t, defaultEngine.dictStore.Peek("k"))
		})
	}
}

func TestReplicationV1SealsATransactionTogether(t *testing.T) {
	// Runs last, once the options are back: the default engine ends feeding
	// no stream, with nothing of this one left.
	t.Cleanup(func() { require.NoError(t, InitReplication()) })
	t.Cleanup(func() { CloseAOF() })
	ResetStores()
	withOptions(t, func(o *Options) { o.ReplicationFeed, o.ReplicationProtocol = true, 1 })
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, InitReplication())
	pull := func(epoch string, offset uint64) ReplicationFrame {
		encoded, ok := run(t, "KEEL.REPL.PULL", epoch, strconv.FormatUint(offset, 10)).(string)
		require.True(t, ok)
		var f ReplicationFrame
		require.NoError(t, json.Unmarshal([]byte(encoded), &f))
		return f
	}
	first := pull("", 0)
	s := &session{t: t}
	s.send("MULTI")
	s.send("SET", "a", "1")
	s.send("SET", "b", "1")
	s.send("EXEC")
	// Protocol 1 sends key images sealed at a pull, and a pull runs between
	// commands, so a transaction's keys are always sealed into one batch.
	delta := pull(first.Epoch, first.To)
	require.False(t, delta.Full)
	require.Contains(t, string(delta.Body), string(appendCommand(nil, "SET", "a", "1")))
	require.Contains(t, string(delta.Body), string(appendCommand(nil, "SET", "b", "1")))
}

func TestTransactionReplayErrorNamesTheCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.aof")
	body := "*1\r\n$5\r\nMULTI\r\n" + string(appendCommand(nil, "NOSUCHCOMMAND")) + "*1\r\n$4\r\nEXEC\r\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	ResetStores()
	_, err := LoadAOF(path)
	require.Error(t, err)
	require.False(t, errors.Is(err, errTruncatedAOF))
	require.ErrorContains(t, err, "replaying NOSUCHCOMMAND at byte 15")
}
