package core

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"memkv/internal/constant"
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
func encodeString(s string) []byte {
	return []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))
}

func encodeStringArray(sa []string) []byte {
	var b []byte
	buf := bytes.NewBuffer(b)
	for _, s := range sa {
		buf.Write(encodeString(s))
	}
	return []byte(fmt.Sprintf("*%d\r\n%s", len(sa), buf.Bytes()))
}

func Encode(value interface{}, isSimpleString bool) []byte {
	switch v := value.(type) {
	case string:
		if isSimpleString {
			return []byte(fmt.Sprintf("+%s%s", v, CRLF))
		}
		return []byte(fmt.Sprintf("$%d%s%s%s", len(v), CRLF, v, CRLF))
	case int64, int32, int16, int8, int:
		return []byte(fmt.Sprintf(":%d\r\n", v))
	case error:
		return []byte(fmt.Sprintf("-%s\r\n", v))
	case []string:
		return encodeStringArray(value.([]string))
	case [][]string:
		var b []byte
		buf := bytes.NewBuffer(b)
		for _, sa := range value.([][]string) {
			buf.Write(encodeStringArray(sa))
		}
		return []byte(fmt.Sprintf("*%d\r\n%s", len(value.([][]string)), buf.Bytes()))
	case []interface{}:
		var b []byte
		buf := bytes.NewBuffer(b)
		for _, x := range value.([]interface{}) {
			buf.Write(Encode(x, false))
		}
		return []byte(fmt.Sprintf("*%d\r\n%s", len(value.([]interface{})), buf.Bytes()))
	case []int:
		var b []byte
		buf := bytes.NewBuffer(b)
		for _, n := range value.([]int) {
			buf.Write([]byte(fmt.Sprintf("%d|", n)))
		}
		return []byte(fmt.Sprintf("@%s", buf.Bytes()))
	default:
		return constant.RespNil
	}
}

// ParseCmd decodes the first complete command in data and reports how many bytes
// it consumed, so that a caller holding several pipelined commands can advance to
// the next one. It returns ErrIncompleteFrame if data does not yet hold a whole
// command, and ErrProtocol if the input is not a well-formed command at all.
func ParseCmd(data []byte) (*MemKVCmd, int, error) {
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

	res := &MemKVCmd{Cmd: strings.ToUpper(tokens[0]), Args: tokens[1:]}
	return res, n, nil
}
