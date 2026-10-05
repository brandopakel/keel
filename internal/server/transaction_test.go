package server

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/core"
)

// runOnce executes c's parsed commands and returns what the run staged.
func runOnce(t *testing.T, c *client, cmds ...*core.Command) string {
	t.Helper()
	c.cmds = cmds
	var arena replyArena
	arena.reset()
	if !executeRun(c, &arena) {
		return ""
	}
	if c.inArena {
		return string(arena.buf[c.outStart:c.outEnd])
	}
	return string(c.out)
}

func command(parts ...string) *core.Command {
	return &core.Command{Cmd: parts[0], Args: parts[1:]}
}

// Queued commands are input the server holds for the connection, so they count
// against the same retained-input limits as unparsed bytes, and a closed
// connection gives them back without running any of them.
func TestQueuedCommandsAreAccountedAsInputAndDiscardedOnClose(t *testing.T) {
	core.ResetStores()
	t.Cleanup(core.ResetStores)
	r, _ := socketPair(t)
	c := &client{fd: r}
	clients[r] = c
	before := retainedInputBytes
	value := strings.Repeat("v", 1<<20)
	require.Equal(t, "+OK\r\n+QUEUED\r\n+QUEUED\r\n", runOnce(t, c,
		command("MULTI"), command("SET", "a", value), command("SET", "b", value)))
	require.True(t, accountClient(c))
	require.GreaterOrEqual(t, retainedInputBytes-before, 2<<20, "the queue is retained input")
	closeClient(c)
	require.Nil(t, c.tx)
	require.Equal(t, before, retainedInputBytes)
	var sink replyBuffer
	responseRw(command("EXISTS", "a", "b"), &sink)
	require.Equal(t, ":0\r\n", sink.buf.String(), "a transaction ends with its connection")
}

func TestTransactionPipelinedInOneRun(t *testing.T) {
	core.ResetStores()
	t.Cleanup(core.ResetStores)
	c := &client{fd: -1}
	got := runOnce(t, c, command("MULTI"), command("SET", "k", "1"), command("INCR", "k"),
		command("EXEC"), command("GET", "k"))
	require.Equal(t, "+OK\r\n+QUEUED\r\n+QUEUED\r\n*2\r\n+OK\r\n:2\r\n$1\r\n2\r\n", got)
	require.Nil(t, c.tx)
}

func TestAuthenticationStillGatesTransactions(t *testing.T) {
	c := clientWithPassword(t, "secret")
	require.Equal(t, "-NOAUTH Authentication required.\r\n", runOnce(t, c, command("MULTI")))
	require.Nil(t, c.tx)
	// EXEC is refused as Redis refuses it, naming why the transaction it
	// would have run is discarded.
	require.Equal(t, "-EXECABORT Transaction discarded because of: NOAUTH Authentication required.\r\n", runOnce(t, c, command("EXEC")))

	// AUTH inside a transaction is queued and runs in its place, as in Redis.
	// The commands queued while the connection was authenticated still run,
	// and a failed AUTH leaves the connection logged in, as it does outside
	// a transaction and in Redis.
	require.Equal(t, "+OK\r\n+OK\r\n+QUEUED\r\n+QUEUED\r\n+QUEUED\r\n+QUEUED\r\n"+
		"*4\r\n+OK\r\n+OK\r\n-WRONGPASS invalid username-password pair or user is disabled.\r\n+OK\r\n",
		runOnce(t, c, command("AUTH", "secret"), command("MULTI"), command("SET", "k", "v"),
			command("AUTH", "default", "secret"), command("AUTH", "wrong"), command("SET", "after", "v"), command("EXEC")))
	require.True(t, c.authenticated)
	require.Nil(t, c.tx)
	require.Equal(t, "$1\r\nv\r\n", runOnce(t, c, command("GET", "k")))
	var sink replyBuffer
	responseRw(command("EXISTS", "k", "after"), &sink)
	require.Equal(t, ":2\r\n", sink.buf.String())

	require.Equal(t, "+OK\r\n+OK\r\n-ERR wrong number of arguments for 'auth' command\r\n"+
		"-EXECABORT Transaction discarded because of previous errors.\r\n",
		runOnce(t, c, command("AUTH", "secret"), command("MULTI"), command("AUTH"), command("EXEC")))
}

// HELLO and CLIENT are queued and run in their place, as Redis runs them; QUIT
// is never queued, and closing the connection discards the transaction.
func TestConnectionCommandsInsideATransaction(t *testing.T) {
	core.ResetStores()
	t.Cleanup(core.ResetStores)
	c := &client{fd: -1, id: 7}
	got := runOnce(t, c, command("MULTI"), command("CLIENT", "SETNAME", "app"), command("HELLO", "2"),
		command("SELECT", "0"), command("ECHO", "hi"), command("CLIENT", "GETNAME"), command("EXEC"))
	require.True(t, strings.HasPrefix(got, "+OK\r\n+QUEUED\r\n+QUEUED\r\n+QUEUED\r\n+QUEUED\r\n+QUEUED\r\n*5\r\n+OK\r\n*14\r\n"), got)
	require.True(t, strings.HasSuffix(got, "+OK\r\n$2\r\nhi\r\n$3\r\napp\r\n"), got)
	require.Equal(t, "app", c.name)
	require.Nil(t, c.tx)

	require.Equal(t, "+OK\r\n-ERR wrong number of arguments for 'client' command\r\n"+
		"-EXECABORT Transaction discarded because of previous errors.\r\n",
		runOnce(t, c, command("MULTI"), command("CLIENT"), command("EXEC")))

	got = runOnce(t, c, command("MULTI"), command("SET", "k", "v"), command("QUIT"), command("EXEC"))
	require.Equal(t, "+OK\r\n+QUEUED\r\n+OK\r\n", got, "QUIT answers at once and nothing after it runs")
	require.True(t, c.closeAfterWrite)
	var sink replyBuffer
	responseRw(command("EXISTS", "k"), &sink)
	require.Equal(t, ":0\r\n", sink.buf.String())
}

// A transaction whose reply cannot be delivered has still run; its connection
// is closed, as any reply over the output limit closes one.
func TestUndeliverableTransactionReplyClosesTheConnection(t *testing.T) {
	oldRespond := respond
	t.Cleanup(func() { respond = oldRespond })
	respond = func(c *client, cmd *core.Command, w io.ReadWriter) {
		if cmd.Cmd == "EXEC" {
			c.err = core.ErrTransactionReplyTooLarge
			return
		}
		oldRespond(c, cmd, w)
	}
	for _, cmds := range [][]*core.Command{{command("EXEC")}, {command("PING"), command("EXEC"), command("PING")}} {
		c := &client{fd: -1}
		var arena replyArena
		arena.reset()
		c.cmds = cmds
		require.False(t, executeRun(c, &arena))
		require.ErrorIs(t, c.err, core.ErrTransactionReplyTooLarge)
		require.Empty(t, arena.buf, "nothing of the run is staged for a connection being closed")
	}
}
