package resp

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/constant"
)

func TestReplyWritesEachShapeInBothProtocols(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		write        func(r *Reply)
		resp2, resp3 string
	}{
		{"status", func(r *Reply) { r.Status("OK") }, "+OK\r\n", "+OK\r\n"},
		{"pong", func(r *Reply) { r.Status("PONG") }, "+PONG\r\n", "+PONG\r\n"},
		{"error", func(r *Reply) { r.Error(errors.New("ERR no\r\nway")) }, "-ERR no  way\r\n", "-ERR no  way\r\n"},
		{"int", func(r *Reply) { r.Int(-7) }, ":-7\r\n", ":-7\r\n"},
		{"ints", func(r *Reply) { r.Int(0).Int(1).Int(2) }, ":0\r\n:1\r\n:2\r\n", ":0\r\n:1\r\n:2\r\n"},
		{"bulk", func(r *Reply) { r.Bulk("a\r\nb") }, "$4\r\na\r\nb\r\n", "$4\r\na\r\nb\r\n"},
		{"null", func(r *Reply) { r.Null() }, "$-1\r\n", "_\r\n"},
		{"null array", func(r *Reply) { r.NullArray() }, "*-1\r\n", "_\r\n"},
		{"empty map", func(r *Reply) { r.EmptyMap() }, "*0\r\n", "%0\r\n"},
		{"empty set", func(r *Reply) { r.EmptySet() }, "*0\r\n", "~0\r\n"},
		{"bool", func(r *Reply) { r.Bool(true).Bool(false) }, ":1\r\n:0\r\n", "#t\r\n#f\r\n"},
		{"double", func(r *Reply) { r.Double("2.5") }, "$3\r\n2.5\r\n", ",2.5\r\n"},
		{"verbatim", func(r *Reply) { r.Verbatim("hi") }, "$2\r\nhi\r\n", "=6\r\ntxt:hi\r\n"},
		{"array", func(r *Reply) { r.Array(2).Bulk("a").Int(1) }, "*2\r\n$1\r\na\r\n:1\r\n", "*2\r\n$1\r\na\r\n:1\r\n"},
		{"set", func(r *Reply) { r.Set(1).Bulk("a") }, "*1\r\n$1\r\na\r\n", "~1\r\n$1\r\na\r\n"},
		{"map", func(r *Reply) { r.Map(1).Bulk("f").Bulk("v") }, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n", "%1\r\n$1\r\nf\r\n$1\r\nv\r\n"},
		{"pair", func(r *Reply) { r.Array(1).Pair().Bulk("m").Double("1") }, "*1\r\n$1\r\nm\r\n$1\r\n1\r\n", "*1\r\n*2\r\n$1\r\nm\r\n,1\r\n"},
		{"raw", func(r *Reply) { r.Raw([]byte("+PONG\r\n")) }, "+PONG\r\n", "+PONG\r\n"},
		{"raw after canned", func(r *Reply) { r.Null().Raw([]byte("x")) }, "$-1\r\nx", "_\r\nx"},
	} {
		for _, resp3 := range []bool{false, true} {
			var r Reply
			r.Reset(Framing{RESP3: resp3})
			tc.write(&r)
			want := tc.resp2
			if resp3 {
				want = tc.resp3
			}
			require.Equal(t, want, string(r.Bytes()), "%s in RESP%d", tc.name, r.Protocol())
			require.False(t, r.Refused())
			require.Equal(t, len(want), r.Len())
		}
	}
}

// first is the address of a slice's first byte, to tell a shared constant
// from a copy.
func first(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

func TestReplyAnswersCannedRepliesWithoutAllocating(t *testing.T) {
	// Not parallel: AllocsPerRun refuses to run beside other tests.
	for _, tc := range []struct {
		write  func(r *Reply)
		canned []byte
	}{
		{func(r *Reply) { r.Status("OK") }, constant.RespOk},
		{func(r *Reply) { r.Int(0) }, constant.RespZero},
		{func(r *Reply) { r.Int(1) }, constant.RespOne},
		{func(r *Reply) { r.Bool(true) }, constant.RespOne},
		{func(r *Reply) { r.Null() }, constant.RespNil},
		{func(r *Reply) { r.NullArray() }, constant.RespNilArray},
		{func(r *Reply) { r.EmptySet() }, constant.RespEmptyArray},
	} {
		var r Reply
		allocs := testing.AllocsPerRun(100, func() {
			r.Reset(Framing{})
			tc.write(&r)
		})
		require.Zero(t, allocs, "%q", tc.canned)
		require.Equal(t, first(tc.canned), first(r.Bytes()), "%q is the shared constant, by reference", tc.canned)
	}
	var r Reply
	r.Reset(Framing{RESP3: true})
	r.Null()
	require.Equal(t, first(constant.Resp3Null), first(r.Bytes()))
	// A canned value followed by anything is copied into a buffer of the
	// reply's own, and the constant is untouched.
	r.Int(2)
	require.Equal(t, "_\r\n:2\r\n", string(r.Bytes()))
	require.Equal(t, "_\r\n", string(constant.Resp3Null))
}

func TestReplyGivesEachCommandABufferOfItsOwn(t *testing.T) {
	t.Parallel()
	var r Reply
	r.Reset(Framing{})
	r.Bulk("first")
	kept := r.Bytes()
	r.Reset(Framing{})
	require.Empty(t, r.Bytes())
	r.Bulk("second")
	require.Equal(t, "$5\r\nfirst\r\n", string(kept), "the transport's reply is not written over by the next command")
	require.Equal(t, "$6\r\nsecond\r\n", string(r.Bytes()))
	require.NotEqual(t, first(kept), first(r.Bytes()))
}

func TestReplyAdmitsBeforeItAllocates(t *testing.T) {
	// Not parallel: AllocsPerRun refuses to run beside other tests.
	budget := &Budget{Limit: 1 << 20}
	r := &Reply{Budget: budget}
	value := strings.Repeat("v", 50)
	allocs := testing.AllocsPerRun(10, func() {
		budget.Begin(0)
		r.Reset(Framing{})
		r.Budget = budget
		r.Bulk(value)
	})
	require.Equal(t, 1.0, allocs, "the buffer, once")
	require.Equal(t, len(value)+7, cap(r.Bytes()), "allocated to the admitted size")
	require.Equal(t, 3*4096, budget.ReplyReserved, "reserved from the budget before the copy: three pages")
	require.False(t, r.Refused())

	// Past what is left of the output limit: its refusal replaces what was
	// written.
	r.Reset(Framing{})
	r.Used = MaxReplyBytes - 100
	r.Int(2).Bulk(strings.Repeat("v", 200))
	require.True(t, r.Refused())
	require.Equal(t, string(ReplyTooLarge), string(r.Bytes()), "the refusal replaces what was written")
	require.Equal(t, first(ReplyTooLarge), first(r.Bytes()), "by reference")
	r.Status("OK").Int(2).Null().Array(1).Raw([]byte("x")).Bulk("y").Double("1")
	require.Equal(t, string(ReplyTooLarge), string(r.Bytes()), "nothing after a refusal is written")
	require.False(t, r.Reserve(1))
	// Nothing left of the limit, as inside an EXEC whose replies fill it:
	// every reply that is not canned is refused.
	r.Reset(Framing{})
	r.Used = MaxReplyBytes
	require.True(t, r.Bulk("x").Refused())
	require.Equal(t, string(ReplyTooLarge), string(r.Bytes()))

	// Past what the budget holds: the budget's refusal.
	small := &Budget{Limit: 4096}
	small.Begin(0)
	r.Reset(Framing{})
	r.Budget, r.Used = small, 0
	r.Bulk("x")
	require.True(t, r.Refused())
	require.Equal(t, string(AllocationPressure), string(r.Bytes()))
	require.Equal(t, uint64(1), small.Refusals)

	// A reply reserved once is charged once, however it is filled; growing
	// past the reservation asks again, for the whole.
	budget.Begin(0)
	r.Reset(Framing{})
	r.Budget, r.Used = budget, 0
	require.True(t, r.Reserve(64))
	require.GreaterOrEqual(t, cap(r.Bytes()), 64)
	r.Array(2).Bulk("a").Bulk("b")
	require.Equal(t, "*2\r\n$1\r\na\r\n$1\r\nb\r\n", string(r.Bytes()))
	require.Equal(t, 3*4096, budget.ReplyReserved, "once")
	r.Bulk(strings.Repeat("v", 5000))
	require.False(t, r.Refused())
	require.Equal(t, 3*4096+3*8192, budget.ReplyReserved, "again, for the whole")
}

func TestReplyRefusesWhatPassesTheOutputLimit(t *testing.T) {
	t.Parallel()
	var r Reply
	require.False(t, r.Reserve(MaxReplyBytes+1))
	require.Equal(t, string(ReplyTooLarge), string(r.Bytes()))
	r.Reset(Framing{})
	require.True(t, r.Reserve(MaxReplyBytes))
	r.Reset(Framing{})
	// A bulk string whose framing alone would pass the limit is refused
	// without being copied.
	r.buf = make([]byte, MaxReplyBytes-4)
	r.Bulk("x")
	require.True(t, r.Refused())
	require.Equal(t, string(ReplyTooLarge), string(r.Bytes()))
}

func TestReplyWithoutAdmissionGrowsAsNeeded(t *testing.T) {
	t.Parallel()
	var r Reply
	r.Reset(Framing{RESP3: true})
	for i := 0; i < 100; i++ {
		r.Int(int64(i))
	}
	require.Equal(t, 100, strings.Count(string(r.Bytes()), "\r\n"))
	require.False(t, r.Refused())
	r.Bulk(strings.Repeat("v", 1<<16))
	require.Equal(t, 102, strings.Count(string(r.Bytes()), "\r\n"))
}

// TestSingleBulkIsAReplysFirstBulk holds SingleBulk to what a Reply's Bulk
// writes as its first value: the same bytes, the same reservation, the same
// refusals, by reference.
func TestSingleBulkIsAReplysFirstBulk(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "v", strings.Repeat("v", 5000)} {
		a, b := &Budget{Limit: 1 << 20}, &Budget{Limit: 1 << 20}
		a.Begin(0)
		b.Begin(0)
		r := Reply{Budget: a}
		want := r.Bulk(value).Bytes()
		got := SingleBulk(0, b, value)
		require.Equal(t, string(want), string(got), "%d bytes", len(value))
		require.Equal(t, cap(want), cap(got), "allocated to the admitted size")
		require.Equal(t, a.ReplyReserved, b.ReplyReserved, "reserved alike")
	}
	require.Equal(t, first(ReplyTooLarge), first(SingleBulk(MaxReplyBytes, nil, "x")), "nothing left of the limit")
	require.Equal(t, first(ReplyTooLarge), first(SingleBulk(MaxReplyBytes-6, nil, "xx")), "one byte past it")
	require.Equal(t, "$1\r\nx\r\n", string(SingleBulk(MaxReplyBytes-7, nil, "x")), "exactly at it")
	small := &Budget{Limit: 4096}
	small.Begin(0)
	require.Equal(t, first(AllocationPressure), first(SingleBulk(0, small, "x")), "past what the budget holds")
	require.Equal(t, uint64(1), small.Refusals)
}
