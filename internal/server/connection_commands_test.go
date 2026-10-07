package server

import (
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

// sent is a command as a client spelled it, its name upper-cased the way the
// decoder upper-cases it.
func sent(parts ...string) *core.Command {
	cmd := command(parts...)
	cmd.Cmd, cmd.Name = upper(parts[0]), parts[0]
	return cmd
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// clientWithPassword is a connection accepted by a server whose RequirePass
// is password, on an engine of its own with an empty keyspace.
func clientWithPassword(t *testing.T, password string) *client {
	return &client{fd: -1, password: password, engine: newTestEngine(t, core.Options{})}
}

// TestCommandsAreNamedAndCountedBeforeAuthentication, as Redis 8.10.1 answers
// a connection that has not logged in: a command it does not have, a
// subcommand a container does not have, and the wrong number of arguments are
// refused as such, ahead of NOAUTH; anything else is NOAUTH, and EXEC is
// refused with EXECABORT.
func TestCommandsAreNamedAndCountedBeforeAuthentication(t *testing.T) {
	c := clientWithPassword(t, "secret")
	for _, tc := range []struct {
		cmd  *core.Command
		want string
	}{
		{sent("nosuch", "a"), "-ERR unknown command 'nosuch', with args beginning with: 'a' \r\n"},
		{sent("nosuch"), "-ERR unknown command 'nosuch'\r\n"},
		{sent("get"), "-ERR wrong number of arguments for 'get' command\r\n"},
		{sent("get", "k"), "-NOAUTH Authentication required.\r\n"},
		{sent("client", "nosuch"), "-ERR unknown subcommand 'nosuch'. Try CLIENT HELP.\r\n"},
		{sent("client"), "-ERR wrong number of arguments for 'client' command\r\n"},
		{sent("client", "id", "x"), "-ERR wrong number of arguments for 'client|id' command\r\n"},
		{sent("client", "id"), "-NOAUTH Authentication required.\r\n"},
		{sent("memory", "usage"), "-ERR wrong number of arguments for 'memory|usage' command\r\n"},
		{sent("memory", "nosuch"), "-ERR unknown subcommand 'nosuch'. Try MEMORY HELP.\r\n"},
		{sent("multi"), "-NOAUTH Authentication required.\r\n"},
		{sent("exec"), "-EXECABORT Transaction discarded because of: NOAUTH Authentication required.\r\n"},
		{sent("exec", "x"), "-EXECABORT Transaction discarded because of: wrong number of arguments for 'exec' command\r\n"},
		{sent("auth"), "-ERR wrong number of arguments for 'auth' command\r\n"},
		{sent("auth", "a", "b", "c"), "-ERR syntax error\r\n"},
		{sent("auth", "wrong"), "-WRONGPASS invalid username-password pair or user is disabled.\r\n"},
		{sent("auth", "someone", "secret"), "-WRONGPASS invalid username-password pair or user is disabled.\r\n"},
		{sent("hello", "3", "auth", "someone", "secret"), "-WRONGPASS invalid username-password pair or user is disabled.\r\n"},
	} {
		require.Equal(t, tc.want, runOnce(t, c, tc.cmd), "%s %v", tc.cmd.Name, tc.cmd.Args)
		require.False(t, c.authenticated)
	}
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("auth", "default", "secret")))
	// A failed attempt afterwards leaves the connection logged in, as Redis's does.
	require.Equal(t, "-WRONGPASS invalid username-password pair or user is disabled.\r\n", runOnce(t, c, sent("auth", "wrong")))
	require.True(t, c.authenticated)
	require.Equal(t, "$-1\r\n", runOnce(t, c, sent("get", "k")))
}

// TestAuthWithoutAPassword: Redis's default user takes any password when none
// is configured, and its one-argument AUTH refuses to pretend otherwise.
func TestAuthWithoutAPassword(t *testing.T) {
	c := clientWithPassword(t, "")
	require.Equal(t, "-ERR AUTH <password> called without any password configured for the default user. "+
		"Are you sure your configuration is correct?\r\n", runOnce(t, c, sent("AUTH", "x")))
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("AUTH", "default", "x")))
	require.Equal(t, "-WRONGPASS invalid username-password pair or user is disabled.\r\n", runOnce(t, c, sent("AUTH", "other", "x")))
	require.Equal(t, "-WRONGPASS invalid username-password pair or user is disabled.\r\n",
		runOnce(t, c, sent("HELLO", "3", "AUTH", "other", "x")))
	require.Equal(t, byte('%'), runOnce(t, c, sent("HELLO", "3", "AUTH", "default", "x"))[0])
}

// TestClientHelpAndSubcommandErrors: the subcommands are looked up as Redis
// looks them up, and HELP lists the ones there are.
func TestClientHelpAndSubcommandErrors(t *testing.T) {
	c := clientWithPassword(t, "")
	require.Equal(t, "-ERR unknown subcommand 'kill'. Try CLIENT HELP.\r\n", runOnce(t, c, sent("CLIENT", "kill", "x", "y")))
	require.Equal(t, "-ERR wrong number of arguments for 'client|setinfo' command\r\n", runOnce(t, c, sent("client", "SetInfo", "lib-name")))
	require.Equal(t, "-ERR Unrecognized option 'a'\r\n", runOnce(t, c, sent("CLIENT", "SETINFO", "a\x00b", "v")),
		"the option is repeated as C prints it, up to a NUL")
	help := runOnce(t, c, sent("client", "help"))
	require.Contains(t, help, "*15\r\n+CLIENT <subcommand> [<arg> [value] [opt] ...]. Subcommands are:\r\n+GETNAME\r\n")
	require.Contains(t, help, "+HELP\r\n+    Print this help.\r\n")
}
