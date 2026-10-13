package core

import "github.com/brandopakel/keel/internal/resp"

// How handlers learn the protocol.
//
// Which protocol a reply is framed in belongs to the connection. It reaches
// here on the Command, and EvalAndResponse holds it in the framing of the
// engine running the command for as long as the command runs, the way the
// log's staging state is held for exactly one command. A handler says what it
// is answering - a map, a set, a score, a yes-or-no - through the framing's
// methods below, which the Engine promotes, so that a handler writes
// e.nullReply() or e.encode(...). Every byte that differs between the
// protocols is written in internal/resp; each method here is the resp.Framing
// method of the same meaning, inlined.
//
// Log replay and replica apply answer nobody and always run as RESP2.

// framing is the protocol a reply is built in: RESP3, or RESP2 when
// replyRESP3 is false, as it is for the zero value.
//
// An Engine embeds the framing of the command running on it. EvalAndResponse
// sets it from the command and restores it when the command returns, so
// nothing after the command inherits it. One command runs on an engine at a
// time, so one framing per engine is enough. A reply built outside any command
// names its protocol instead: Encode's is RESP2, and EncodeAs takes the
// connection's.
type framing struct {
	// replyRESP3 is whether the reply being built is RESP3.
	replyRESP3 bool
}

// wire is f as the encoders take it.
func (f framing) wire() resp.Framing { return resp.Framing{RESP3: f.replyRESP3} }

// replyProtocol is the protocol version of the reply being built, 2 or 3,
// for the replies that report it.
func (f framing) replyProtocol() int { return f.wire().Protocol() }

// encode is Encode in f's protocol.
func (f framing) encode(value interface{}, isSimpleString bool) []byte {
	return f.wire().Encode(value, isSimpleString)
}

// nullReply is the null bulk string: a key, field or member that is not there.
func (f framing) nullReply() []byte { return f.wire().Null() }

// nullArrayReply is the null array, which RESP2 sends where the reply would
// otherwise have been an array. RESP3 has one null for both.
func (f framing) nullArrayReply() []byte { return f.wire().NullArray() }

// emptyMapReply and emptySetReply answer a missing hash and a missing set.
func (f framing) emptyMapReply() []byte { return f.wire().EmptyMap() }

func (f framing) emptySetReply() []byte { return f.wire().EmptySet() }

// boolReply is ReplyBool for a reply that is nothing else.
func (f framing) boolReply(b bool) []byte { return f.wire().Boolean(b) }

// The append forms build into a reply being assembled, each with the size
// counterpart the admission checks use to size a reply exactly before
// allocating it.

func (f framing) appendNull(dst []byte) []byte { return f.wire().AppendNull(dst) }

func (f framing) appendNullArray(dst []byte) []byte { return f.wire().AppendNullArray(dst) }

func (f framing) appendMapHeader(dst []byte, pairs int) []byte {
	return f.wire().AppendMapHeader(dst, pairs)
}

func (f framing) appendSetHeader(dst []byte, n int) []byte { return f.wire().AppendSetHeader(dst, n) }

func (f framing) appendPairHeader(dst []byte) []byte { return f.wire().AppendPairHeader(dst) }

func (f framing) appendVerbatim(dst []byte, text string) []byte {
	return f.wire().AppendVerbatim(dst, text)
}

// appendDouble writes a float already written as text, as a RESP3 double or
// a RESP2 bulk string. It takes the framing as an argument where the helpers
// around it are its methods, because a method cannot have a type parameter.
func appendDouble[T string | []byte](f framing, dst []byte, text T) []byte {
	return resp.AppendDouble(f.wire(), dst, text)
}

func (f framing) nullSize() int { return f.wire().NullSize() }

func (f framing) pairHeaderSize() int { return f.wire().PairHeaderSize() }

func (f framing) mapHeaderSize(pairs int) int { return f.wire().MapHeaderSize(pairs) }

func (f framing) addDoubleSize(size, length int) (int, bool) {
	return f.wire().AddDoubleSize(size, length)
}

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
