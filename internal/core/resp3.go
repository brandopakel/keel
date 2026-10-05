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
// here on the Command, and evalAndResponse holds it in the framing of the
// engine running the command for as long as the command runs, the way the
// log's staging state is held for exactly one command. A handler says what it
// is answering - a map, a set, a score, a yes-or-no - through the framing's
// methods below, which the Engine promotes, so that a handler writes
// e.nullReply() or e.encode(...); every byte that differs between the
// protocols is written in this file.
//
// What is logged, replicated and dumped is not a reply, and never depends on
// this. The log and the replication feed record commands in the RESP2 arrays a
// client sends, built by appendCommand from the protocol-neutral append helpers
// in resp.go; a KEEL.DUMP image is a bulk string either way. Log replay and
// replica apply answer nobody and always run as RESP2.

// framing is the protocol a reply is built in: RESP3, or RESP2 when
// replyRESP3 is false, as it is for the zero value.
//
// An Engine embeds the framing of the command running on it. evalAndResponse
// sets it from the command and restores it when the command returns, so
// nothing after the command inherits it. One command runs on an engine at a
// time, so one framing per engine is enough. A reply built outside any command
// names its protocol instead, with a framing of its own: Encode's is RESP2,
// and EncodeAs takes the connection's. So the replica's requests to its
// primary, encoded on a goroutine of their own, and the connection layer's
// replies read no engine at all.
type framing struct {
	// replyRESP3 is whether the reply being built is RESP3.
	replyRESP3 bool
}

// replyProtocol is the protocol version of the reply being built, 2 or 3, for
// the replies that report it.
func (f framing) replyProtocol() int {
	if f.replyRESP3 {
		return 3
	}
	return 2
}

// EncodeAs is Encode for a reply built outside a command, in the protocol
// given rather than RESP2. HELLO and CLIENT are answered by the connection
// layer, which knows the connection's protocol and runs no command through
// here.
func EncodeAs(value interface{}, isSimpleString, resp3 bool) []byte {
	return framing{replyRESP3: resp3}.encode(value, isSimpleString)
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

// infoField is a field name in BF.INFO and CF.INFO, which RedisBloom sends as
// a simple string in both protocols.
type infoField string

// infoEntry is one field of BF.INFO or CF.INFO: an integer, or nil for a value
// the filter does not have - the expansion rate of one that does not grow.
type infoEntry struct {
	name  string
	value interface{}
}

// infoReply answers BF.INFO and CF.INFO as RedisBloom does: a map from
// simple-string names to integers, which RESP2 lays out as the flat array of
// its names and values.
func (f framing) infoReply(entries []infoEntry) []byte {
	out := make(ReplyMap, 0, 2*len(entries))
	for _, e := range entries {
		out = append(out, infoField(e.name), e.value)
	}
	return f.encode(out, false)
}

// infoFieldReply answers BF.INFO for the one field asked for, as RedisBloom
// does: a map of that one name in RESP3, and in RESP2 an array holding only
// the value, with no name.
func (f framing) infoFieldReply(e infoEntry) []byte {
	if f.replyRESP3 {
		return f.infoReply([]infoEntry{e})
	}
	return f.encode([]interface{}{e.value}, false)
}

// nullReply is the null bulk string: a key, field or member that is not there.
func (f framing) nullReply() []byte {
	if f.replyRESP3 {
		return constant.Resp3Null
	}
	return constant.RespNil
}

// nullArrayReply is the null array, which RESP2 sends where the reply would
// otherwise have been an array: a counted pop of a missing key, or a GEOPOS
// member that is not there. RESP3 has one null for both.
func (f framing) nullArrayReply() []byte {
	if f.replyRESP3 {
		return constant.Resp3Null
	}
	return constant.RespNilArray
}

// emptyMapReply and emptySetReply answer a missing hash and a missing set.
func (f framing) emptyMapReply() []byte {
	if f.replyRESP3 {
		return constant.Resp3EmptyMap
	}
	return constant.RespEmptyArray
}

func (f framing) emptySetReply() []byte {
	if f.replyRESP3 {
		return constant.Resp3EmptySet
	}
	return constant.RespEmptyArray
}

// boolReply is ReplyBool for a reply that is nothing else.
func (f framing) boolReply(b bool) []byte {
	switch {
	case f.replyRESP3 && b:
		return constant.Resp3True
	case f.replyRESP3:
		return constant.Resp3False
	case b:
		return constant.RespOne
	}
	return constant.RespZero
}

// The append forms build into a reply being assembled. Each has a size
// counterpart below that the admission checks use to size a reply exactly
// before allocating it, and the two are kept beside each other so they agree.

func (f framing) appendNull(dst []byte) []byte { return append(dst, f.nullReply()...) }

func (f framing) appendNullArray(dst []byte) []byte { return append(dst, f.nullArrayReply()...) }

// appendMapHeader starts a map of pairs keys and values.
func (f framing) appendMapHeader(dst []byte, pairs int) []byte {
	if !f.replyRESP3 {
		return appendArrayHeader(dst, 2*pairs)
	}
	dst = append(dst, '%')
	dst = strconv.AppendInt(dst, int64(pairs), 10)
	return append(dst, '\r', '\n')
}

// appendSetHeader starts a set of n members.
func (f framing) appendSetHeader(dst []byte, n int) []byte {
	if !f.replyRESP3 {
		return appendArrayHeader(dst, n)
	}
	dst = append(dst, '~')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, '\r', '\n')
}

// appendPairHeader starts a [member, score] pair. RESP3 nests each pair as an
// array of two; RESP2 lays the two out flat in the enclosing array, so there is
// nothing to start.
func (f framing) appendPairHeader(dst []byte) []byte {
	if !f.replyRESP3 {
		return dst
	}
	return appendArrayHeader(dst, 2)
}

// appendDouble writes a float already written as text, as a RESP3 double or a
// RESP2 bulk string. The text is the same in both - the score or coordinate the
// command has always answered with - so only the framing differs. It takes the
// framing as an argument where the helpers around it are its methods, because
// a method cannot have a type parameter.
func appendDouble[T string | []byte](f framing, dst []byte, text T) []byte {
	if !f.replyRESP3 {
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

// appendVerbatim writes text as a RESP3 verbatim string of format txt - the
// format redis-cli prints as it is - or a RESP2 bulk string. The length counts
// the format and its colon.
func (f framing) appendVerbatim(dst []byte, text string) []byte {
	if !f.replyRESP3 {
		return appendBulkString(dst, text)
	}
	dst = append(dst, '=')
	dst = strconv.AppendInt(dst, int64(len(text)+4), 10)
	dst = append(dst, "\r\ntxt:"...)
	dst = append(dst, text...)
	return append(dst, '\r', '\n')
}

// nullSize is the encoded size of a null.
func (f framing) nullSize() int { return len(f.nullReply()) }

// pairHeaderSize is what appendPairHeader adds per pair.
func (f framing) pairHeaderSize() int {
	if f.replyRESP3 {
		return 4
	}
	return 0
}

// mapHeaderSize is the encoded size of a map header for pairs keys and values.
func (f framing) mapHeaderSize(pairs int) int {
	if f.replyRESP3 {
		return decimalDigits(pairs) + 3
	}
	return decimalDigits(2*pairs) + 3
}

// addDoubleSize is addBulkSize for a double of the given text length, checked
// against the output limit the same way.
func (f framing) addDoubleSize(size, length int) (int, bool) {
	if !f.replyRESP3 {
		return addBulkSize(size, length)
	}
	if length > MaxReplyBytes-size-3 {
		return size, false
	}
	return size + length + 3, true
}
