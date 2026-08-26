package core_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/core"
)

func TestSimpleStringDecode(t *testing.T) {
	cases := map[string]string{
		"+OK\r\n": "OK",
	}
	for k, v := range cases {
		value, _ := core.Decode([]byte(k))
		if v != value {
			t.Fail()
		}
	}
}

func TestError(t *testing.T) {
	cases := map[string]string{
		"-Error message\r\n": "Error message",
	}
	for k, v := range cases {
		value, _ := core.Decode([]byte(k))
		if v != value {
			t.Fail()
		}
	}
}

func TestInt64(t *testing.T) {
	cases := map[string]int64{
		":0\r\n":    0,
		":1000\r\n": 1000,
	}
	for k, v := range cases {
		value, _ := core.Decode([]byte(k))
		if v != value {
			t.Fail()
		}
	}
}

func TestBulkStringDecode(t *testing.T) {
	cases := map[string]string{
		"$5\r\nhello\r\n": "hello",
		"$0\r\n\r\n":      "",
	}
	for k, v := range cases {
		value, _ := core.Decode([]byte(k))
		if v != value {
			t.Fail()
		}
	}
}

func TestArrayDecode(t *testing.T) {
	cases := map[string][]interface{}{
		"*0\r\n":                                                   {},
		"*2\r\n$5\r\nhello\r\n$5\r\nworld\r\n":                     {"hello", "world"},
		"*3\r\n:1\r\n:2\r\n:3\r\n":                                 {int64(1), int64(2), int64(3)},
		"*5\r\n:1\r\n:2\r\n:3\r\n:4\r\n$5\r\nhello\r\n":            {int64(1), int64(2), int64(3), int64(4), "hello"},
		"*2\r\n*3\r\n:1\r\n:2\r\n:3\r\n*2\r\n+Hello\r\n-World\r\n": {[]int64{int64(1), int64(2), int64(3)}, []interface{}{"Hello", "World"}},
	}
	for k, v := range cases {
		value, _ := core.Decode([]byte(k))
		array := value.([]interface{})
		if len(array) != len(v) {
			t.Fail()
		}
		for i := range array {
			if fmt.Sprintf("%v", v[i]) != fmt.Sprintf("%v", array[i]) {
				t.Fail()
			}
		}
	}
}

func TestEncodeString2DArray(t *testing.T) {
	var decode = [][]string{{"hello", "world"}, {"1", "2", "3"}, {"xyz"}}
	encode := core.Encode(decode, false)
	assert.EqualValues(t, "*3\r\n*2\r\n$5\r\nhello\r\n$5\r\nworld\r\n*3\r\n$1\r\n1\r\n$1\r\n2\r\n$1\r\n3\r\n*1\r\n$3\r\nxyz\r\n", string(encode))
	decodeAgain, _ := core.Decode(encode)
	for i := 0; i < 3; i++ {
		for j := 0; j < len(decode[i]); j++ {
			assert.EqualValues(t, decode[i][j], decodeAgain.([]interface{})[i].([]interface{})[j])
		}
	}
}

func TestEncodeInterfaceArray(t *testing.T) {
	cases := map[string][]interface{}{
		"*0\r\n":                                        {},
		"*2\r\n$5\r\nhello\r\n$5\r\nworld\r\n":          {"hello", "world"},
		"*3\r\n:1\r\n:2\r\n:3\r\n":                      {int64(1), int64(2), int64(3)},
		"*5\r\n:1\r\n:2\r\n:3\r\n:4\r\n$5\r\nhello\r\n": {int64(1), int64(2), int64(3), int64(4), "hello"},
		"*2\r\n*3\r\n:1\r\n:2\r\n:3\r\n*2\r\n$5\r\nHello\r\n$5\r\nWorld\r\n": {[]interface{}{int64(1), int64(2), int64(3)}, []interface{}{"Hello", "World"}},
	}
	for k, v := range cases {
		encode := core.Encode(v, false)
		assert.EqualValues(t, k, string(encode))
	}
}

func TestParseCmd(t *testing.T) {
	cases := map[string]core.MemKVCmd{
		"*3\r\n$3\r\nput\r\n$5\r\nhello\r\n$5\r\nworld\r\n": core.MemKVCmd{
			Cmd:  "PUT",
			Args: []string{"hello", "world"},
		}}
	for k, v := range cases {
		cmd, _, _ := core.ParseCmd([]byte(k))
		if cmd.Cmd != v.Cmd {
			t.Fail()
		}
		if len(cmd.Args) != len(v.Args) {
			t.Fail()
		}
		for i := 0; i < len(cmd.Args); i++ {
			if cmd.Args[i] != v.Args[i] {
				t.Fail()
			}
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
