// Package resp is the Redis serialisation protocol, as a client speaks it to
// a server and a server answers: the parser that turns bytes into a Command,
// and the encoders that frame a reply in RESP2 or RESP3. It is a leaf: it
// imports nothing of the engine, so the server, the engine and, in time, the
// public package can all speak the protocol without each other (phase 4 of
// docs/embedding-plan.md).
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
// ParseCmd or ParseCmdReserved, and everything sent back is built by Encode
// or the Append helpers.
//
// Those five are RESP2. A connection that negotiates RESP3 still sends
// commands the same way, but is answered with RESP3's further types; the
// encoders have them, and a Framing chooses between the two. The Append
// helpers write only what both protocols share, which is also why the log and
// the replication feed, which are commands rather than replies, are built from
// them alone.
package resp

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

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

// ErrRequestAllocation refuses a request the caller's budget cannot hold.
var ErrRequestAllocation = errors.New("ERR request allocation budget exhausted")

// Upper bounds borrowed from Redis. Without them a client could send a tiny
// header such as "*1000000000\r\n" and make us allocate gigabytes before we ever
// look at the payload, which crashes the whole server.
const (
	MaxMultiBulkLength = 1024 * 1024
	MaxBulkLength      = 512 * 1024 * 1024
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
	n, ok := ParseDecimal(line)
	if !ok {
		return 0, ErrProtocol
	}
	return n, nil
}

// ParseDecimal reads an optionally signed decimal integer without allocating.
// It refuses anything that is not digits after the sign, and anything that
// does not fit in 64 bits, because a length that overflowed would come out
// small and be trusted.
func ParseDecimal(b []byte) (int64, bool) {
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
	if n > MaxBulkLength {
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
	if n > MaxMultiBulkLength {
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

// ParseCmd decodes the first complete command in data and reports how many bytes
// it consumed, so that a caller holding several pipelined commands can advance to
// the next one. It returns ErrIncompleteFrame if data does not yet hold a whole
// command, and ErrProtocol if the input is not a well-formed command at all.
func ParseCmd(data []byte) (*Command, int, error) {
	if len(data) == 0 || data[0] != '*' {
		return ParseCmdReference(data)
	}
	r := frameReader{data: data, pos: 1}
	n, err := r.integer()
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 || n > MaxMultiBulkLength {
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
			return ParseCmdReference(data)
		}
		r.pos++
		token, err := r.bulkString()
		if err != nil {
			return nil, 0, err
		}
		tokens = append(tokens, token)
	}
	return ParsedCommand(tokens[0], tokens[1:]), r.pos, nil
}

// ParseCmdReference is the reference decoder, which the differential fuzz
// tests hold the faster parsers to: it decodes a whole value with DecodeOne
// and then asks whether it was a command. DecodeOne remains the general RESP
// value decoder for replies and dumps.
func ParseCmdReference(data []byte) (*Command, int, error) {
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

	return ParsedCommand(tokens[0], tokens[1:]), n, nil
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

// RequestAllocationSize includes conservative size-class/rounding headroom.
// It is an admission charge for owned allocations, not measured heap or RSS.
func RequestAllocationSize(n int) (int, bool) {
	const maxInt = int(^uint(0) >> 1)
	if n < 0 || n > maxInt-n/4-64 {
		return 0, false
	}
	if n == 0 {
		return 0, true
	}
	return n + n/4 + 64, true
}

type commandSpan struct{ start, end int }

// ParseCmdReserved validates one complete frame before reserving and copying
// its strings. Incomplete large frames therefore do not repeatedly copy keys or
// earlier arguments. The common sixteen-field layout stays on the stack; larger
// complete frames are walked again after admission rather than growing metadata
// from an untrusted count. The callback must not mutate data or retain it.
func ParseCmdReserved(data []byte, reserve func(int) bool) (*Command, int, error) {
	if len(data) == 0 || data[0] != '*' {
		return nil, 0, commandShapeError(data)
	}
	r := frameReader{data: data, pos: 1}
	n, err := r.integer()
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 || n > MaxMultiBulkLength {
		return nil, 0, ErrProtocol
	}
	first := r.pos
	var spans [16]commandSpan
	charge := 80 // Command, its 64 bytes rounded to their size class, and the caller's pointer to it.
	add := func(size int) bool {
		padded, ok := RequestAllocationSize(size)
		if !ok || padded > int(^uint(0)>>1)-charge {
			return false
		}
		charge += padded
		return true
	}
	if !add(int(n-1) * 16) {
		return nil, 0, ErrRequestAllocation
	}
	for i := 0; i < int(n); i++ {
		span, err := r.commandStringSpan()
		if err != nil {
			// The reference decoder validates an entire unusual RESP value
			// before rejecting its command shape. Preserve that distinction
			// between incomplete and invalid without constructing any values.
			return nil, 0, commandShapeError(data)
		}
		if i < len(spans) {
			spans[i] = span
		}
		size := span.end - span.start
		if !add(size) {
			return nil, 0, ErrRequestAllocation
		}
		// Unicode uppercasing can grow and replace its backing allocation.
		// Reserve that temporary copy as well as the owned command string.
		if i == 0 && (size > int(^uint(0)>>1)/3 || !add(3*size)) {
			return nil, 0, ErrRequestAllocation
		}
	}
	consumed := r.pos
	if reserve != nil && !reserve(charge) {
		return nil, 0, ErrRequestAllocation
	}
	command := allocateDecodedCommand(int(n) - 1)
	args := command.Args
	var name, sent string
	r.pos = first
	for i := 0; i < int(n); i++ {
		var span commandSpan
		if n <= int64(len(spans)) {
			span = spans[i]
		} else {
			span, err = r.commandStringSpan()
			if err != nil {
				return nil, 0, err
			}
		}
		if i == 0 {
			name, sent = ownedUpperCommand(data[span.start:span.end])
		} else {
			args[i-1] = string(data[span.start:span.end])
		}
	}
	command.Cmd = name
	if name != sent {
		// The spelling the client sent, which an unknown command's error
		// echoes; both strings are within the admitted charge.
		command.Name = sent
	}
	return command, consumed, nil
}

// strings.ToUpper can grow its builder repeatedly for expanding Unicode or
// invalid UTF-8. Size that rare path first so the admitted original string and
// at most three-byte-per-input-byte conversion each allocate only once. The
// original is returned as well, as the client spelled the name.
func ownedUpperCommand(data []byte) (upper, sent string) {
	name := string(data)
	hasLower := false
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			size := 0
			for _, r := range name {
				size += utf8.RuneLen(unicode.ToUpper(r))
			}
			var b strings.Builder
			b.Grow(size)
			for _, r := range name {
				b.WriteRune(unicode.ToUpper(r))
			}
			return b.String(), name
		}
		hasLower = hasLower || ('a' <= name[i] && name[i] <= 'z')
	}
	if hasLower {
		return strings.ToUpper(name), name
	}
	return name, name
}

func (r *frameReader) commandStringSpan() (commandSpan, error) {
	if r.pos >= len(r.data) {
		return commandSpan{}, ErrIncompleteFrame
	}
	kind := r.data[r.pos]
	r.pos++
	switch kind {
	case '+', '-':
		start := r.pos
		line, err := r.line()
		return commandSpan{start, start + len(line)}, err
	case '$':
		n, err := r.integer()
		if err != nil {
			return commandSpan{}, err
		}
		if n < -1 || n > MaxBulkLength {
			return commandSpan{}, ErrProtocol
		}
		if n == -1 {
			return commandSpan{r.pos, r.pos}, nil
		}
		if n > int64(len(r.data)-r.pos)-2 {
			return commandSpan{}, ErrIncompleteFrame
		}
		span := commandSpan{r.pos, r.pos + int(n)}
		if r.data[span.end] != '\r' || r.data[span.end+1] != '\n' {
			return commandSpan{}, ErrProtocol
		}
		r.pos = span.end + 2
		return span, nil
	default:
		return commandSpan{}, ErrProtocol
	}
}

func commandShapeError(data []byte) error {
	r := frameReader{data: data}
	if err := r.skipValue(); err != nil {
		return err
	}
	return ErrProtocol
}

// skipValue mirrors value's framing and depth rules without allocating decoded
// strings, interfaces or arrays. It is used only for invalid command shapes.
func (r *frameReader) skipValue() error {
	if r.pos >= len(r.data) {
		return ErrIncompleteFrame
	}
	switch r.data[r.pos] {
	case '+', '-', '$':
		_, err := r.commandStringSpan()
		return err
	case ':':
		r.pos++
		_, err := r.integer()
		return err
	case '*':
		r.pos++
		n, err := r.integer()
		if err != nil {
			return err
		}
		if n < -1 || n > MaxMultiBulkLength {
			return ErrProtocol
		}
		if n == -1 {
			return nil
		}
		if r.depth >= maxArrayDepth {
			return ErrProtocol
		}
		r.depth++
		defer func() { r.depth-- }()
		for i := int64(0); i < n; i++ {
			if err := r.skipValue(); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrProtocol
	}
}
