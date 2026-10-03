package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
)

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
// Which protocol a reply is framed in belongs to the connection. It reaches
// here on the Command, and EvalAndResponse holds it in replyRESP3 for as long
// as the command runs, the way the log's staging state is held for exactly one
// command. A handler says what it is answering - a map, a set, a score, a
// yes-or-no - through the helpers below, and every byte that differs between
// the protocols is written in this file.
//
// What is logged, replicated and dumped is not a reply, and never depends on
// this. The log and the replication feed record commands in the RESP2 arrays a
// client sends, built by appendCommand from the protocol-neutral append helpers
// in resp.go; a KEEL.DUMP image is a bulk string either way. Log replay and
// replica apply answer nobody and always run as RESP2.

// replyRESP3 is whether the reply being built is RESP3.
//
// EvalAndResponse sets it from the command and restores it when the command
// returns, and EncodeAs sets it for the replies the connection layer builds
// itself. Commands execute one at a time on the event loop, so one variable is
// enough. Encode reads it only for nil and the Reply types below, which is why
// the replica's requests to its primary, encoded as []string on a goroutine of
// their own, never touch it.
var replyRESP3 bool

// replyProtocol is the protocol version of the reply being built, 2 or 3, for
// the replies that report it.
func replyProtocol() int {
	if replyRESP3 {
		return 3
	}
	return 2
}

// EncodeAs is Encode for a reply built outside EvalAndResponse, in the
// protocol given rather than that of a command in progress. HELLO and CLIENT
// are answered by the connection layer, which knows the connection's protocol
// and runs no command through here.
func EncodeAs(value interface{}, isSimpleString, resp3 bool) []byte {
	saved := replyRESP3
	replyRESP3 = resp3
	defer func() { replyRESP3 = saved }()
	return Encode(value, isSimpleString)
}

// The reply values Encode frames differently for the two protocols. Anything
// else Encode is given means the same thing in both.
type (
	// ReplyMap is a list of keys and values, alternating: a RESP3 map, and in
	// RESP2 the flat array of its keys and values that RESP2 clients decode into
	// a map. It must have an even number of elements.
	ReplyMap []interface{}
	// ReplyDouble is a float already written as text, in the form the command
	// has always replied with: a RESP3 double, and a bulk string in RESP2.
	ReplyDouble string
	// ReplyBool is a yes-or-no: a RESP3 boolean, and the integer 1 or 0 in RESP2.
	ReplyBool bool
	// ReplyVerbatim is text meant to be shown as it is, INFO's and CLIENT
	// INFO's: a RESP3 verbatim string of format txt, and a bulk string in RESP2.
	ReplyVerbatim string
)

// replyTextBool is a yes-or-no that RESP2 has always spelled as the bulk string
// "1" or "0" here, where RedisBloom sends an integer: CF.MEXISTS. RESP3 sends
// RedisBloom's boolean; a RESP2 client of this server keeps the strings it
// already decodes.
type replyTextBool bool

// infoField is a field name in BF.INFO and CF.INFO. RedisBloom sends these as
// simple strings, and RESP3 follows it; this server's RESP2 replies have always
// sent them as bulk strings, which a RESP2 client reads the same way, and keep
// doing so.
type infoField string

// infoEntry is one field of BF.INFO or CF.INFO.
type infoEntry struct {
	name  string
	value int64
}

// infoReply answers BF.INFO and CF.INFO. RESP3 is RedisBloom's form, a map
// from simple-string names to integers. RESP2 is what each has always sent
// here: bulk-string names, and integers - or, for CF.INFO, textValues, the
// same numbers as decimal bulk strings.
func infoReply(entries []infoEntry, textValues bool) []byte {
	out := make(ReplyMap, 0, 2*len(entries))
	for _, e := range entries {
		var value interface{} = e.value
		if textValues && !replyRESP3 {
			value = strconv.FormatInt(e.value, 10)
		}
		out = append(out, infoField(e.name), value)
	}
	return Encode(out, false)
}

// nullReply is the null bulk string: a key, field or member that is not there.
func nullReply() []byte {
	if replyRESP3 {
		return constant.Resp3Null
	}
	return constant.RespNil
}

// nullArrayReply is the null array, which RESP2 sends where the reply would
// otherwise have been an array: a counted pop of a missing key, or a GEOPOS
// member that is not there. RESP3 has one null for both.
func nullArrayReply() []byte {
	if replyRESP3 {
		return constant.Resp3Null
	}
	return constant.RespNilArray
}

// emptyMapReply and emptySetReply answer a missing hash and a missing set.
func emptyMapReply() []byte {
	if replyRESP3 {
		return constant.Resp3EmptyMap
	}
	return constant.RespEmptyArray
}

func emptySetReply() []byte {
	if replyRESP3 {
		return constant.Resp3EmptySet
	}
	return constant.RespEmptyArray
}

// boolReply is ReplyBool for a reply that is nothing else.
func boolReply(b bool) []byte {
	switch {
	case replyRESP3 && b:
		return constant.Resp3True
	case replyRESP3:
		return constant.Resp3False
	case b:
		return constant.RespOne
	}
	return constant.RespZero
}

// The append forms build into a reply being assembled. Each has a size
// counterpart below that the admission checks use to size a reply exactly
// before allocating it, and the two are kept beside each other so they agree.

func appendNull(dst []byte) []byte { return append(dst, nullReply()...) }

func appendNullArray(dst []byte) []byte { return append(dst, nullArrayReply()...) }

// appendMapHeader starts a map of pairs keys and values.
func appendMapHeader(dst []byte, pairs int) []byte {
	if !replyRESP3 {
		return appendArrayHeader(dst, 2*pairs)
	}
	dst = append(dst, '%')
	dst = strconv.AppendInt(dst, int64(pairs), 10)
	return append(dst, '\r', '\n')
}

// appendSetHeader starts a set of n members.
func appendSetHeader(dst []byte, n int) []byte {
	if !replyRESP3 {
		return appendArrayHeader(dst, n)
	}
	dst = append(dst, '~')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, '\r', '\n')
}

// appendPairHeader starts a [member, score] pair. RESP3 nests each pair as an
// array of two; RESP2 lays the two out flat in the enclosing array, so there is
// nothing to start.
func appendPairHeader(dst []byte) []byte {
	if !replyRESP3 {
		return dst
	}
	return appendArrayHeader(dst, 2)
}

// appendDouble writes a float already written as text, as a RESP3 double or a
// RESP2 bulk string. The text is the same in both - the score or coordinate the
// command has always answered with - so only the framing differs.
func appendDouble[T string | []byte](dst []byte, text T) []byte {
	if !replyRESP3 {
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

func appendBool(dst []byte, b bool) []byte { return append(dst, boolReply(b)...) }

// appendVerbatim writes text as a RESP3 verbatim string of format txt - the
// format redis-cli prints as it is - or a RESP2 bulk string. The length counts
// the format and its colon.
func appendVerbatim(dst []byte, text string) []byte {
	if !replyRESP3 {
		return appendBulkString(dst, text)
	}
	dst = append(dst, '=')
	dst = strconv.AppendInt(dst, int64(len(text)+4), 10)
	dst = append(dst, "\r\ntxt:"...)
	dst = append(dst, text...)
	return append(dst, '\r', '\n')
}

// nullSize is the encoded size of a null.
func nullSize() int { return len(nullReply()) }

// pairHeaderSize is what appendPairHeader adds per pair.
func pairHeaderSize() int {
	if replyRESP3 {
		return 4
	}
	return 0
}

// mapHeaderSize is the encoded size of a map header for pairs keys and values.
func mapHeaderSize(pairs int) int {
	if replyRESP3 {
		return decimalDigits(pairs) + 3
	}
	return decimalDigits(2*pairs) + 3
}

// addDoubleSize is addBulkSize for a double of the given text length, checked
// against the output limit the same way.
func addDoubleSize(size, length int) (int, bool) {
	if !replyRESP3 {
		return addBulkSize(size, length)
	}
	if length > MaxReplyBytes-size-3 {
		return size, false
	}
	return size + length + 3, true
}
