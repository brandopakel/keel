package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmplifiedRepliesKeepConnectionAndPersistenceUsable(t *testing.T) {
	for _, mode := range []string{"off", "barrier", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			var flags []string
			if mode != "off" {
				flags = []string{"-appendonly", "-appendfsync", "always", "-aof-async-append", "-appendfilename", filepath.Join(t.TempDir(), "store.aof")}
				if mode == "concurrent" {
					flags = append(flags, "-aof-concurrent-append")
				}
			}
			s := startTestServer(t, flags...)
			c, r := connectTest(t, s)
			value := strings.Repeat("x", 1<<20)
			for _, command := range [][]string{{"SET", "string", value}, {"HSET", "hash", "field", value}, {"SADD", "set", value}} {
				if got := call(t, c, r, command...); got != "+OK" && got != ":1" {
					t.Fatal(got)
				}
			}
			mget, hmget := []string{"MGET"}, []string{"HMGET", "hash"}
			for i := 0; i < 65; i++ {
				mget = append(mget, "string")
				hmget = append(hmget, "field")
			}
			pipeline := request(mget...) + request(hmget...) + request("SRANDMEMBER", "set", "-65") + request("PING")
			if _, err := io.WriteString(c, pipeline); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if got := reply(t, r); !strings.HasPrefix(got, "-ERR reply exceeds") {
					t.Fatal(got)
				}
			}
			if got := reply(t, r); got != "+PONG" {
				t.Fatal(got)
			}
			if got := call(t, c, r, "GET", "string"); got != value {
				t.Fatal("stored value changed")
			}
			c.Close()
			s.stop(t)
			if mode != "off" {
				s = startTestServer(t, flags...)
				c, r = connectTest(t, s)
				if got := call(t, c, r, "GET", "string"); got != value {
					t.Fatal("restart lost string")
				}
				if got := call(t, c, r, "HGET", "hash", "field"); got != value {
					t.Fatal("restart lost hash")
				}
				if got := call(t, c, r, "SCARD", "set"); got != ":1" {
					t.Fatal("restart changed set")
				}
				c.Close()
				s.stop(t)
			}
		})
	}
}
