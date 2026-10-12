package resp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
)

// The Append helpers build RESP into a caller-supplied slice.
//
// The previous form was []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)), which
// copies the payload twice: once for the string Sprintf returns, and again for
// the []byte conversion. On a GET that is two allocations and two copies the
// size of the value before it reaches the reply buffer at all. Appending writes
// it once.
func AppendBulkString(dst []byte, s string) []byte {
	dst = append(dst, '$')
	dst = strconv.AppendInt(dst, int64(len(s)), 10)
	dst = append(dst, '\r', '\n')
	dst = append(dst, s...)
	return append(dst, '\r', '\n')
}

func AppendArrayHeader(dst []byte, n int) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, '\r', '\n')
}

func AppendSimpleString(dst []byte, s string) []byte {
	dst = append(dst, '+')
	dst = append(dst, s...)
	return append(dst, '\r', '\n')
}

// AppendError writes an error reply. An error line ends at the first CRLF, and
// some errors quote the argument that caused them, which a client chose - so a
// CR or LF inside the message would end the reply early and leave the rest to
// be read as the next one. Both are written as spaces, as Redis writes them.
func AppendError(dst []byte, msg string) []byte {
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

func AppendInteger(dst []byte, n int64) []byte {
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, n, 10)
	return append(dst, '\r', '\n')
}

func EncodeString(s string) []byte {
	return AppendBulkString(make([]byte, 0, len(s)+16), s)
}

func EncodeStringArray(sa []string) []byte {
	size := 16
	for _, s := range sa {
		size += len(s) + 16
	}
	b := AppendArrayHeader(make([]byte, 0, size), len(sa))
	for _, s := range sa {
		b = AppendBulkString(b, s)
	}
	return b
}

// RESP3, negotiated per connection by HELLO 3.
//
// RESP3 keeps every RESP2 type and adds types a client can decode without
// knowing which command it sent. Six of them are sent here:
//
//	_\r\n                  null, standing for both RESP2 nulls
//	%2\r\n<k><v><k><v>     map: a count of pairs, then each key and value
//	~3\r\n<a><b><c>        set
//	,2.5\r\n               double
//	#t\r\n  #f\r\n         boolean
//	=8\r\ntxt:text\r\n     verbatim string: a three-letter format, a colon, text
//
// A RESP3 client chooses how to decode a reply by its type, so a reply in its
// RESP2 shape on a RESP3 connection is not merely old-fashioned, it is wrong:
// redis-py hands the application a list where HGETALL promised a dict, and
// bytes where ZSCORE promised a float. Every command therefore answers a RESP3
// connection in exactly the shape Redis 8 does, which is mostly obvious - a
// hash is a map, a set is a set, a score is a double - and sometimes not:
// GEODIST stays a bulk string, SRANDMEMBER stays an array, and ZPOPMIN nests
// its member/score pairs only when it was given a count.
//
// Which protocol a reply is framed in belongs to the connection, and reaches
// the encoders as a Framing. Every byte that differs between the protocols is
// written in this file.
//
// What is logged, replicated and dumped is not a reply, and never depends on
// this. The log and the replication feed record commands in the RESP2 arrays a
// client sends, built from the protocol-neutral Append helpers above; a dump
// image is a bulk string either way.

// Framing is the protocol a reply is built in: RESP3, or RESP2 when RESP3 is
// false, as it is for the zero value.
type Framing struct {
	// RESP3 is whether the reply being built is RESP3.
	RESP3 bool
}

// Protocol is the protocol version of the reply being built, 2 or 3, for the
// replies that report it.
func (f Framing) Protocol() int {
	if f.RESP3 {
		return 3
	}
	return 2
}

// The reply values Encode frames differently for the two protocols. Anything
// else Encode is given means the same thing in both.
type (
	// Map is a list of keys and values, alternating: a RESP3 map, and in
	// RESP2 the flat array of its keys and values that RESP2 clients decode
	// into a map. It must have an even number of elements.
	Map []interface{}
	// Double is a float already written as text, in the form the command
	// has always replied with: a RESP3 double, and a bulk string in RESP2.
	Double string
	// Bool is a yes-or-no: a RESP3 boolean, and the integer 1 or 0 in RESP2.
	Bool bool
	// Verbatim is text meant to be shown as it is, INFO's and CLIENT INFO's:
	// a RESP3 verbatim string of format txt, and a bulk string in RESP2.
	Verbatim string
	// Field is a name in a map sent as a simple string in both protocols, as
	// RedisBloom sends the field names of BF.INFO and CF.INFO.
	Field string
)

// Encode builds the reply for a Go value, framed in RESP2: for a reply built
// outside any command, the transport's own errors, and anything else that is
// the same in both protocols.
//
// A string is a bulk string unless isSimpleString asks for the +text form,
// which is for status replies like OK and PONG. Integers of every width are
// RESP integers, an error is a RESP error, nil is the null bulk string, and
// slices become arrays of whatever they hold. A value of a type not listed is
// a bug in the command that produced it, and is answered as an error naming
// the type rather than as a silent nil.
func Encode(value interface{}, isSimpleString bool) []byte {
	return Framing{}.Encode(value, isSimpleString)
}

// EncodeAs is Encode in the protocol given rather than RESP2, for a reply
// built outside a command by a connection that knows its protocol.
func EncodeAs(value interface{}, isSimpleString, resp3 bool) []byte {
	return Framing{RESP3: resp3}.Encode(value, isSimpleString)
}

// Encode is Encode in f's protocol. Only nil and the reply types above come
// out differently in RESP3.
func (f Framing) Encode(value interface{}, isSimpleString bool) []byte {
	switch v := value.(type) {
	case nil:
		return f.Null()
	case Map:
		b := f.AppendMapHeader(nil, len(v)/2)
		for _, x := range v {
			b = append(b, f.Encode(x, false)...)
		}
		return b
	case Double:
		return AppendDouble(f, make([]byte, 0, len(v)+16), string(v))
	case Bool:
		return f.Boolean(bool(v))
	case Verbatim:
		return f.AppendVerbatim(make([]byte, 0, len(v)+24), string(v))
	case Field:
		return AppendSimpleString(make([]byte, 0, len(v)+3), string(v))
	case string:
		if isSimpleString {
			return AppendSimpleString(make([]byte, 0, len(v)+3), v)
		}
		return EncodeString(v)
	case error:
		msg := v.Error()
		return AppendError(make([]byte, 0, len(msg)+3), msg)
	case int:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case int8:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case int16:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case int32:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case int64:
		return AppendInteger(make([]byte, 0, 24), v)
	case uint8:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case uint16:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case uint32:
		return AppendInteger(make([]byte, 0, 24), int64(v))
	case uint64:
		b := append(make([]byte, 0, 24), ':')
		b = strconv.AppendUint(b, v, 10)
		return append(b, '\r', '\n')
	case []string:
		return EncodeStringArray(v)
	case [][]string:
		b := AppendArrayHeader(nil, len(v))
		for _, sa := range v {
			b = append(b, EncodeStringArray(sa)...)
		}
		return b
	case []interface{}:
		b := AppendArrayHeader(nil, len(v))
		for _, x := range v {
			b = append(b, f.Encode(x, false)...)
		}
		return b
	case []int:
		b := AppendArrayHeader(make([]byte, 0, 4+8*len(v)), len(v))
		for _, n := range v {
			b = AppendInteger(b, int64(n))
		}
		return b
	case []int64:
		b := AppendArrayHeader(make([]byte, 0, 4+8*len(v)), len(v))
		for _, n := range v {
			b = AppendInteger(b, n)
		}
		return b
	}
	return AppendError(nil, fmt.Sprintf("ERR cannot encode a %T as a reply", value))
}

// Null is the null bulk string: a key, field or member that is not there.
func (f Framing) Null() []byte {
	if f.RESP3 {
		return constant.Resp3Null
	}
	return constant.RespNil
}

// NullArray is the null array, which RESP2 sends where the reply would
// otherwise have been an array: a counted pop of a missing key, or a GEOPOS
// member that is not there. RESP3 has one null for both.
func (f Framing) NullArray() []byte {
	if f.RESP3 {
		return constant.Resp3Null
	}
	return constant.RespNilArray
}

// EmptyMap and EmptySet answer a missing hash and a missing set.
func (f Framing) EmptyMap() []byte {
	if f.RESP3 {
		return constant.Resp3EmptyMap
	}
	return constant.RespEmptyArray
}

func (f Framing) EmptySet() []byte {
	if f.RESP3 {
		return constant.Resp3EmptySet
	}
	return constant.RespEmptyArray
}

// Boolean is Bool for a reply that is nothing else.
func (f Framing) Boolean(b bool) []byte {
	switch {
	case f.RESP3 && b:
		return constant.Resp3True
	case f.RESP3:
		return constant.Resp3False
	case b:
		return constant.RespOne
	}
	return constant.RespZero
}

// The Append forms build into a reply being assembled. Each has a size
// counterpart below that the admission checks use to size a reply exactly
// before allocating it, and the two are kept beside each other so they agree.

func (f Framing) AppendNull(dst []byte) []byte { return append(dst, f.Null()...) }

func (f Framing) AppendNullArray(dst []byte) []byte { return append(dst, f.NullArray()...) }

// AppendMapHeader starts a map of pairs keys and values.
func (f Framing) AppendMapHeader(dst []byte, pairs int) []byte {
	if !f.RESP3 {
		return AppendArrayHeader(dst, 2*pairs)
	}
	dst = append(dst, '%')
	dst = strconv.AppendInt(dst, int64(pairs), 10)
	return append(dst, '\r', '\n')
}

// AppendSetHeader starts a set of n members.
func (f Framing) AppendSetHeader(dst []byte, n int) []byte {
	if !f.RESP3 {
		return AppendArrayHeader(dst, n)
	}
	dst = append(dst, '~')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, '\r', '\n')
}

// AppendPairHeader starts a [member, score] pair. RESP3 nests each pair as an
// array of two; RESP2 lays the two out flat in the enclosing array, so there is
// nothing to start.
func (f Framing) AppendPairHeader(dst []byte) []byte {
	if !f.RESP3 {
		return dst
	}
	return AppendArrayHeader(dst, 2)
}

// AppendDouble writes a float already written as text, as a RESP3 double or a
// RESP2 bulk string. The text is the same in both - the score or coordinate the
// command has always answered with - so only the framing differs. It takes the
// framing as an argument where the helpers around it are its methods, because
// a method cannot have a type parameter.
func AppendDouble[T string | []byte](f Framing, dst []byte, text T) []byte {
	if !f.RESP3 {
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(text)), 10)
		dst = append(dst, '\r', '\n')
		dst = append(dst, text...)
		return append(dst, '\r', '\n')
	}
	dst = append(dst, ',')
	dst = append(dst, text...)
	return append(dst, '\r', '\n')
}

// AppendVerbatim writes text as a RESP3 verbatim string of format txt - the
// format redis-cli prints as it is - or a RESP2 bulk string. The length counts
// the format and its colon.
func (f Framing) AppendVerbatim(dst []byte, text string) []byte {
	if !f.RESP3 {
		return AppendBulkString(dst, text)
	}
	dst = append(dst, '=')
	dst = strconv.AppendInt(dst, int64(len(text)+4), 10)
	dst = append(dst, "\r\ntxt:"...)
	dst = append(dst, text...)
	return append(dst, '\r', '\n')
}

// MaxReplyBytes is the per-client encoded output ceiling. Amplifying reads
// enforce it before allocating their payload, independently of append mode.
const MaxReplyBytes = 64 << 20

// DecimalDigits is how many digits n takes in decimal.
func DecimalDigits(n int) int {
	digits := 1
	for n >= 10 {
		n /= 10
		digits++
	}
	return digits
}

// AddBulkSize checks before adding, including RESP framing, without
// overflowing: the size with a bulk string of length added, and whether that
// is still within MaxReplyBytes.
func AddBulkSize(size, length int) (int, bool) {
	framing := DecimalDigits(length) + 5
	if length > MaxReplyBytes-size-framing {
		return size, false
	}
	return size + length + framing, true
}

// NullSize is the encoded size of a null.
func (f Framing) NullSize() int { return len(f.Null()) }

// PairHeaderSize is what AppendPairHeader adds per pair.
func (f Framing) PairHeaderSize() int {
	if f.RESP3 {
		return 4
	}
	return 0
}

// MapHeaderSize is the encoded size of a map header for pairs keys and values.
func (f Framing) MapHeaderSize(pairs int) int {
	if f.RESP3 {
		return DecimalDigits(pairs) + 3
	}
	return DecimalDigits(2*pairs) + 3
}

// AddDoubleSize is AddBulkSize for a double of the given text length, checked
// against the output limit the same way.
func (f Framing) AddDoubleSize(size, length int) (int, bool) {
	if !f.RESP3 {
		return AddBulkSize(size, length)
	}
	if length > MaxReplyBytes-size-3 {
		return size, false
	}
	return size + length + 3, true
}
