package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseLongDoubleReadsWhatStrtoldReads holds parseLongDouble to the
// grammar Redis's string2ld accepts through strtold, and to its range errors.
func TestParseLongDoubleReadsWhatStrtoldReads(t *testing.T) {
	t.Parallel()
	accepted := map[string]string{
		"0":             "0",
		"-0":            "0",
		"10.5":          "10.5",
		".5":            "0.5",
		"5.":            "5",
		"+1":            "1",
		"1.5e2":         "150",
		"-5.0e3":        "-5000",
		"1E2":           "100",
		"0x10":          "16",
		"0x1e":          "30",
		"0x1p3":         "8",
		"0X.8P1":        "1",
		"0e9999":        "0",
		"0x0p-99999999": "0",
		"1e4000":        "",
		"1e-4900":       "",
	}
	for in, want := range accepted {
		f, ok := parseLongDouble(in)
		require.True(t, ok, "%q is a number strtold reads", in)
		if want != "" {
			require.Equal(t, want, formatLongDoubleHuman(f), "%q", in)
		}
	}
	for _, inf := range []string{"inf", "-inf", "INF", "Infinity", "+infinity"} {
		f, ok := parseLongDouble(inf)
		require.True(t, ok, "%q", inf)
		require.True(t, f.IsInf(), "%q", inf)
	}
	refused := []string{
		"", " 1", "1 ", "x", "nan", "-nan", "NaN", "1_0", "0b1", "0o7", "1e", "1e+", "0x", "0xp1", ".", "-",
		"1.2.3", "1e5000", "-1e5000", "1e-5000", "1e999999999", "-1e999999999", "1e-999999999",
		"0x1p99999", "0x1p-99999", "1\x002",
	}
	for _, in := range refused {
		_, ok := parseLongDouble(in)
		require.False(t, ok, "%q is not a number string2ld accepts", in)
	}
	long := make([]byte, maxLongDoubleChars)
	for i := range long {
		long[i] = '1'
	}
	_, ok := parseLongDouble(string(long))
	require.False(t, ok, "a number of MAX_LONG_DOUBLE_CHARS is refused before it is read")
}

// TestLongDoubleAddsAsX87 holds INCRBYFLOAT's arithmetic and its reply to
// Redis's on Linux x86-64, a 64-bit mantissa printed with %.17Lf: the
// examples of Redis's own INCRBYFLOAT page, which a float64 answers
// differently (10.59999999999999964 for the first).
func TestLongDoubleAddsAsX87(t *testing.T) {
	t.Parallel()
	add := func(a, b string) string {
		x, ok := parseLongDouble(a)
		require.True(t, ok)
		y, ok := parseLongDouble(b)
		require.True(t, ok)
		sum, finite := addLongDouble(x, y)
		require.True(t, finite)
		return formatLongDoubleHuman(sum)
	}
	require.Equal(t, "10.6", add("10.50", "0.1"))
	require.Equal(t, "5.6", add("10.6", "-5"))
	require.Equal(t, "5200", add("5.0e3", "2.0e2"))
	require.Equal(t, "0", add("-0.0", "0"))
	require.Equal(t, "0", add("1e-30", "-1e-30"))
	require.Equal(t, "0", add("-1e-30", "0"), "a negative that prints as -0 is answered as 0")
	require.Equal(t, "3", add("1", "2"))

	x, _ := parseLongDouble("inf")
	y, _ := parseLongDouble("1")
	_, finite := addLongDouble(x, y)
	require.False(t, finite, "inf plus a number")
	big, _ := parseLongDouble("1.1e4932")
	_, finite = addLongDouble(big, big)
	require.False(t, finite, "a sum past LDBL_MAX overflows")
}
