package core

import (
	"bytes"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMSETEXKeysBoundsTheCountWithoutOverflow: dispatch finds an MSETEX's
// keys (commandKeys, through keyCountedPairs) before the command reads its
// count, so a count no MSETEX accepts must find no keys, whatever its size.
// A count doubled before it was bounded wrapped negative for 2^62 and
// 2^63-1, passed the bound, and made a slice of that capacity, which
// panicked; one MSETEX from any client was enough.
func TestMSETEXKeysBoundsTheCountWithoutOverflow(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{math.MaxInt64, 1 << 62, 1 << 31, math.MaxInt32 + 1, math.MaxInt32, 2} {
		args := []string{strconv.FormatInt(n, 10), "k", "v"}
		require.NotPanics(t, func() { require.Nil(t, msetexKeys(args), "count %d", n) })
		cmd := &Command{Cmd: "MSETEX", Args: args}
		require.NotPanics(t, func() { require.Empty(t, commandKeys(cmd), "count %d", n) })
	}
	require.Equal(t, []string{"a", "b"}, msetexKeys([]string{"2", "a", "1", "b", "2", "EX", "10"}))
	require.Equal(t, []string{"k"}, msetexKeys([]string{"1", "k", "v"}))
}

// TestMSETEXAnswersAHugeCountAsRedisDoes: the command itself, sent with a
// count near the int64 limit, answers Redis's refusal rather than taking the
// engine down, and writes nothing.
func TestMSETEXAnswersAHugeCountAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	run := func(args ...string) string {
		var out bytes.Buffer
		require.NoError(t, e.EvalAndResponse(&Command{Cmd: args[0], Args: args[1:]}, &out))
		return out.String()
	}
	for _, n := range []string{"9223372036854775807", "4611686018427387904"} {
		require.Equal(t, "-ERR invalid numkeys value\r\n", run("MSETEX", n, "k", "v"))
		require.Equal(t, "-ERR invalid numkeys value\r\n", run("MSETEX", n, "k", "v", "EX", "1"))
	}
	require.Equal(t, ":0\r\n", run("EXISTS", "k"))
}
