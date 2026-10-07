package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubcommandsAreLookedUpAsRedisLooksThemUp, against Redis 8.10.1: a
// container's subcommand is found whatever its case, named in lower case in
// an arity error, and repeated as sent, NUL-ended and 128 bytes at most, when
// there is no such subcommand.
func TestSubcommandsAreLookedUpAsRedisLooksThemUp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"UsAge"}, "ERR wrong number of arguments for 'memory|usage' command"},
		{[]string{"stats", "x"}, "ERR wrong number of arguments for 'memory|stats' command"},
		{[]string{"Nosuch", "x"}, "ERR unknown subcommand 'Nosuch'. Try MEMORY HELP."},
		{[]string{"a\x00b"}, "ERR unknown subcommand 'a'. Try MEMORY HELP."},
		{[]string{"a\r\nb"}, "ERR unknown subcommand 'a  b'. Try MEMORY HELP."},
		{[]string{strings.Repeat("x", 200)}, "ERR unknown subcommand '" + strings.Repeat("x", 128) + "'. Try MEMORY HELP."},
	} {
		require.EqualError(t, CommandError(&Command{Cmd: "MEMORY", Args: c.args}), c.want)
	}
	require.EqualError(t, CommandError(&Command{Cmd: "MEMORY"}), "ERR wrong number of arguments for 'memory' command")
	require.NoError(t, CommandError(&Command{Cmd: "MEMORY", Args: []string{"usage", "k", "SAMPLES", "5"}}),
		"options past a subcommand's count are its own to read")
}

// TestEchoArgumentIsCsPercentS: Redis repeats some arguments whole, as C's
// %s prints them, ending at a NUL; the echo here is cut at 16 KiB as well, so
// an error never copies an argument the size of the query buffer.
func TestEchoArgumentIsCsPercentS(t *testing.T) {
	t.Parallel()
	require.Equal(t, "a", EchoArgument("a\x00b"))
	require.Equal(t, strings.Repeat("x", 200), EchoArgument(strings.Repeat("x", 200)))
	require.Len(t, EchoArgument(strings.Repeat("x", 1<<20)), argumentEchoLimit)
}

// TestHelpIsRedissForTheSubcommandsThereAre: MEMORY HELP is in Redis's form,
// simple strings in either protocol, so that "Try MEMORY HELP." has an answer.
func TestHelpIsRedissForTheSubcommandsThereAre(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, resp3 := range []bool{false, true} {
		help := string(rawReplyAsOn(t, e, resp3, "MEMORY", "help"))
		require.True(t, strings.HasPrefix(help, "*8\r\n+MEMORY <subcommand> [<arg> [value] [opt] ...]. Subcommands are:\r\n+STATS\r\n"), help)
		require.True(t, strings.HasSuffix(help, "+HELP\r\n+    Print this help.\r\n"), help)
	}
	require.Equal(t, "-ERR wrong number of arguments for 'memory|help' command\r\n", string(rawReplyOn(t, e, "MEMORY", "HELP", "x")))
}

// TestMemoryUsageReadsSamplesAsRedisDoes: SAMPLES takes a count that is not
// negative; anything else after the key is a syntax error.
func TestMemoryUsageReadsSamplesAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	require.Equal(t, rawReplyOn(t, e, "MEMORY", "USAGE", "k"), rawReplyOn(t, e, "MEMORY", "USAGE", "k", "samples", "0"))
	for args, want := range map[string]string{
		"BAD":         "-ERR syntax error\r\n",
		"SAMPLES":     "-ERR syntax error\r\n",
		"SAMPLES -1":  "-ERR syntax error\r\n",
		"SAMPLES x":   "-ERR value is not an integer or out of range\r\n",
		"SAMPLES 5 X": "-ERR syntax error\r\n",
	} {
		require.Equal(t, want, string(rawReplyOn(t, e, "MEMORY", append([]string{"USAGE", "k"}, strings.Fields(args)...)...)), args)
	}
}

// TestInfoTakesSectionsAsRedisDoes: any number of sections, a name that is
// not one adding nothing.
func TestInfoTakesSectionsAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	both := runOn(t, e, "INFO", "server", "KEYSPACE").(string)
	require.Contains(t, both, "# Server\r\n")
	require.Contains(t, both, "# Keyspace\r\n")
	require.NotContains(t, both, "# Memory\r\n")
	require.Equal(t, "", runOn(t, e, "INFO", "nosuch", "other"))
	require.Equal(t, infoSteady(runOn(t, e, "INFO").(string)), infoSteady(runOn(t, e, "INFO", "everything").(string)))
}
