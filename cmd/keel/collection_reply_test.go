package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectedCollectionPopsPreservePipelineAndRestart(t *testing.T) {
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
			for i := 0; i < 65; i++ {
				member := fmt.Sprintf("%03d", i) + value
				if got := call(t, c, r, "RPUSH", "list", member); got != fmt.Sprintf(":%d", i+1) {
					t.Fatal(got)
				}
				if got := call(t, c, r, "SADD", "set", member); got != ":1" {
					t.Fatal(got)
				}
				if got := call(t, c, r, "ZADD", "zset", fmt.Sprint(i), member); got != ":1" {
					t.Fatal(got)
				}
			}
			pipeline := request("LPOP", "list", "65") + request("RPOP", "list", "65") + request("SPOP", "set", "65") + request("ZPOPMIN", "zset", "65") + request("ZPOPMAX", "zset", "65") + request("PING")
			if _, err := io.WriteString(c, pipeline); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				if got := reply(t, r); !strings.HasPrefix(got, "-ERR reply exceeds") {
					t.Fatal(got)
				}
			}
			if got := reply(t, r); got != "+PONG" {
				t.Fatal(got)
			}
			verify := func() {
				t.Helper()
				for _, cmd := range [][]string{{"LLEN", "list"}, {"SCARD", "set"}, {"ZCARD", "zset"}} {
					if got := call(t, c, r, cmd...); got != ":65" {
						t.Fatalf("%v: %s", cmd, got)
					}
				}
				if got := call(t, c, r, "LINDEX", "list", "0"); got != "000"+value {
					t.Fatal("list head changed")
				}
				if got := call(t, c, r, "ZSCORE", "zset", "064"+value); got != "64" {
					t.Fatal("sorted member changed")
				}
			}
			verify()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			s.stop(t)
			if mode != "off" {
				for i := 0; i < 2; i++ {
					s = startTestServer(t, flags...)
					c, r = connectTest(t, s)
					verify()
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
					s.stop(t)
				}
			}
		})
	}
}
