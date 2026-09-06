package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
)

// RESP, the Redis serialisation protocol, as a client speaks it to a server.
//
// Five kinds of value, each announced by its first byte and ended by CRLF:
//
//	+text\r\n           simple string
//	-text\r\n           error
//	:123\r\n            integer
//	$5\r\nhello\r\n     bulk string, length first; $-1\r\n is the null string
//	*2\r\n<a><b>        array, count first, then its values; *-1\r\n is the
//	                    null array
//
// A command is an array of bulk strings. Everything a client sends is read by
// ParseCmd, and everything sent back is built by Encode or the append helpers.

const CRLF = "\r\n"

var crlf = []byte(CRLF)

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
	// maxArrayDepth bounds how deeply arrays may nest. Each level is a
	// recursive call, so without a bound a client could send "*1\r\n" a few
	// hundred thousand times and run the decoder out of stack. A command is
	// one array of bulk strings, so no legitimate input comes near this.
	maxArrayDepth = 32
)

// frameReader walks one RESP value at the front of a byte slice, remembering
// how far it has got so the caller knows where the next value starts.
type frameReader struct {
	data []byte
	pos  int
	// depth is how many arrays are open around the value being read.
	depth int
}

// line returns the current line without its CRLF and steps past it. No CRLF in
// what remains means the line has not finished arriving.
func (r *frameReader) line() ([]byte, error) {
	rest := r.data[r.pos:]
	end := bytes.Index(rest, crlf)
	if end < 0 {
		return nil, ErrIncompleteFrame
	}
	r.pos += end + 2
	return rest[:end], nil
}

// integer reads a line holding a decimal integer.
func (r *frameReader) integer() (int64, error) {
	line, err := r.line()
	if err != nil {
		return 0, err
	}
	n, ok := parseDecimal(line)
	if !ok {
		return 0, ErrProtocol
	}
	return n, nil
}

// parseDecimal reads an optionally signed decimal integer without allocating.
// It refuses anything that is not digits after the sign, and anything that
// does not fit in 64 bits, because a length that overflowed would come out
// small and be trusted.
func parseDecimal(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	negative := false
	switch b[0] {
	case '-':
		negative = true
		b = b[1:]
	case '+':
		b = b[1:]
	}
	if len(b) == 0 {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	if negative {
		if n > 1<<63 {
			return 0, false
		}
		return -int64(n), true
	}
	if n > math.MaxInt64 {
		return 0, false
	}
	return int64(n), true
}

// value decodes the value at the reader's position.
//
// Simple strings and errors both come back as the string they carry; a caller
// that has to tell them apart looks at the raw bytes. The null bulk string
// comes back as "" and the null array as nil, for the same reason: the
// distinction is in the wire form, and the decoded form is what tests and the
// log replay find convenient.
func (r *frameReader) value() (interface{}, error) {
	if r.pos >= len(r.data) {
		return nil, ErrIncompleteFrame
	}
	kind := r.data[r.pos]
	r.pos++
	switch kind {
	case '+', '-':
		line, err := r.line()
		if err != nil {
			return nil, err
		}
		return string(line), nil
	case ':':
		return r.integer()
	case '$':
		return r.bulk()
	case '*':
		return r.array()
	}
	return nil, ErrProtocol
}

// bulk reads a length and then that many bytes.
func (r *frameReader) bulk() (interface{}, error) {
	return r.bulkString()
}

// bulkString keeps command tokens typed, avoiding boxing each string into an
// interface only to extract it again. Every string owns its bytes; retaining a
// small key never pins the rest of a client's large input buffer.
func (r *frameReader) bulkString() (string, error) {
	n, err := r.integer()
	if err != nil {
		return "", err
	}
	if n < -1 {
		return "", ErrProtocol
	}
	if n < 0 {
		return "", nil
	}
	if n > maxBulkLength {
		return "", ErrProtocol
	}
	// The payload is followed by a CRLF of its own. It is stepped over rather
	// than checked, as Redis steps over it: the length is what delimits the
	// payload, and a client that gets the length right and the terminator
	// wrong is not worth dropping.
	end := r.pos + int(n) + 2
	if end > len(r.data) {
		return "", ErrIncompleteFrame
	}
	if r.data[end-2] != '\r' || r.data[end-1] != '\n' {
		return "", ErrProtocol
	}
	s := string(r.data[r.pos : r.pos+int(n)])
	r.pos = end
	return s, nil
}

// array reads a count and then that many values.
func (r *frameReader) array() (interface{}, error) {
	n, err := r.integer()
	if err != nil {
		return nil, err
	}
	if n < -1 {
		return nil, ErrProtocol
	}
	if n < 0 {
		return nil, nil
	}
	if n > maxMultiBulkLength {
		return nil, ErrProtocol
	}
	if r.depth >= maxArrayDepth {
		return nil, ErrProtocol
	}
	r.depth++
	defer func() { r.depth-- }()

	out := make([]interface{}, 0, min(int(n), 16))
	for i := int64(0); i < n; i++ {
		value, err := r.value()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// DecodeOne decodes the single RESP value at the front of data and reports how
// many bytes it consumed. It returns ErrIncompleteFrame when data holds only
// part of a value, so callers reading from a socket know to wait for more.
func DecodeOne(data []byte) (interface{}, int, error) {
	r := frameReader{data: data}
	v, err := r.value()
	if err != nil {
		return nil, 0, err
	}
	return v, r.pos, nil
}

// Decode decodes the value at the front of data, ignoring anything after it.
func Decode(data []byte) (interface{}, error) {
	v, _, err := DecodeOne(data)
	return v, err
}

// readLength reads the count or length that follows a type byte, and reports
// how many bytes the header took including the type byte and its CRLF.
func readLength(data []byte) (int, int, error) {
	r := frameReader{data: data, pos: 1}
	n, err := r.integer()
	if err != nil {
		return 0, 0, err
	}
	return int(n), r.pos, nil
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

func appendSimpleString(dst []byte, s string) []byte {
	dst = append(dst, '+')
	dst = append(dst, s...)
	return append(dst, '\r', '\n')
}

// appendError writes an error reply. An error line ends at the first CRLF, and
// some errors quote the argument that caused them, which a client chose - so a
// CR or LF inside the message would end the reply early and leave the rest to
// be read as the next one. Both are written as spaces, as Redis writes them.
func appendError(dst []byte, msg string) []byte {
	dst = append(dst, '-')
	if strings.IndexAny(msg, "\r\n") < 0 {
		dst = append(dst, msg...)
	} else {
		for i := 0; i < len(msg); i++ {
			if c := msg[i]; c == '\r' || c == '\n' {
				dst = append(dst, ' ')
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '\r', '\n')
}

func appendInteger(dst []byte, n int64) []byte {
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, n, 10)
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

// Encode builds the reply for a Go value.
//
// A string is a bulk string unless isSimpleString asks for the +text form,
// which is for status replies like OK and PONG. Integers of every width are
// RESP integers, an error is a RESP error, nil is the null bulk string, and
// slices become arrays of whatever they hold. A value of a type not listed is
// a bug in the command that produced it, and is answered as an error naming
// the type rather than as a silent nil.
func Encode(value interface{}, isSimpleString bool) []byte {
	switch v := value.(type) {
	case nil:
		return constant.RespNil
	case string:
		if isSimpleString {
			return appendSimpleString(make([]byte, 0, len(v)+3), v)
		}
		return encodeString(v)
	case error:
		msg := v.Error()
		return appendError(make([]byte, 0, len(msg)+3), msg)
	case int:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case int8:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case int16:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case int32:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case int64:
		return appendInteger(make([]byte, 0, 24), v)
	case uint8:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case uint16:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case uint32:
		return appendInteger(make([]byte, 0, 24), int64(v))
	case uint64:
		b := append(make([]byte, 0, 24), ':')
		b = strconv.AppendUint(b, v, 10)
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
		b := appendArrayHeader(make([]byte, 0, 4+8*len(v)), len(v))
		for _, n := range v {
			b = appendInteger(b, int64(n))
		}
		return b
	case []int64:
		b := appendArrayHeader(make([]byte, 0, 4+8*len(v)), len(v))
		for _, n := range v {
			b = appendInteger(b, n)
		}
		return b
	}
	return appendError(nil, fmt.Sprintf("ERR cannot encode a %T as a reply", value))
}

// ParseCmd decodes the first complete command in data and reports how many bytes
// it consumed, so that a caller holding several pipelined commands can advance to
// the next one. It returns ErrIncompleteFrame if data does not yet hold a whole
// command, and ErrProtocol if the input is not a well-formed command at all.
func ParseCmd(data []byte) (*Command, int, error) {
	if len(data) == 0 || data[0] != '*' {
		return parseCmdGeneric(data)
	}
	r := frameReader{data: data, pos: 1}
	n, err := r.integer()
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 || n > maxMultiBulkLength {
		return nil, 0, ErrProtocol
	}
	// Capacity follows received tokens, not an untrusted declared array size.
	tokens := make([]string, 0, min(int(n), 16))
	for i := int64(0); i < n; i++ {
		if r.pos == len(data) {
			return nil, 0, ErrIncompleteFrame
		}
		if data[r.pos] != '$' {
			// Preserve the existing decoder's handling of unusual RESP types,
			// including its incomplete-versus-invalid classification.
			return parseCmdGeneric(data)
		}
		r.pos++
		token, err := r.bulkString()
		if err != nil {
			return nil, 0, err
		}
		tokens = append(tokens, token)
	}
	return &Command{Cmd: strings.ToUpper(tokens[0]), Args: tokens[1:]}, r.pos, nil
}

// parseCmdGeneric is also used as the reference decoder by differential fuzz
// tests. DecodeOne remains the general RESP value decoder for replies/dumps.
func parseCmdGeneric(data []byte) (*Command, int, error) {
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
	count, pos, err := readLength(data)
	if err != nil || count < 0 {
		return 0
	}
	for i := 0; i < count; i++ {
		if pos >= len(data) || data[pos] != '$' {
			return 0
		}
		length, adv, err := readLength(data[pos:])
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
