package core

import (
	"errors"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
)

const CRLF string = "\r\n"

// A single TCP read is not guaranteed to contain exactly one RESP value.
// The kernel may hand us half of a command, or several pipelined commands at
// once, so decoding has to distinguish two very different kinds of failure.

// ErrIncompleteFrame means data holds the start of a well-formed RESP value but
// not all of it yet. The caller should keep the bytes it has, read more from the
// socket, and try again. This is a normal part of reading from a stream and must
// never close the connection.
var ErrIncompleteFrame = errors.New("incomplete RESP frame")

// ErrProtocol means data cannot become a valid RESP value no matter how many
// more bytes arrive. The caller should reply with an error and close the
// offending connection.
var ErrProtocol = errors.New("ERR Protocol error: invalid RESP input")

// Upper bounds borrowed from Redis. Without them a client could send a tiny
// header such as "*1000000000\r\n" and make us allocate gigabytes before we ever
// look at the payload, which crashes the whole server.
const (
	maxMultiBulkLength = 1024 * 1024
	maxBulkLength      = 512 * 1024 * 1024
)

// +OK\r\n => "OK", 5
func readSimpleString(data []byte) (string, int, error) {
	pos := 1
	for ; pos < len(data) && data[pos] != '\r'; pos++ {
	}
	// pos+1 must be readable: we need the '\r' we stopped on and the '\n' after it.
	if pos+1 >= len(data) {
		return "", 0, ErrIncompleteFrame
	}
	return string(data[1:pos]), pos + 2, nil
}

// :123\r\n => 123, 6
// :-2\r\n  => -2, 6
func readInt64(data []byte) (int64, int, error) {
	pos := 1
	negative := false
	if pos < len(data) && data[pos] == '-' {
		negative = true
		pos++
	}

	var res int64 = 0
	digits := 0
	for ; pos < len(data) && data[pos] != '\r'; pos++ {
		if data[pos] < '0' || data[pos] > '9' {
			return 0, 0, ErrProtocol
		}
		res = res*10 + int64(data[pos]-'0')
		digits++
	}
	if pos+1 >= len(data) {
		return 0, 0, ErrIncompleteFrame
	}
	if digits == 0 {
		return 0, 0, ErrProtocol
	}
	if negative {
		res = -res
	}
	return res, pos + 2, nil
}

func readError(data []byte) (string, int, error) {
	return readSimpleString(data)
}

// $5\r\nhello\r\n => 5, 4
func readLen(data []byte) (int, int, error) {
	res, pos, err := readInt64(data)
	if err != nil {
		return 0, 0, err
	}
	return int(res), pos, nil
}

// $5\r\nhello\r\n => "hello", 11
func readBulkString(data []byte) (string, int, error) {
	length, pos, err := readLen(data)
	if err != nil {
		return "", 0, err
	}
	// $-1\r\n is the RESP null bulk string. It has no payload of its own, so the
	// value ends where the header ends.
	if length < 0 {
		return "", pos, nil
	}
	if length > maxBulkLength {
		return "", 0, ErrProtocol
	}
	// The payload is followed by a trailing CRLF, hence the +2.
	if pos+length+2 > len(data) {
		return "", 0, ErrIncompleteFrame
	}
	return string(data[pos : pos+length]), pos + length + 2, nil
}

// *2\r\n$5\r\nhello\r\n$5\r\nworld\r\n => {"hello", "world"}
func readArray(data []byte) (interface{}, int, error) {
	length, pos, err := readLen(data)
	if err != nil {
		return nil, 0, err
	}
	if length < 0 {
		return nil, pos, nil
	}
	if length > maxMultiBulkLength {
		return nil, 0, ErrProtocol
	}

	var res []interface{} = make([]interface{}, length)
	for i := range res {
		elem, delta, err := DecodeOne(data[pos:])
		if err != nil {
			return nil, 0, err
		}
		res[i] = elem
		pos += delta
	}
	return res, pos, nil
}

// @1|2|3| -> {1, 2, 3}
func readIntArray(data []byte) (interface{}, int, error) {
	var res []int
	cur := 0
	pos := 1
	for ; pos < len(data); pos++ {
		if data[pos] == '|' {
			res = append(res, cur)
			cur = 0
			continue
		}
		if data[pos] < '0' || data[pos] > '9' {
			return nil, 0, ErrProtocol
		}
		cur = cur*10 + int(data[pos]-'0')
	}
	return res, pos, nil
}

// DecodeOne decodes the single RESP value at the front of data and reports how
// many bytes it consumed. It returns ErrIncompleteFrame when data holds only
// part of a value, so callers reading from a socket know to wait for more.
func DecodeOne(data []byte) (interface{}, int, error) {
	if len(data) == 0 {
		return nil, 0, ErrIncompleteFrame
	}
	switch data[0] {
	case '+':
		return readSimpleString(data)
	case ':':
		return readInt64(data)
	case '-':
		return readError(data)
	case '$':
		return readBulkString(data)
	case '*':
		return readArray(data)
	case '@':
		return readIntArray(data)
	}
	return nil, 0, ErrProtocol
}

func Decode(data []byte) (interface{}, error) {
	res, _, err := DecodeOne(data)
	return res, err
}

// The append* helpers build RESP into a caller-supplied slice.
//
// The previous form was []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)), which
// copies the payload twice: once for the string Sprintf returns, and again for
// the []byte conversion. On a GET that is two allocations and two copies the
// size of the value before it reaches the reply buffer at all. Appending writes
// it once.
func appendBulkString(dst []byte, s string) []byte {
	dst = append(dst, '$')
	dst = strconv.AppendInt(dst, int64(len(s)), 10)
	dst = append(dst, '\r', '\n')
	dst = append(dst, s...)
	return append(dst, '\r', '\n')
}

func appendArrayHeader(dst []byte, n int) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, '\r', '\n')
}

func encodeString(s string) []byte {
	return appendBulkString(make([]byte, 0, len(s)+16), s)
}

func encodeStringArray(sa []string) []byte {
	size := 16
	for _, s := range sa {
		size += len(s) + 16
	}
	b := appendArrayHeader(make([]byte, 0, size), len(sa))
	for _, s := range sa {
		b = appendBulkString(b, s)
	}
	return b
}

func Encode(value interface{}, isSimpleString bool) []byte {
	switch v := value.(type) {
	case string:
		if isSimpleString {
			b := make([]byte, 0, len(v)+3)
			b = append(b, '+')
			b = append(b, v...)
			return append(b, '\r', '\n')
		}
		return encodeString(v)
	case int64:
		return appendInt(v)
	case int32:
		return appendInt(int64(v))
	case int16:
		return appendInt(int64(v))
	case int8:
		return appendInt(int64(v))
	case int:
		return appendInt(int64(v))
	case error:
		msg := v.Error()
		b := make([]byte, 0, len(msg)+3)
		b = append(b, '-')
		b = append(b, msg...)
		return append(b, '\r', '\n')
	case []string:
		return encodeStringArray(v)
	case [][]string:
		b := appendArrayHeader(nil, len(v))
		for _, sa := range v {
			b = append(b, encodeStringArray(sa)...)
		}
		return b
	case []interface{}:
		b := appendArrayHeader(nil, len(v))
		for _, x := range v {
			b = append(b, Encode(x, false)...)
		}
		return b
	case []int:
		b := make([]byte, 0, len(v)*4+1)
		b = append(b, '@')
		for _, n := range v {
			b = strconv.AppendInt(b, int64(n), 10)
			b = append(b, '|')
		}
		return b
	default:
		return constant.RespNil
	}
}

func appendInt(n int64) []byte {
	b := make([]byte, 0, 24)
	b = append(b, ':')
	b = strconv.AppendInt(b, n, 10)
	return append(b, '\r', '\n')
}

// ParseCmd decodes the first complete command in data and reports how many bytes
// it consumed, so that a caller holding several pipelined commands can advance to
// the next one. It returns ErrIncompleteFrame if data does not yet hold a whole
// command, and ErrProtocol if the input is not a well-formed command at all.
func ParseCmd(data []byte) (*Command, int, error) {
	value, n, err := DecodeOne(data)
	if err != nil {
		return nil, 0, err
	}

	// A command is always a RESP array of bulk strings. Anything else - an inline
	// "PING\r\n", a stray integer, a null array - is a protocol error rather than
	// something we can execute.
	array, ok := value.([]interface{})
	if !ok || len(array) == 0 {
		return nil, 0, ErrProtocol
	}

	tokens := make([]string, len(array))
	for i := range tokens {
		token, ok := array[i].(string)
		if !ok {
			return nil, 0, ErrProtocol
		}
		tokens[i] = token
	}

	res := &Command{Cmd: strings.ToUpper(tokens[0]), Args: tokens[1:]}
	return res, n, nil
}

// FrameShortfall reports how many more bytes must arrive before the command at
// the front of data can be decoded.
//
// A reader uses it to size its next read. Pulling a 256KB value 64KB at a time
// is four syscalls and a growing buffer; knowing the exact shortfall lets the
// caller ask for the remainder in one call, straight into the right-sized
// destination. Redis does the same thing for what it calls big arguments.
//
// It returns 0 when the shortfall is not knowable and the caller should fall
// back to a fixed-size read: the frame is already complete, it is malformed, or
// the bytes still missing are a length header whose digits have not all
// arrived, so how much follows it is not yet established.
func FrameShortfall(data []byte) int {
	if len(data) == 0 || data[0] != '*' {
		return 0
	}
	count, pos, err := readLen(data)
	if err != nil || count < 0 {
		return 0
	}
	for i := 0; i < count; i++ {
		if pos >= len(data) || data[pos] != '$' {
			return 0
		}
		length, adv, err := readLen(data[pos:])
		if err != nil {
			// The header itself is still arriving; its payload length is
			// unknown until the digits and their CRLF are all here.
			return 0
		}
		pos += adv
		if length < 0 {
			continue // $-1\r\n carries no payload
		}
		// The payload is followed by CRLF, hence the +2.
		if end := pos + length + 2; end > len(data) {
			return end - len(data)
		} else {
			pos = end
		}
	}
	return 0
}
