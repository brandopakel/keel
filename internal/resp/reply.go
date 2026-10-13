package resp

import "github.com/brandopakel/keel/internal/constant"

// Reply is the sink a command writes its answer into: a buffer framed in the
// connection's protocol, which grows only once it has been admitted - within
// its ceiling, and reserved from the transport's budget - and which the
// transport takes by reference when the command is done.
//
// It is the command language's side of phase 4 (docs/embedding-plan.md): a
// typed operation answers with Go values, and the command language writes
// them here in the shape the command's reply has in Redis, so that a handler
// never builds RESP of its own. Three properties of the server's reply path
// hold here, and a handler that writes through a Reply keeps them without
// trying:
//
//   - Reserve before allocate. A reply whose size is known before it is
//     built, a bulk string or a sized array, is sized exactly, admitted, and
//     allocated once, as encodeBoundedString and the collection replies
//     allocate today. Admission is asked only past what was admitted
//     already, and refuses with ReplyTooLarge past what is left of the
//     output limit and with
//     AllocationPressure when the budget cannot hold the reply; a refusal
//     replaces the reply, and nothing more is written. Admission is concrete,
//     what is used of the limit and a budget pointer, checked by static
//     calls, because an
//     interface call on every reply measured as 3% of GET.
//   - Zero copy, and a buffer per command. Bytes returns the buffer itself,
//     which the transport keeps as the reply of a batch of one until it is
//     written, or appends to its arena, as it does with a handler's returned
//     slice today. So a Reply never reuses a buffer: Reset forgets it, and the
//     next command allocates its own.
//   - Zero allocation for a canned reply. +OK, :0, :1, a null, a boolean and
//     the empty map and set are the shared constants the handlers answer with
//     today, held by reference while they are the whole reply, so a command
//     that answers one of them allocates nothing.
type Reply struct {
	Framing
	// Used is how much of the output limit the replies before this one have
	// used already, EXEC's earlier commands': the reply may come to
	// MaxReplyBytes less Used. Zero, the whole limit is the reply's.
	Used int
	// Budget is the transport's allocation budget, which the reply's memory
	// is reserved from; nil reserves nothing.
	Budget *Budget
	// buf is the reply being built, nil until something that is not a
	// canned value alone is written.
	buf []byte
	// canned is a shared constant written while it was the whole reply,
	// held by reference.
	canned []byte
	// admitted is the size admission has allowed the reply to reach, so
	// that a reply reserved once and then filled is asked for once.
	admitted int
	refused  bool
}

// Reset readies r for a command answered in f's protocol. The buffer of the
// command before belongs to the transport now, and is forgotten.
func (r *Reply) Reset(f Framing) {
	r.Framing = f
	r.buf, r.canned, r.admitted, r.refused = nil, nil, 0, false
}

// Bytes is the reply as built: the refusal, if one replaced it. The slice is
// the caller's to keep; the next Reset leaves it alone.
func (r *Reply) Bytes() []byte {
	if r.buf == nil {
		return r.canned
	}
	return r.buf
}

// Refused reports whether an admission replaced the reply with its refusal.
func (r *Reply) Refused() bool { return r.refused }

// Len is how many bytes the reply holds so far.
func (r *Reply) Len() int { return len(r.Bytes()) }

// own gives r a buffer of its own, with room for extra bytes past what it
// holds, moving a canned value into it.
func (r *Reply) own(extra int) {
	if r.buf != nil {
		return
	}
	r.buf = append(make([]byte, 0, len(r.canned)+extra), r.canned...)
	r.canned = nil
}

// admit asks for the reply to grow to size bytes in all, and makes room. It
// asks only past what was admitted already: a reply Reserve sized is filled
// without asking again, so the budget is charged for it once. A refusal
// replaces what has been written; anything written after it is dropped.
func (r *Reply) admit(size int) bool {
	if r.refused {
		return false
	}
	if size > r.admitted {
		if refusal := r.admission(size); refusal != nil {
			r.refuse(refusal)
			return false
		}
	}
	r.admitted = max(r.admitted, size)
	r.own(size - r.Len())
	if cap(r.buf) < size {
		grown := make([]byte, len(r.buf), size)
		copy(grown, r.buf)
		r.buf = grown
	}
	return true
}

// admission is nil when a reply of size encoded bytes may be built, and
// otherwise the refusal to send in its place: past the ceiling, or past what
// the budget can hold.
func (r *Reply) admission(size int) []byte {
	if size > MaxReplyBytes-r.Used {
		return ReplyTooLarge
	}
	if r.Budget != nil && !r.Budget.ReserveReply(size) {
		return AllocationPressure
	}
	return nil
}

// refuse replaces the reply with refusal, by reference, and ends it.
func (r *Reply) refuse(refusal []byte) {
	r.buf, r.canned, r.refused = nil, refusal, true
}

// Raw appends bytes already encoded: a canned reply, held by reference while
// it is the whole reply, or one a handler built itself.
func (r *Reply) Raw(p []byte) *Reply {
	switch {
	case r.refused:
	case r.buf == nil && r.canned == nil:
		r.canned = p
	default:
		r.own(len(p))
		r.buf = append(r.buf, p...)
	}
	return r
}

// Status writes a simple string, +OK or +PONG.
func (r *Reply) Status(s string) *Reply {
	if s == "OK" {
		return r.Raw(constant.RespOk)
	}
	if !r.refused {
		r.own(len(s) + 3)
		r.buf = AppendSimpleString(r.buf, s)
	}
	return r
}

// Error writes an error reply.
func (r *Reply) Error(err error) *Reply {
	if !r.refused {
		msg := err.Error()
		r.own(len(msg) + 3)
		r.buf = AppendError(r.buf, msg)
	}
	return r
}

// Int writes an integer; 0 and 1 are the shared constants.
func (r *Reply) Int(n int64) *Reply {
	switch n {
	case 0:
		return r.Raw(constant.RespZero)
	case 1:
		return r.Raw(constant.RespOne)
	}
	if !r.refused {
		r.own(24)
		r.buf = AppendInteger(r.buf, n)
	}
	return r
}

// Bulk writes a bulk string, sized and admitted before it is copied.
func (r *Reply) Bulk(s string) *Reply {
	if r.buf == nil && r.canned == nil && !r.refused {
		// The reply is this one value, as GET's is: sized, admitted and
		// allocated once, with nothing before it to keep.
		size, fits := AddBulkSize(0, len(s))
		if !fits {
			return r.tooLarge()
		}
		if refusal := r.admission(size); refusal != nil {
			r.refuse(refusal)
			return r
		}
		r.admitted = size
		r.buf = AppendBulkString(make([]byte, 0, size), s)
		return r
	}
	size, fits := AddBulkSize(r.Len(), len(s))
	if !fits {
		return r.tooLarge()
	}
	if r.admit(size) {
		r.buf = AppendBulkString(r.buf, s)
	}
	return r
}

// Null writes the null bulk string, and NullArray the null array; RESP3 has
// one null for both.
func (r *Reply) Null() *Reply { return r.Raw(r.Framing.Null()) }

func (r *Reply) NullArray() *Reply { return r.Raw(r.Framing.NullArray()) }

// EmptyMap and EmptySet answer a missing hash and a missing set.
func (r *Reply) EmptyMap() *Reply { return r.Raw(r.Framing.EmptyMap()) }

func (r *Reply) EmptySet() *Reply { return r.Raw(r.Framing.EmptySet()) }

// Bool writes a yes-or-no: a RESP3 boolean, and 1 or 0 in RESP2.
func (r *Reply) Bool(b bool) *Reply { return r.Raw(r.Framing.Boolean(b)) }

// Double writes a float already written as text: a RESP3 double, and a bulk
// string in RESP2.
func (r *Reply) Double(text string) *Reply {
	if !r.refused {
		r.own(len(text) + 16)
		r.buf = AppendDouble(r.Framing, r.buf, text)
	}
	return r
}

// Verbatim writes text shown as it is: a RESP3 verbatim string of format
// txt, and a bulk string in RESP2.
func (r *Reply) Verbatim(text string) *Reply {
	size, fits := AddBulkSize(r.Len(), len(text)+4)
	if !fits {
		return r.tooLarge()
	}
	if r.admit(size) {
		r.buf = r.Framing.AppendVerbatim(r.buf, text)
	}
	return r
}

// Array starts an array of n values, which the caller then writes. Reserve
// sizes the whole reply first for a reply whose size is known.
func (r *Reply) Array(n int) *Reply {
	if !r.refused {
		r.own(DecimalDigits(n) + 3)
		r.buf = AppendArrayHeader(r.buf, n)
	}
	return r
}

// Set starts a set of n members: a RESP3 set, an array in RESP2.
func (r *Reply) Set(n int) *Reply {
	if !r.refused {
		r.own(DecimalDigits(n) + 3)
		r.buf = r.Framing.AppendSetHeader(r.buf, n)
	}
	return r
}

// Map starts a map of pairs keys and values: a RESP3 map, and in RESP2 the
// flat array of its keys and values.
func (r *Reply) Map(pairs int) *Reply {
	if !r.refused {
		r.own(r.Framing.MapHeaderSize(pairs))
		r.buf = r.Framing.AppendMapHeader(r.buf, pairs)
	}
	return r
}

// Pair starts a [member, score] pair: nested in RESP3, flat in RESP2.
func (r *Reply) Pair() *Reply {
	if !r.refused {
		r.own(r.Framing.PairHeaderSize())
		r.buf = r.Framing.AppendPairHeader(r.buf)
	}
	return r
}

// Reserve admits a reply of size encoded bytes in all and allocates it once,
// for a reply a command has sized before building it: an array of bulk
// strings, a map. It reports whether the reply may be built; refused, the
// refusal is the reply.
func (r *Reply) Reserve(size int) bool {
	if size > MaxReplyBytes {
		r.tooLarge()
		return false
	}
	return r.admit(size)
}

// tooLarge replaces the reply with the output limit's refusal.
func (r *Reply) tooLarge() *Reply {
	r.refuse(ReplyTooLarge)
	return r
}

// ReplyTooLarge refuses a reply that would pass MaxReplyBytes.
var ReplyTooLarge = []byte("-ERR reply exceeds the 64 MiB output limit\r\n")
