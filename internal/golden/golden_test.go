package golden

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFixtureRoundTrips(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("v", TokenLimit+1)
	cases := []Case{{Name: "strings/set", Steps: []Step{
		{Args: []string{"SET", "k", "a b"}, Replies: [2]string{"+OK\r\n", "+OK\r\n"}, Records: [][]string{{"SET", "k", "a b"}}},
		{Directive: "sleep 5ms"},
		{Directive: "expire-cycle", Records: [][]string{{"DEL", "brief"}}},
		{Args: []string{"SMEMBERS", "s"}, Flags: []string{FlagSorted}, Replies: [2]string{"*0\r\n", "~0\r\n"}},
		{Args: []string{"GET", "big"}, Replies: [2]string{long, long}},
		{Args: []string{"SET", "big", long}, Replies: [2]string{"+OK\r\n", "+OK\r\n"}, Records: [][]string{{"SET", "big", long}}},
	}}, {Name: "keys/empty", Steps: []Step{{Args: []string{"EXISTS", "é\x00\r\n"}, Replies: [2]string{":0\r\n", ":0\r\n"}}}}}
	var b bytes.Buffer
	require.NoError(t, Write(&b, cases))
	require.Contains(t, b.String(), "> \"SMEMBERS\" \"s\" @sorted\n2 \"*0\\r\\n\"\n3 \"~0\\r\\n\"\n")
	require.Contains(t, b.String(), "! expire-cycle\n~ \"DEL\" \"brief\"\n")
	require.Contains(t, b.String(), "> \"SET\" \"big\" "+Digest(long)+"\n2 \"+OK")
	require.Contains(t, b.String(), "> \"GET\" \"big\"\n2 "+Digest(long)+"\n3 "+Digest(long)+"\n")
	got, err := Parse(&b)
	require.NoError(t, err)
	// A long string is read back as its digest, which is how a harness holds
	// it (Held) before comparing.
	held := cases
	held[0].Steps[4].Replies = [2]string{Digest(long), Digest(long)}
	held[0].Steps[5].Args[2] = Digest(long)
	held[0].Steps[5].Records[0][2] = Digest(long)
	require.Equal(t, held, got)
	require.Equal(t, Digest(long), Held(long))
	require.Equal(t, "short", Held("short"))
}

func TestParseRefusesMalformedLines(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"> GET \"k\"\n", "case a\n2 \"x\"\n", "case a\n> GET k\n", "case a\n> GET \"k\" @sorted \"x\"\n", "case a\nwhat\n",
		"case a\n! sleep 1ms\n2 \"+OK\\r\\n\"\n", "case a\n~ \"DEL\" \"k\"\n"} {
		_, err := Parse(strings.NewReader(text))
		require.Error(t, err, "%q", text)
	}
}

func TestNormalizers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reply, want string
		flags       []string
	}{
		{"*3\r\n$1\r\nc\r\n$1\r\na\r\n$1\r\nb\r\n", "*3\r\n$1\r\na\r\n$1\r\nb\r\n$1\r\nc\r\n", []string{FlagSorted}},
		{"~2\r\n$1\r\nb\r\n$1\r\na\r\n", "~2\r\n$1\r\na\r\n$1\r\nb\r\n", []string{FlagSorted}},
		// SCAN: the cursor and the keys are not reordered, the keys are.
		{"*2\r\n$1\r\n0\r\n*2\r\n$1\r\nb\r\n$1\r\na\r\n", "*2\r\n$1\r\n0\r\n*2\r\n$1\r\na\r\n$1\r\nb\r\n", []string{FlagSorted}},
		// HGETALL: pairs stay together, in RESP2's flat array and RESP3's map.
		{"*4\r\n$1\r\nb\r\n$1\r\n2\r\n$1\r\na\r\n$1\r\n1\r\n", "*4\r\n$1\r\na\r\n$1\r\n1\r\n$1\r\nb\r\n$1\r\n2\r\n", []string{FlagPairs}},
		{"%2\r\n$1\r\nb\r\n:2\r\n$1\r\na\r\n:1\r\n", "%2\r\n$1\r\na\r\n:1\r\n$1\r\nb\r\n:2\r\n", []string{FlagPairs}},
		{"%2\r\n+a\r\n:5\r\n+b\r\n:6\r\n", "%2\r\n+a\r\n:0\r\n+b\r\n:0\r\n", []string{FlagValues}},
		{"*4\r\n+a\r\n:5\r\n+b\r\n:6\r\n", "*4\r\n+a\r\n:0\r\n+b\r\n:0\r\n", []string{FlagValues}},
		{"=10\r\ntxt:hello\r\n", "=", []string{FlagMasked}},
		{"*4\r\n+Size\r\n:5\r\n+Items\r\n:6\r\n", "*4\r\n+Size\r\n:0\r\n+Items\r\n:6\r\n", []string{FlagZero + "Size"}},
		{"%2\r\n$4\r\nSize\r\n:5\r\n+Items\r\n:6\r\n", "%2\r\n$4\r\nSize\r\n:0\r\n+Items\r\n:6\r\n", []string{FlagZero + "Size"}},
		{":3599\r\n", ":3600\r\n", []string{FlagTTLSeconds}},
		{":7199500\r\n", ":7200000\r\n", []string{FlagTTLMillis}},
		{":-2\r\n", ":-2\r\n", []string{FlagTTLSeconds}},
		{"_\r\n", "_\r\n", []string{FlagSorted, FlagPairs}},
		{"*-1\r\n", "*-1\r\n", []string{FlagSorted}},
		{"-ERR x\r\n", "-ERR x\r\n", []string{FlagSorted}},
		// An array of pairs is not reordered: its elements are not scalars.
		{"*2\r\n*2\r\n$1\r\nb\r\n,1\r\n*2\r\n$1\r\na\r\n,2\r\n", "*2\r\n*2\r\n$1\r\nb\r\n,1\r\n*2\r\n$1\r\na\r\n,2\r\n", []string{FlagSorted}},
	} {
		got, err := Normalize([]byte(tc.reply), tc.flags)
		require.NoError(t, err, "%q", tc.reply)
		require.Equal(t, tc.want, string(got), "%q %v", tc.reply, tc.flags)
	}
	_, err := Normalize([]byte("+OK\r\n"), []string{"nosuch"})
	require.Error(t, err)
	_, err = Normalize([]byte("+OK\r\n"), []string{""})
	require.Error(t, err)
	_, err = Normalize([]byte("+OK\r\n"), []string{FlagTTLSeconds})
	require.Error(t, err)
	require.Equal(t, [][]string{{"SREM", "s", "a", "b"}}, NormalizeRecords([][]string{{"SREM", "s", "b", "a"}}, []string{FlagRecordsSorted}))
}
