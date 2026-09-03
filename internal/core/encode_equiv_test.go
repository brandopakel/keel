package core_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/core"
)

// encodeOld is the fmt.Sprintf-based encoder that Encode replaced, reproduced
// verbatim so the append-based version can be checked against it.
//
// The rewrite exists to stop copying every payload twice, which is invisible in
// the output and therefore exactly the kind of change that silently corrupts a
// reply. Comparing byte for byte against the original is the only way to be
// sure the wire format did not move.
func encodeOld(value interface{}, isSimpleString bool) []byte {
	switch v := value.(type) {
	case string:
		if isSimpleString {
			return []byte(fmt.Sprintf("+%s%s", v, core.CRLF))
		}
		return []byte(fmt.Sprintf("$%d%s%s%s", len(v), core.CRLF, v, core.CRLF))
	case int64, int32, int16, int8, int:
		return []byte(fmt.Sprintf(":%d\r\n", v))
	case error:
		return []byte(fmt.Sprintf("-%s\r\n", v))
	case []string:
		return encodeStringArrayOld(value.([]string))
	case [][]string:
		var b []byte
		buf := bytes.NewBuffer(b)
		for _, sa := range value.([][]string) {
			buf.Write(encodeStringArrayOld(sa))
		}
		return []byte(fmt.Sprintf("*%d\r\n%s", len(value.([][]string)), buf.Bytes()))
	case []interface{}:
		var b []byte
		buf := bytes.NewBuffer(b)
		for _, x := range value.([]interface{}) {
			buf.Write(encodeOld(x, false))
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

func encodeStringArrayOld(sa []string) []byte {
	var b []byte
	buf := bytes.NewBuffer(b)
	for _, s := range sa {
		buf.Write([]byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)))
	}
	return []byte(fmt.Sprintf("*%d\r\n%s", len(sa), buf.Bytes()))
}

func TestEncodeMatchesPreviousImplementation(t *testing.T) {
	cases := []struct {
		name   string
		value  interface{}
		simple bool
	}{
		{"empty bulk string", "", false},
		{"short bulk string", "hello", false},
		{"simple string", "OK", true},
		{"empty simple string", "", true},
		{"string with CRLF inside", "a\r\nb", false},
		{"binary payload", string([]byte{0, 1, 2, 255, 254}), false},
		{"1MB payload", strings.Repeat("x", 1<<20), false},
		{"utf8 payload", "héllo wörld ✓", false},
		{"int", 42, false},
		{"negative int", -7, false},
		{"zero int", 0, false},
		{"int64 max", int64(9223372036854775807), false},
		{"int64 min", int64(-9223372036854775808), false},
		{"int32", int32(-5), false},
		{"int16", int16(300), false},
		{"int8", int8(-128), false},
		{"error", errors.New("ERR something went wrong"), false},
		{"empty string slice", []string{}, false},
		{"string slice", []string{"a", "bb", "ccc"}, false},
		{"string slice with empties", []string{"", "x", ""}, false},
		{"nested string slices", [][]string{{"a"}, {"b", "c"}}, false},
		{"empty nested", [][]string{}, false},
		{"interface slice", []interface{}{"a", 1, errors.New("e")}, false},
		{"empty interface slice", []interface{}{}, false},
		{"int slice", []int{1, 2, 3}, false},
		{"empty int slice", []int{}, false},
		{"int slice negatives", []int{-1, 0, 99}, false},
		{"unsupported type falls back to nil", 3.14, false},
		{"nil value", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := encodeOld(c.value, c.simple)
			got := core.Encode(c.value, c.simple)
			assert.True(t, bytes.Equal(want, got),
				"encoding differs\n old: %q\n new: %q", truncate(want), truncate(got))
		})
	}
}

func truncate(b []byte) string {
	if len(b) > 120 {
		return string(b[:60]) + "..." + string(b[len(b)-30:])
	}
	return string(b)
}

// The benchmarks below run the old and new encoders side by side. -benchmem is
// the point: the win is allocated bytes per operation, which is invisible in
// wall-clock at small sizes and dominant at large ones.
func benchEncode(b *testing.B, size int, old bool) {
	b.Helper()
	v := strings.Repeat("x", size)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if old {
			_ = encodeOld(v, false)
		} else {
			_ = core.Encode(v, false)
		}
	}
}

func BenchmarkEncodeOld64B(b *testing.B)   { benchEncode(b, 64, true) }
func BenchmarkEncodeNew64B(b *testing.B)   { benchEncode(b, 64, false) }
func BenchmarkEncodeOld4KB(b *testing.B)   { benchEncode(b, 4096, true) }
func BenchmarkEncodeNew4KB(b *testing.B)   { benchEncode(b, 4096, false) }
func BenchmarkEncodeOld256KB(b *testing.B) { benchEncode(b, 262144, true) }
func BenchmarkEncodeNew256KB(b *testing.B) { benchEncode(b, 262144, false) }
func BenchmarkEncodeOld1MB(b *testing.B)   { benchEncode(b, 1<<20, true) }
func BenchmarkEncodeNew1MB(b *testing.B)   { benchEncode(b, 1<<20, false) }
