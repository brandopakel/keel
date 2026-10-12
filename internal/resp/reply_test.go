package resp

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// admission refuses any reply over limit bytes with refusal, and counts what
// it was asked.
type admission struct {
	limit   int
	refusal []byte
	asked   []int
}

func (a *admission) AdmitReply(size int) []byte {
	a.asked = append(a.asked, size)
	if size > a.limit {
		return a.refusal
	}
	return nil
}

func TestReplyWritesEachShapeInBothProtocols(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		write        func(r *Reply)
		resp2, resp3 string
	}{
		{"status", func(r *Reply) { r.Status("OK") }, "+OK\r\n", "+OK\r\n"},
		{"error", func(r *Reply) { r.Error(errors.New("ERR no\r\nway")) }, "-ERR no  way\r\n", "-ERR no  way\r\n"},
		{"int", func(r *Reply) { r.Int(-7) }, ":-7\r\n", ":-7\r\n"},
		{"bulk", func(r *Reply) { r.Bulk("a\r\nb") }, "$4\r\na\r\nb\r\n", "$4\r\na\r\nb\r\n"},
		{"null", func(r *Reply) { r.Null() }, "$-1\r\n", "_\r\n"},
		{"null array", func(r *Reply) { r.NullArray() }, "*-1\r\n", "_\r\n"},
		{"bool", func(r *Reply) { r.Bool(true).Bool(false) }, ":1\r\n:0\r\n", "#t\r\n#f\r\n"},
		{"double", func(r *Reply) { r.Double("2.5") }, "$3\r\n2.5\r\n", ",2.5\r\n"},
		{"verbatim", func(r *Reply) { r.Verbatim("hi") }, "$2\r\nhi\r\n", "=6\r\ntxt:hi\r\n"},
		{"array", func(r *Reply) { r.Array(2).Bulk("a").Int(1) }, "*2\r\n$1\r\na\r\n:1\r\n", "*2\r\n$1\r\na\r\n:1\r\n"},
		{"set", func(r *Reply) { r.Set(1).Bulk("a") }, "*1\r\n$1\r\na\r\n", "~1\r\n$1\r\na\r\n"},
		{"map", func(r *Reply) { r.Map(1).Bulk("f").Bulk("v") }, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n", "%1\r\n$1\r\nf\r\n$1\r\nv\r\n"},
		{"pair", func(r *Reply) { r.Array(1).Pair().Bulk("m").Double("1") }, "*1\r\n$1\r\nm\r\n$1\r\n1\r\n", "*1\r\n*2\r\n$1\r\nm\r\n,1\r\n"},
		{"raw", func(r *Reply) { r.Raw([]byte("+PONG\r\n")) }, "+PONG\r\n", "+PONG\r\n"},
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

func TestReplyAdmitsBeforeItAllocates(t *testing.T) {
	t.Parallel()
	a := &admission{limit: 100, refusal: []byte("-ERR no room\r\n")}
	r := &Reply{Admission: a}
	value := strings.Repeat("v", 50)
	r.Bulk(value)
	require.Equal(t, []int{len(value) + 7}, a.asked, "the exact encoded size, before the copy")
	require.Equal(t, len(value)+7, cap(r.Bytes()), "allocated once, to the admitted size")
	require.False(t, r.Refused())

	r.Reset(Framing{})
	r.Int(1).Bulk(strings.Repeat("v", 200))
	require.True(t, r.Refused())
	require.Equal(t, "-ERR no room\r\n", string(r.Bytes()), "the refusal replaces what was written")
	require.Equal(t, []int{len(value) + 7, 4 + 200 + 8}, a.asked, "asked for the whole reply so far")
	r.Status("OK").Int(2).Null().Array(1).Raw([]byte("x"))
	require.Equal(t, "-ERR no room\r\n", string(r.Bytes()), "nothing after a refusal is written")

	r.Reset(Framing{})
	require.False(t, r.Refused())
	require.Empty(t, r.Bytes())
	require.True(t, r.Reserve(64))
	require.GreaterOrEqual(t, cap(r.Bytes()), 64)
	r.Array(2).Bulk("a").Bulk("b")
	require.Equal(t, "*2\r\n$1\r\na\r\n$1\r\nb\r\n", string(r.Bytes()))
	require.False(t, r.Reserve(101))
	require.True(t, r.Refused())
	require.Equal(t, "-ERR no room\r\n", string(r.Bytes()))
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
