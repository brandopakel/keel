package resp

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// words is what splitInline makes of line, or nil and false when its quotes
// do not balance. Both passes are run, and must agree.
func words(t *testing.T, line string) ([]string, bool) {
	t.Helper()
	var got []string
	argc, size, ok := splitInline([]byte(line), func(w []byte) { got = append(got, string(w)) })
	countArgc, countSize, countOK := splitInline([]byte(line), nil)
	require.Equal(t, ok, countOK, "%q", line)
	if !ok {
		return nil, false
	}
	require.Equal(t, argc, countArgc, "%q", line)
	require.Equal(t, size, countSize, "%q", line)
	require.Len(t, got, argc)
	n := 0
	for _, w := range got {
		n += len(w)
	}
	require.Equal(t, n, size)
	return got, true
}

// TestInlineWordsAreSplitAsRedisSplitsThem: each line is split as Redis's
// sdssplitargs splits it (sds.c), read case by case from its source.
func TestInlineWordsAreSplitAsRedisSplitsThem(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		line string
		want []string
	}{
		{"", nil},
		{"   \t ", nil},
		{"PING", []string{"PING"}},
		{"  set   k\tv  ", []string{"set", "k", "v"}},
		{"\v\fPING", []string{"PING"}},                        // isspace skips \v and \f between words
		{"SET k a\vb", []string{"SET", "k", "a\vb"}},          // but only space, \t, \r and \n end a bare word
		{"SET k a\rb", []string{"SET", "k", "a", "b"}},        // a \r inside the line separates
		{`SET k "a b"`, []string{"SET", "k", "a b"}},          // double quotes hold blanks
		{`SET k ""`, []string{"SET", "k", ""}},                // and may hold nothing
		{`SET k "\x41\x4a\x4A"`, []string{"SET", "k", "AJJ"}}, // hex escapes, either case
		{`SET k "\x4"`, []string{"SET", "k", "x4"}},           // \x without two hex digits is x
		{`SET k "\xZZ"`, []string{"SET", "k", "xZZ"}},         //
		{`SET k "\n\r\t\b\a"`, []string{"SET", "k", "\n\r\t\b\a"}},
		{`SET k "\"\\\q"`, []string{"SET", "k", `"\q`}},              // anything else escaped is itself
		{`SET k 'a "b'`, []string{"SET", "k", `a "b`}},               // single quotes hold double ones
		{`SET k 'it\'s'`, []string{"SET", "k", "it's"}},              // and escape only themselves
		{`SET k 'a\nb'`, []string{"SET", "k", `a\nb`}},               //
		{`SET k a"b c"`, []string{"SET", "k", "ab c"}},               // a quote may open mid-word
		{`SET k a'b c'`, []string{"SET", "k", "ab c"}},               //
		{`SET k \x41`, []string{"SET", "k", `\x41`}},                 // escapes mean nothing outside quotes
		{"PING\x00 ignored", []string{"PING"}},                       // a C string: NUL ends the line
		{"\x00PING", nil},                                            //
		{"SET k \xc3\xa9\xff", []string{"SET", "k", "\xc3\xa9\xff"}}, // bytes past ASCII are not blanks
		{`"" foo`, []string{"", "foo"}},
	} {
		got, ok := words(t, c.line)
		require.True(t, ok, "%q", c.line)
		require.Equal(t, c.want, got, "%q", c.line)
	}
}

// TestInlineQuotesThatDoNotBalanceAreRefused: an unclosed quote, a closing
// quote not followed by a blank, and a backslash that escapes the end are
// sdssplitargs's errors, and Redis's "unbalanced quotes in request".
func TestInlineQuotesThatDoNotBalanceAreRefused(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`SET k "abc`, `SET k 'abc`, `SET k "a"b`, `SET k 'a'b`, `SET k "abc\`, `SET k "a\"`,
		`set """test-key""" test-value`, // Redis's own case (tests/unit/protocol.tcl)
	} {
		_, ok := words(t, line)
		require.False(t, ok, "%q", line)
		_, _, err := ParseInlineReserved([]byte(line+"\r\n"), nil)
		require.ErrorIs(t, err, ErrUnbalancedQuotes, "%q", line)
		require.ErrorIs(t, err, ErrProtocol, "a protocol error, refused and the connection closed")
		require.Equal(t, "ERR Protocol error: unbalanced quotes in request", err.Error())
	}
	_, ok := words(t, "SET k \"a\x00b\"")
	require.False(t, ok, "a NUL ends the string inside the quotes, though no line holding one reaches the split")
	_, ok = words(t, `SET k "a"	b`)
	require.True(t, ok, "a closing quote may be followed by any blank")
}

// TestInlineRequestsAreFramedAsRedisFramesThem: a request ends at its first
// newline, with or without a carriage return before it; a line with no words
// is skipped; and a line longer than 64 KiB is refused.
func TestInlineRequestsAreFramedAsRedisFramesThem(t *testing.T) {
	t.Parallel()
	parse := func(wire string) (*Command, int, error) {
		return ParseInlineReserved([]byte(wire), func(int) bool { return true })
	}
	cmd, n, err := parse("get k\r\nPING\r\n")
	require.NoError(t, err)
	require.Equal(t, 7, n)
	require.Equal(t, "GET", cmd.Cmd)
	require.Equal(t, "get", cmd.Name, "the name as sent, for an unknown command's error")
	require.Equal(t, []string{"k"}, cmd.Args)

	cmd, n, err = parse("PING\nGET k")
	require.NoError(t, err)
	require.Equal(t, 5, n, "a newline alone ends the line")
	require.Equal(t, "PING", cmd.Cmd)
	require.Empty(t, cmd.Args)

	for _, wire := range []string{"\r\n", "\n", "   \r\n", "\v\f\t\r\n"} {
		cmd, n, err = parse(wire)
		require.NoError(t, err, "%q", wire)
		require.Nil(t, cmd, "%q is skipped", wire)
		require.Equal(t, len(wire), n)
	}

	_, _, err = parse("SET k v")
	require.ErrorIs(t, err, ErrIncompleteFrame, "no newline yet")
	for _, wire := range []string{"PING\x00\r\n", "\x00\r\nPING\r\n", "SET k \"a\x00b\"\r\n"} {
		_, _, err = parse(wire)
		require.ErrorIs(t, err, ErrIncompleteFrame, "%q: a NUL hides the newline, as strchr stops at it", wire)
		_, _, err = parse(wire + strings.Repeat("x", InlineMaxSize))
		require.ErrorIs(t, err, ErrTooBigInline, "%q: until the limit refuses the line", wire)
	}
	cmd, _, err = parse("PING\r\nGET \x00")
	require.NoError(t, err, "a NUL in a later line hides nothing of this one")
	require.Equal(t, "PING", cmd.Cmd)
	_, _, err = parse("SET k \r")
	require.ErrorIs(t, err, ErrIncompleteFrame)

	long := "SET k " + strings.Repeat("x", InlineMaxSize-7)
	cmd, n, err = parse(long + "\r\n")
	require.NoError(t, err, "64 KiB before the newline, the \\r included, is the longest line read")
	require.Equal(t, len(long)+2, n)
	require.Len(t, cmd.Args[1], InlineMaxSize-7)
	_, _, err = parse(long + "\r")
	require.ErrorIs(t, err, ErrIncompleteFrame, "64 KiB without a newline still waits")
	_, _, err = parse(long + "xx")
	require.ErrorIs(t, err, ErrTooBigInline, "one byte more without a newline is refused")
	require.Equal(t, "ERR Protocol error: too big inline request", err.Error())
	_, _, err = parse(long + "xx\r\n")
	require.ErrorIs(t, err, ErrTooBigInline, "and so is a longer line whose newline has arrived")
	require.ErrorIs(t, err, ErrProtocol)
}

// TestOnlyClientsSpeakInline: the parser that the log's replay and the
// replica link use reads RESP alone, as Redis's replica refuses inline
// commands from its primary (networking.c, processInlineBuffer) and its AOF
// loader a record that does not start with '*'.
func TestOnlyClientsSpeakInline(t *testing.T) {
	t.Parallel()
	for _, wire := range []string{"PING\r\n", "SET k v\r\n", "\r\n", "\n"} {
		_, _, err := ParseCmd([]byte(wire))
		require.ErrorIs(t, err, ErrProtocol, "%q", wire)
	}
}

// TestInlineRequestsAreAdmittedBeforeTheyAllocate: the budget is asked
// before the command is built, and a refusal allocates nothing of it.
func TestInlineRequestsAreAdmittedBeforeTheyAllocate(t *testing.T) {
	t.Parallel()
	wire := []byte("SET key " + strings.Repeat("v", 1000) + "\r\n")
	var charged int
	cmd, _, err := ParseInlineReserved(wire, func(n int) bool { charged = n; return true })
	require.NoError(t, err)
	require.Equal(t, "SET", cmd.Cmd)
	require.Greater(t, charged, 1000+80)
	cmd, n, err := ParseInlineReserved(wire, func(int) bool { return false })
	require.ErrorIs(t, err, ErrRequestAllocation)
	require.Nil(t, cmd)
	require.Zero(t, n)
}

// TestInlineCommandsOwnTheirBytes: nothing of the parsed command points into
// the buffer it was read from, which the connection reuses.
func TestInlineCommandsOwnTheirBytes(t *testing.T) {
	t.Parallel()
	wire := []byte("set key \"value one\"\r\n")
	cmd, _, err := ParseInlineReserved(wire, nil)
	require.NoError(t, err)
	clear(wire)
	require.Equal(t, "SET", cmd.Cmd)
	require.Equal(t, []string{"key", "value one"}, cmd.Args)
}

// repr quotes s as Redis's sdscatrepr does, which is how redis-cli prints a
// value and what it accepts back.
func repr(s []byte) []byte {
	out := []byte{'"'}
	for _, c := range s {
		switch c {
		case '\\', '"':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		case '\a':
			out = append(out, '\\', 'a')
		case '\b':
			out = append(out, '\\', 'b')
		default:
			if c < 0x20 || c > 0x7e {
				out = append(out, '\\', 'x', "0123456789abcdef"[c>>4], "0123456789abcdef"[c&15])
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}

// FuzzInlineSplitIsConsistent: for any line, the counting pass and the
// emitting pass agree, the parsed command holds what was emitted, and
// nothing panics or reads past the line.
func FuzzInlineSplitIsConsistent(f *testing.F) {
	for _, seed := range []string{
		"PING", `SET k "a b"`, `SET k "\x41\x4"`, `SET k 'it\'s'`, `a"b c"`, `"abc`, `"a"b`, "a\vb\fc",
		"\x00", "PING\x00x", `"\`, `'\`, `"\x`, "\"\\x4", strings.Repeat(`"x" `, 50),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		var emitted [][]byte
		argc, size, ok := splitInline(line, func(w []byte) { emitted = append(emitted, append([]byte(nil), w...)) })
		countArgc, countSize, countOK := splitInline(line, nil)
		if ok != countOK || argc != countArgc || size != countSize {
			t.Fatalf("passes disagree on %q: %d/%d/%v and %d/%d/%v", line, argc, size, ok, countArgc, countSize, countOK)
		}
		if !ok {
			return
		}
		if len(emitted) != argc {
			t.Fatalf("%q: %d words emitted, %d counted", line, len(emitted), argc)
		}
		if bytes.IndexByte(line, '\n') >= 0 {
			return
		}
		cmd, n, err := ParseInlineReserved(append(append([]byte(nil), line...), '\n'), nil)
		if bytes.IndexByte(line, 0) >= 0 {
			// The NUL hides the newline, as it does from Redis's strchr.
			want := ErrIncompleteFrame
			if len(line)+1 > InlineMaxSize {
				want = ErrTooBigInline
			}
			if !errors.Is(err, want) {
				t.Fatalf("%q: %v, want %v", line, err, want)
			}
			return
		}
		if len(line) > InlineMaxSize {
			if !errors.Is(err, ErrTooBigInline) {
				t.Fatalf("%d bytes: %v", len(line), err)
			}
			return
		}
		if err != nil || n != len(line)+1 {
			t.Fatalf("%q: %v, consumed %d", line, err, n)
		}
		if argc == 0 {
			if cmd != nil {
				t.Fatalf("%q: no words, yet %#v", line, cmd)
			}
			return
		}
		if cmd.SentName() != string(emitted[0]) || len(cmd.Args) != argc-1 {
			t.Fatalf("%q: %#v from %q", line, cmd, emitted)
		}
		for i, arg := range cmd.Args {
			if arg != string(emitted[i+1]) {
				t.Fatalf("%q: argument %d is %q, emitted %q", line, i, arg, emitted[i+1])
			}
		}
	})
}

// FuzzInlineQuotedWordsRoundTrip: any words, quoted as redis-cli prints them
// (sdscatrepr), are read back as themselves.
func FuzzInlineQuotedWordsRoundTrip(f *testing.F) {
	f.Add([]byte("SET"), []byte("k"), []byte("v"))
	f.Add([]byte("set"), []byte("a b"), []byte("\x00\xff\"\\\n\r\t\a\b'"))
	f.Add([]byte(""), []byte(""), []byte(`\x41`))
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		line := bytes.Join([][]byte{repr(a), repr(b), repr(c)}, []byte(" "))
		got, ok := func() ([]string, bool) {
			var out []string
			_, _, ok := splitInline(line, func(w []byte) { out = append(out, string(w)) })
			return out, ok
		}()
		if !ok {
			t.Fatalf("%q did not balance", line)
		}
		want := []string{string(a), string(b), string(c)}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("%q read back as %q, want %q", line, got, want)
		}
	})
}
