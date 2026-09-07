package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func FuzzCommandParserMatchesValueDecoder(f *testing.F) {
	for _, parts := range [][]string{{"PING"}, {"SET", "k", "v"}, {"GET", "\x00\xff"}, {"MSET", "a", "", "b", "v"}} {
		wire := appendCommand(nil, parts...)
		for i := 0; i <= len(wire); i++ {
			f.Add(wire[:i])
		}
	}
	for _, wire := range []string{"*3\r\n+SET\r\n+k\r\n+v\r\n", "*1\r\n$-1\r\n", "*1\r\n:1\r\n", "*1000000\r\n", "*1\r\n*1\r\n$4\r\nPING\r\n"} {
		f.Add([]byte(wire))
	}
	f.Fuzz(func(t *testing.T, wire []byte) {
		got, used, err := ParseCmd(wire)
		want, expectedUsed, expectedErr := parseCmdGeneric(wire)
		if !errors.Is(err, expectedErr) || used != expectedUsed || !reflect.DeepEqual(got, want) {
			t.Fatalf("parser mismatch: got %#v/%d/%v; reference %#v/%d/%v", got, used, err, want, expectedUsed, expectedErr)
		}
	})
}

func TestParsedCommandOwnsInputBytes(t *testing.T) {
	wire := appendCommand(nil, "SET", "small-key", strings.Repeat("x", 1<<20))
	command, _, err := ParseCmd(wire)
	if err != nil {
		t.Fatal(err)
	}
	clear(wire)
	if command.Cmd != "SET" || command.Args[0] != "small-key" || command.Args[1] != strings.Repeat("x", 1<<20) {
		t.Fatal("parsed command retained mutable socket-buffer storage")
	}
}
