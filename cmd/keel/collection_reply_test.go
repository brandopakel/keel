package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/testlock"
)

// TestRejectedCollectionPopsPreservePipelineAndRestart sends counted pops whose
// replies would pass the 64 MiB output limit (core.MaxReplyBytes) to a real
// server. Each must be refused before it removes anything, the pipeline must
// carry on to the PING behind it, and the refusals must leave nothing in the
// log, so two restarts still find every member.
//
// A pop can only exceed the limit if its collection holds more than 64 MiB,
// so each collection keeps 65 members of 1 MiB. The refusals do not depend on
// each other, so each collection type gets its own server and log rather than
// sharing one: that holds each log to about 65 MiB, where the three together
// came to about 195 MiB. Automatic rewriting is off because the log crosses
// its 64 MiB minimum here. A rewrite would write a second file, whose size
// depends on timing because keys written while it runs are emitted again, and
// a rewrite finishing after the pops would replace the history the restarts
// are meant to replay, hiding a refusal that had been logged.
func TestRejectedCollectionPopsPreservePipelineAndRestart(t *testing.T) {
	// Parallel with the other tests, but its subtests run one at a time: each
	// writes and replays a log of large collections, about 65 MiB of files,
	// and side by side they would hold several of those at once.
	t.Parallel()
	const members = 65
	count := fmt.Sprint(members)
	value := strings.Repeat("x", 1<<20)
	member := func(i int) string { return fmt.Sprintf("%03d", i) + value }
	type check struct {
		command []string
		want    string
	}
	collections := []struct {
		kind   string
		add    func(i int) []string
		added  func(i int) string
		pops   [][]string
		checks []check
	}{
		{
			kind:  "list",
			add:   func(i int) []string { return []string{"RPUSH", "list", member(i)} },
			added: func(i int) string { return fmt.Sprintf(":%d", i+1) },
			pops:  [][]string{{"LPOP", "list", count}, {"RPOP", "list", count}},
			checks: []check{
				{[]string{"LLEN", "list"}, ":" + count},
				{[]string{"LINDEX", "list", "0"}, member(0)},
				{[]string{"LINDEX", "list", "-1"}, member(members - 1)},
			},
		},
		{
			kind:  "set",
			add:   func(i int) []string { return []string{"SADD", "set", member(i)} },
			added: func(int) string { return ":1" },
			pops:  [][]string{{"SPOP", "set", count}},
			checks: []check{
				{[]string{"SCARD", "set"}, ":" + count},
				{[]string{"SISMEMBER", "set", member(members - 1)}, ":1"},
			},
		},
		{
			kind:  "zset",
			add:   func(i int) []string { return []string{"ZADD", "zset", fmt.Sprint(i), member(i)} },
			added: func(int) string { return ":1" },
			pops:  [][]string{{"ZPOPMIN", "zset", count}, {"ZPOPMAX", "zset", count}},
			checks: []check{
				{[]string{"ZCARD", "zset"}, ":" + count},
				{[]string{"ZSCORE", "zset", member(0)}, "0"},
				{[]string{"ZSCORE", "zset", member(members - 1)}, fmt.Sprint(members - 1)},
			},
		},
	}
	for _, mode := range []string{"off", "barrier", "concurrent"} {
		for _, col := range collections {
			t.Run(mode+"/"+col.kind, func(t *testing.T) {
				var flags []string
				if mode != "off" {
					// About 65 MiB of logs: held apart from core's largest
					// writer, and taken before the log's directory, so that
					// it is released only once those files are gone.
					testlock.HoldDiskHeavy(t)
					flags = []string{"-appendonly", "-appendfsync", "always", "-aof-async-append",
						"-auto-aof-rewrite-percentage", "0", "-appendfilename", filepath.Join(t.TempDir(), "store.aof")}
					if mode == "concurrent" {
						flags = append(flags, "-aof-concurrent-append")
					}
				}
				s := startTestServer(t, flags...)
				c, r := connectTest(t, s)
				for i := 0; i < members; i++ {
					if got := call(t, c, r, col.add(i)...); got != col.added(i) {
						t.Fatal(got)
					}
				}
				var pipeline strings.Builder
				for _, pop := range col.pops {
					pipeline.WriteString(request(pop...))
				}
				pipeline.WriteString(request("PING"))
				if _, err := io.WriteString(c, pipeline.String()); err != nil {
					t.Fatal(err)
				}
				for _, pop := range col.pops {
					if got := reply(t, r); !strings.HasPrefix(got, "-ERR reply exceeds") {
						t.Fatalf("%s: %.80s", pop[0], got)
					}
				}
				if got := reply(t, r); got != "+PONG" {
					t.Fatal(got)
				}
				verify := func() {
					t.Helper()
					for _, check := range col.checks {
						// Members are 1 MiB; keep failure messages readable.
						if got := call(t, c, r, check.command...); got != check.want {
							t.Fatalf("%s %s: %.80s", check.command[0], check.command[1], got)
						}
					}
				}
				verify()
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				s.stop(t)
				if mode != "off" {
					for i := 0; i < 2; i++ {
						// Replaying the earlier 195 MiB form of this log took longer
						// than the generic five-second startup deadline on Intel CI.
						// Record recovery time and keep an explicit budget here.
						s = startTestServerWithin(t, 30*time.Second, flags...)
						t.Logf("large %s replay %d ready in %s", col.kind, i+1, s.startupElapsed)
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
}
