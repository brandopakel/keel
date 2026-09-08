package core

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReservedParserRejectsBeforeCopyingLargeArguments(t *testing.T) {
	wire := appendCommand(nil, "SET", strings.Repeat("k", 2<<20), strings.Repeat("v", 2<<20))
	runtime.GC()
	var before, after runtime.MemStats
	calls := 0
	runtime.ReadMemStats(&before)
	cmd, used, err := ParseCmdReserved(wire, func(charge int) bool {
		calls++
		return false
	})
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, ErrRequestAllocation)
	require.Nil(t, cmd)
	require.Zero(t, used)
	require.Equal(t, 1, calls)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10))
}

func TestReservedParserIncompleteLargeFrameDoesNotCopyEarlierArguments(t *testing.T) {
	wire := appendCommand(nil, "SET", strings.Repeat("k", 2<<20), strings.Repeat("v", 2<<20))
	wire = wire[:len(wire)-1]
	runtime.GC()
	var before, after runtime.MemStats
	calls := 0
	runtime.ReadMemStats(&before)
	for i := 0; i < 20; i++ {
		cmd, used, err := ParseCmdReserved(wire, func(int) bool { calls++; return true })
		if cmd != nil || used != 0 || !errors.Is(err, ErrIncompleteFrame) {
			t.Fatal("incomplete frame was accepted")
		}
	}
	runtime.ReadMemStats(&after)
	require.Zero(t, calls, "no reservation before a complete command exists")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10))
}

func TestReservedParserOwnsBytesAndChargesLargeFieldLists(t *testing.T) {
	parts := []string{"mset"}
	for i := 0; i < 40; i++ {
		parts = append(parts, strings.Repeat("x", i+1))
	}
	wire := appendCommand(nil, parts...)
	charge := 0
	cmd, used, err := ParseCmdReserved(wire, func(n int) bool { charge = n; return true })
	require.NoError(t, err)
	require.Equal(t, len(wire), used)
	require.Greater(t, charge, len(wire))
	clear(wire)
	require.Equal(t, "MSET", cmd.Cmd)
	require.Equal(t, parts[1:], cmd.Args)
	require.Equal(t, len(cmd.Args), cap(cmd.Args), "no hidden command token or growth slack retained")
}

func TestRequestAllocationSizeRejectsOverflow(t *testing.T) {
	for _, n := range []int{-1, int(^uint(0) >> 1)} {
		_, ok := RequestAllocationSize(n)
		require.False(t, ok)
	}
	size, ok := RequestAllocationSize(0)
	require.True(t, ok)
	require.Zero(t, size)
}

func FuzzReservedCommandParserMatchesValueDecoder(f *testing.F) {
	for _, parts := range [][]string{{"PING"}, {"set", "k", "v"}, {"GET", "\x00\xff"}, {"MSET", "a", "", "b", "v"}} {
		wire := appendCommand(nil, parts...)
		for i := 0; i <= len(wire); i++ {
			f.Add(wire[:i])
		}
	}
	for _, wire := range []string{"*3\r\n+SET\r\n+k\r\n-v\r\n", "*1\r\n$-1\r\n", "*1\r\n:1\r\n", "*1000000\r\n", "*1\r\n*1\r\n$4\r\nPING\r\n", "*2\r\n:1\r\n", strings.Repeat("*1\r\n", 33)} {
		f.Add([]byte(wire))
	}
	f.Fuzz(func(t *testing.T, wire []byte) {
		got, used, err := ParseCmdReserved(wire, func(int) bool { return true })
		want, expectedUsed, expectedErr := parseCmdGeneric(wire)
		if !errors.Is(err, expectedErr) || used != expectedUsed || !reflect.DeepEqual(got, want) {
			t.Fatalf("reserved parser mismatch: got %#v/%d/%v; reference %#v/%d/%v", got, used, err, want, expectedUsed, expectedErr)
		}
	})
}

var parsedAdmissionBenchmarkSink *Command

func BenchmarkRequestParsingAdmission(b *testing.B) {
	small := appendCommand(nil, "SET", "key", strings.Repeat("v", 64))
	partial := appendCommand(nil, "SET", strings.Repeat("k", 1<<20), strings.Repeat("v", 1<<20))
	for _, fixture := range []struct {
		name       string
		wire       []byte
		incomplete bool
	}{{"set64", small, false}, {"incomplete-large-key", partial[:len(partial)-1], true}} {
		for _, parser := range []struct {
			name  string
			parse func([]byte) (*Command, int, error)
		}{{"existing", ParseCmd}, {"admitted", func(wire []byte) (*Command, int, error) {
			return ParseCmdReserved(wire, func(int) bool { return true })
		}}} {
			b.Run(fixture.name+"/"+parser.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					cmd, used, err := parser.parse(fixture.wire)
					if fixture.incomplete {
						if !errors.Is(err, ErrIncompleteFrame) || cmd != nil || used != 0 {
							b.Fatal("incomplete frame accepted")
						}
					} else if err != nil || used != len(fixture.wire) || cmd.Cmd != "SET" || len(cmd.Args) != 2 {
						b.Fatal("complete frame changed")
					}
					parsedAdmissionBenchmarkSink = cmd
				}
			})
		}
	}
}
