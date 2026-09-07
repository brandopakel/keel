package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/constant"
)

func FuzzStringEncodingMatchesCanonicalInteger(f *testing.F) {
	for _, value := range []string{"", "0", "-0", "+1", "007", "-1", "9223372036854775807", "-9223372036854775808", "9223372036854775808", "\x00\xff", strings.Repeat("9", 1000)} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		n, err := strconv.ParseInt(value, 10, 64)
		wantInteger := err == nil && strconv.FormatInt(n, 10) == value
		kind, encoding := deduceTypeString(value)
		if kind != constant.ObjTypeString || (encoding == constant.ObjEncodingInt) != wantInteger {
			t.Fatalf("incorrect integer classification for %q", value)
		}
	})
}

func BenchmarkStringClassification(b *testing.B) {
	for _, size := range []int{64, 1024, 1 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			value := strings.Repeat("x", size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = deduceTypeString(value)
			}
		})
	}
}
