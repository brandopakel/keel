package core_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/core"
)

// The wire forms below are the ones the RESP specification gives, so a case
// here is a check against the protocol rather than against this decoder.

func TestDecodeEachValueType(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want interface{}
	}{
		{"simple string", "+OK\r\n", "OK"},
		{"empty simple string", "+\r\n", ""},
		{"error carries its message", "-ERR something\r\n", "ERR something"},
		{"integer", ":1000\r\n", int64(1000)},
		{"zero", ":0\r\n", int64(0)},
		{"negative integer", ":-2\r\n", int64(-2)},
		{"explicitly positive integer", ":+7\r\n", int64(7)},
		{"largest integer", ":9223372036854775807\r\n", int64(9223372036854775807)},
		{"smallest integer", ":-9223372036854775808\r\n", int64(-9223372036854775808)},
		{"bulk string", "$5\r\nhello\r\n", "hello"},
		{"empty bulk string", "$0\r\n\r\n", ""},
		{"bulk string holding CRLF", "$4\r\na\r\nb\r\n", "a\r\nb"},
		{"null bulk string reads as empty", "$-1\r\n", ""},
		{"empty array", "*0\r\n", []interface{}{}},
		{"array of bulk strings", "*2\r\n$5\r\nhello\r\n$5\r\nworld\r\n", []interface{}{"hello", "world"}},
		{"array of integers", "*3\r\n:1\r\n:2\r\n:3\r\n", []interface{}{int64(1), int64(2), int64(3)}},
		{"mixed array", "*3\r\n:1\r\n$5\r\nhello\r\n+OK\r\n", []interface{}{int64(1), "hello", "OK"}},
		{"nested arrays", "*2\r\n*2\r\n:1\r\n:2\r\n*2\r\n+Hello\r\n-World\r\n",
			[]interface{}{[]interface{}{int64(1), int64(2)}, []interface{}{"Hello", "World"}}},
		{"null array reads as nil", "*-1\r\n", nil},
		{"null inside an array", "*2\r\n$-1\r\n*-1\r\n", []interface{}{"", nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n, err := core.DecodeOne([]byte(c.wire))
			assert.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, len(c.wire), n, "the whole value and nothing more is consumed")
		})
	}
}

func TestDecodeStopsAtTheEndOfTheFirstValue(t *testing.T) {
	wire := "+first\r\n:2\r\n"
	got, n, err := core.DecodeOne([]byte(wire))
	assert.NoError(t, err)
	assert.Equal(t, "first", got)
	assert.Equal(t, len("+first\r\n"), n)

	got, err = core.Decode([]byte(wire))
	assert.NoError(t, err)
	assert.Equal(t, "first", got, "Decode ignores what follows")
}

func TestDecodeIncompleteValues(t *testing.T) {
	for _, wire := range []string{
		"", "+", "+OK", "+OK\r", ":", ":12", ":12\r", "$", "$5", "$5\r\n", "$5\r\nhel", "$5\r\nhello", "$5\r\nhello\r",
		"*", "*2", "*2\r\n", "*2\r\n:1\r\n", "*2\r\n:1\r\n$3\r\nab",
	} {
		_, n, err := core.DecodeOne([]byte(wire))
		assert.True(t, errors.Is(err, core.ErrIncompleteFrame), "%q should be incomplete, got %v", wire, err)
		assert.Zero(t, n)
	}
}

func TestDecodeRefusesWhatCanNeverBeValid(t *testing.T) {
	for _, wire := range []string{
		"%1\r\n",                    // no such type byte
		"PING\r\n",                  // inline commands are not RESP
		":\r\n",                     // an integer needs digits
		":-\r\n",                    // a sign alone is not a number
		":12a\r\n",                  // nor is a number with letters in it
		":99999999999999999999\r\n", // more than 64 bits
		":-9223372036854775809\r\n", // one below the smallest
		"$abc\r\n",                  // a length must be a number
		"$536870913\r\n",            // a bulk string above the 512MB bound
		"*1048577\r\n",              // an array above the 1M element bound
		"*1\r\n%\r\n",               // a bad element spoils the array
	} {
		_, n, err := core.DecodeOne([]byte(wire))
		assert.True(t, errors.Is(err, core.ErrProtocol), "%q should be a protocol error, got %v", wire, err)
		assert.Zero(t, n)
	}
}

// TestDecodeBoundsNesting: each nested array is a recursive call, and a client
// could otherwise open a few hundred thousand of them with one header each and
// run the decoder out of stack.
func TestDecodeBoundsNesting(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("*1\r\n", depth) + ":1\r\n"
	}
	v, _, err := core.DecodeOne([]byte(nested(32)))
	assert.NoError(t, err, "32 levels are allowed")
	for i := 0; i < 32; i++ {
		v = v.([]interface{})[0]
	}
	assert.Equal(t, int64(1), v)

	_, _, err = core.DecodeOne([]byte(nested(33)))
	assert.True(t, errors.Is(err, core.ErrProtocol), "33 are not: got %v", err)
	_, _, err = core.DecodeOne([]byte(nested(200000)))
	assert.True(t, errors.Is(err, core.ErrProtocol), "and a stack's worth is refused at the bound, not after it")

	// A partial deep frame is still incomplete rather than a protocol error,
	// as long as it is within the bound.
	_, _, err = core.DecodeOne([]byte(strings.Repeat("*1\r\n", 10)))
	assert.True(t, errors.Is(err, core.ErrIncompleteFrame))
}

func TestEncodeProducesTheSpecifiedWireForm(t *testing.T) {
	cases := []struct {
		name   string
		value  interface{}
		simple bool
		want   string
	}{
		{"simple string", "OK", true, "+OK\r\n"},
		{"empty simple string", "", true, "+\r\n"},
		{"bulk string", "hello", false, "$5\r\nhello\r\n"},
		{"empty bulk string", "", false, "$0\r\n\r\n"},
		{"bulk string with CRLF inside", "a\r\nb", false, "$4\r\na\r\nb\r\n"},
		{"binary bulk string", string([]byte{0, 1, 255}), false, "$3\r\n\x00\x01\xff\r\n"},
		{"utf8 counts bytes", "h\u00e9", false, "$3\r\nh\u00e9\r\n"},
		{"int", 42, false, ":42\r\n"},
		{"negative int", -7, false, ":-7\r\n"},
		{"int64 max", int64(9223372036854775807), false, ":9223372036854775807\r\n"},
		{"int64 min", int64(-9223372036854775808), false, ":-9223372036854775808\r\n"},
		{"int32", int32(-5), false, ":-5\r\n"},
		{"int16", int16(300), false, ":300\r\n"},
		{"int8", int8(-128), false, ":-128\r\n"},
		{"uint32", uint32(4294967295), false, ":4294967295\r\n"},
		{"uint64", uint64(18446744073709551615), false, ":18446744073709551615\r\n"},
		{"error", errors.New("ERR something went wrong"), false, "-ERR something went wrong\r\n"},
		{"error quoting a client's line breaks", errors.New("ERR bad key 'a\r\nb\n'"), false, "-ERR bad key 'a  b '\r\n"},
		{"nil is the null bulk string", nil, false, "$-1\r\n"},
		{"empty string slice", []string{}, false, "*0\r\n"},
		{"string slice", []string{"a", "bb", "ccc"}, false, "*3\r\n$1\r\na\r\n$2\r\nbb\r\n$3\r\nccc\r\n"},
		{"string slice with empties", []string{"", "x"}, false, "*2\r\n$0\r\n\r\n$1\r\nx\r\n"},
		{"nested string slices", [][]string{{"a"}, {"b", "c"}}, false, "*2\r\n*1\r\n$1\r\na\r\n*2\r\n$1\r\nb\r\n$1\r\nc\r\n"},
		{"empty nested", [][]string{}, false, "*0\r\n"},
		{"interface slice", []interface{}{"a", 1, errors.New("e"), nil}, false, "*4\r\n$1\r\na\r\n:1\r\n-e\r\n$-1\r\n"},
		{"empty interface slice", []interface{}{}, false, "*0\r\n"},
		{"int slice is an array of integers", []int{1, 0, -1}, false, "*3\r\n:1\r\n:0\r\n:-1\r\n"},
		{"int64 slice", []int64{7}, false, "*1\r\n:7\r\n"},
		{"empty int slice", []int{}, false, "*0\r\n"},
		{"unencodable type is an error, not a silent nil", 3.14, false, "-ERR cannot encode a float64 as a reply\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, string(core.Encode(c.value, c.simple)))
		})
	}
}

func TestEncodeThenDecodeRoundTrips(t *testing.T) {
	values := []interface{}{
		"hello", "", int64(-3), []interface{}{"a", int64(1)}, []interface{}{},
		[]interface{}{[]interface{}{"deep"}, "b"},
	}
	for _, v := range values {
		got, err := core.Decode(core.Encode(v, false))
		assert.NoError(t, err)
		assert.Equal(t, v, got, "%#v", v)
	}
	got, _ := core.Decode(core.Encode([]string{"x", "y"}, false))
	assert.Equal(t, []interface{}{"x", "y"}, got)
	got, _ = core.Decode(core.Encode([]int{1, 2}, false))
	assert.Equal(t, []interface{}{int64(1), int64(2)}, got)
}

func TestParseCmdUpperCasesTheNameAndKeepsTheArguments(t *testing.T) {
	wire := "*3\r\n$3\r\nput\r\n$5\r\nhello\r\n$5\r\nWorld\r\n"
	cmd, n, err := core.ParseCmd([]byte(wire))
	assert.NoError(t, err)
	assert.Equal(t, len(wire), n)
	assert.Equal(t, "PUT", cmd.Cmd)
	assert.Equal(t, []string{"hello", "World"}, cmd.Args, "arguments keep their case")

	cmd, _, err = core.ParseCmd([]byte("*1\r\n$4\r\nping\r\n"))
	assert.NoError(t, err)
	assert.Equal(t, "PING", cmd.Cmd)
	assert.Empty(t, cmd.Args)
}

// The benchmark reports allocated bytes per operation, which is where the
// append-based encoder wins over formatting into a string first.
func benchEncode(b *testing.B, size int) {
	b.Helper()
	v := strings.Repeat("x", size)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = core.Encode(v, false)
	}
}

func BenchmarkEncode64B(b *testing.B)   { benchEncode(b, 64) }
func BenchmarkEncode4KB(b *testing.B)   { benchEncode(b, 4096) }
func BenchmarkEncode256KB(b *testing.B) { benchEncode(b, 262144) }
func BenchmarkEncode1MB(b *testing.B)   { benchEncode(b, 1<<20) }

func BenchmarkParseCmd(b *testing.B) {
	raw := []byte(fmt.Sprintf("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$%d\r\n%s\r\n", 64, strings.Repeat("v", 64)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := core.ParseCmd(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// A command that has not fully arrived yet must be reported as incomplete so the
// caller can wait for the rest, rather than being parsed out of bounds.
func TestParseCmdIncompleteFrame(t *testing.T) {
	full := "*3\r\n$3\r\nSET\r\n$5\r\nhello\r\n$5\r\nworld\r\n"
	// Every strict prefix of a valid command is incomplete, never fatal.
	for i := 1; i < len(full); i++ {
		cmd, n, err := core.ParseCmd([]byte(full[:i]))
		assert.Nil(t, cmd)
		assert.Zero(t, n)
		assert.True(t, errors.Is(err, core.ErrIncompleteFrame),
			"prefix of length %d should be incomplete, got %v", i, err)
	}

	cmd, n, err := core.ParseCmd([]byte(full))
	assert.NoError(t, err)
	assert.EqualValues(t, len(full), n)
	assert.EqualValues(t, "SET", cmd.Cmd)
}

// Input that can never become a command must be rejected as a protocol error
// instead of panicking the server.
func TestParseCmdProtocolError(t *testing.T) {
	cases := []string{
		"PING\r\n",        // inline command, not a RESP array
		":1\r\n",          // an integer is not a command
		"+OK\r\n",         // nor is a simple string
		"*0\r\n",          // an empty array has no command name
		"*1\r\n:1\r\n",    // arguments must be bulk strings
		"%1\r\n",          // unknown type byte
		"*1000000000\r\n", // absurd element count must not be allocated
	}
	for _, c := range cases {
		cmd, n, err := core.ParseCmd([]byte(c))
		assert.Nil(t, cmd, "input %q", c)
		assert.Zero(t, n, "input %q", c)
		assert.True(t, errors.Is(err, core.ErrProtocol),
			"input %q should be a protocol error, got %v", c, err)
	}
}

// Several commands can arrive in one read. Each call consumes exactly one, so a
// caller can walk the buffer without losing the commands that follow.
func TestParseCmdPipelined(t *testing.T) {
	one := "*1\r\n$4\r\nPING\r\n"
	two := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	buf := []byte(one + two + one)

	var got []string
	for len(buf) > 0 {
		cmd, n, err := core.ParseCmd(buf)
		assert.NoError(t, err)
		got = append(got, cmd.Cmd)
		buf = buf[n:]
	}
	assert.EqualValues(t, []string{"PING", "SET", "PING"}, got)
}

// Commands larger than one read buffer used to slice past the end of the data.
func TestParseCmdLargeBulkString(t *testing.T) {
	for _, size := range []int{484, 486, 1024, 65536} {
		value := strings.Repeat("A", size)
		raw := fmt.Sprintf("*3\r\n$3\r\nSET\r\n$3\r\nbig\r\n$%d\r\n%s\r\n", size, value)
		cmd, n, err := core.ParseCmd([]byte(raw))
		assert.NoError(t, err, "size %d", size)
		assert.EqualValues(t, len(raw), n)
		assert.EqualValues(t, "SET", cmd.Cmd)
		assert.EqualValues(t, value, cmd.Args[1])
	}
}

// The server encodes negative integers itself (see constant.TtlKeyNotExist), so
// decoding has to round-trip them.
func TestDecodeNegativeInt(t *testing.T) {
	cases := map[string]int64{
		":-1\r\n": -1,
		":-2\r\n": -2,
	}
	for k, v := range cases {
		value, err := core.Decode([]byte(k))
		assert.NoError(t, err)
		assert.EqualValues(t, v, value)
	}
}

// $-1\r\n is the RESP null bulk string and must decode without consuming a payload.
func TestDecodeNullBulkString(t *testing.T) {
	value, err := core.Decode([]byte("$-1\r\n"))
	assert.NoError(t, err)
	assert.EqualValues(t, "", value)
}

// TestFrameShortfall covers the sizing hint the reader uses to ask the kernel
// for exactly the rest of a half-arrived command instead of a fixed chunk.
func TestFrameShortfall(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int
	}{
		{
			name: "complete command needs nothing",
			data: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhello\r\n",
			want: 0,
		},
		{
			name: "payload half arrived, shortfall is the rest plus CRLF",
			data: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$10\r\nabc",
			want: 9, // 7 payload bytes outstanding, then CRLF
		},
		{
			name: "payload not started at all",
			data: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$262144\r\n",
			want: 262146,
		},
		{
			name: "length header still arriving is not knowable",
			data: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$2621",
			want: 0,
		},
		{
			name: "earlier argument incomplete",
			data: "*3\r\n$3\r\nSE",
			want: 3, // one payload byte outstanding, then CRLF
		},
		{
			name: "not an array",
			data: "+OK\r\n",
			want: 0,
		},
		{
			name: "empty",
			data: "",
			want: 0,
		},
		{
			name: "array header alone is not knowable",
			data: "*3\r\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, core.FrameShortfall([]byte(c.data)))
		})
	}
}

// TestFrameShortfallMatchesWhatDecodingNeeds ties the hint to the parser: after
// supplying exactly the reported shortfall, the frame must decode.
func TestFrameShortfallMatchesWhatDecodingNeeds(t *testing.T) {
	full := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$2048\r\n" + strings.Repeat("v", 2048) + "\r\n")
	for _, cut := range []int{25, 30, 100, 1024, len(full) - 1} {
		partial := full[:cut]
		short := core.FrameShortfall(partial)
		if short == 0 {
			continue // not knowable at this cut, which is allowed
		}
		assert.Equal(t, len(full), cut+short,
			"shortfall at cut %d must land exactly on the end of the frame", cut)

		_, _, err := core.ParseCmd(full[:cut+short])
		assert.NoError(t, err, "supplying the shortfall must complete the frame")
	}
}
