package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// killValue is what the writer process sets key i to: its number, and up to
// about a kilobyte more, so that records vary in length and a kill can cut one
// anywhere.
func killValue(i int) string { return strconv.Itoa(i) + ":" + strings.Repeat("x", (i*37)%1000) }

// TestEveryAcknowledgedWriteSurvivesAKill: a process writes through calls on
// an engine Open made, from four goroutines, and reports each write once its
// call has returned. It is killed with SIGKILL, with no Close, at a point
// nobody chose, and the log it leaves - torn or not - is opened again here.
// Every write it reported is there, under every fsync policy: a call returns
// only once its record is written, which a killed process does not undo, and
// under always only once it is synced. One run is killed while a rewrite it
// started is under way.
func TestEveryAcknowledgedWriteSurvivesAKill(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		policy  FsyncPolicy
		rewrite bool
		acks    int
	}{
		{"always", FsyncAlways, false, 300},
		{"everysec", FsyncEverySec, false, 2000},
		{"no", FsyncNever, false, 2000},
		{"everysec, during a rewrite", FsyncEverySec, true, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "keel.aof")
			writer := exec.Command(os.Args[0], "-test.run=^TestHelperProcessWritesUntilKilled$")
			writer.Env = append(os.Environ(), "KEEL_CORE_KILL_LOG="+path, "KEEL_CORE_KILL_FSYNC="+string(c.policy))
			if c.rewrite {
				writer.Env = append(writer.Env, "KEEL_CORE_KILL_REWRITE=1")
			}
			out, err := writer.StdoutPipe()
			require.NoError(t, err)
			writer.Stderr = os.Stderr
			require.NoError(t, writer.Start())
			killed := false
			t.Cleanup(func() {
				if !killed {
					_ = writer.Process.Kill()
					_ = writer.Wait()
				}
			})

			// Every line the process wrote before it died is read, the ones
			// still in the pipe when it was killed included. With a rewrite,
			// the kill comes c.acks writes after it began, and before it ended.
			acked := map[string]string{}
			rewriting, since := !c.rewrite, 0
			lines := bufio.NewScanner(out)
			deadline := time.Now().Add(time.Minute)
			for since < c.acks || !rewriting {
				require.True(t, time.Now().Before(deadline), "the writer reported %d writes in a minute", len(acked))
				require.True(t, lines.Scan(), "the writer stopped: %v", lines.Err())
				fields := strings.Fields(lines.Text())
				switch fields[0] {
				case "ack":
					i, err := strconv.Atoi(fields[2])
					require.NoError(t, err)
					acked[fields[1]] = killValue(i)
					if rewriting {
						since++
					}
				case "rewriting":
					rewriting = true
				case "rewritten":
					t.Fatal("the rewrite finished before the kill; it is slowed so that it cannot")
				default:
					t.Fatalf("the writer said %q", lines.Text())
				}
			}
			require.NoError(t, writer.Process.Kill())
			_ = writer.Wait()
			killed = true
			for lines.Scan() {
				if fields := strings.Fields(lines.Text()); fields[0] == "ack" {
					i, err := strconv.Atoi(fields[2])
					require.NoError(t, err)
					acked[fields[1]] = killValue(i)
				}
			}

			e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
			for key, want := range acked {
				got := mustDo(t, e, "GET", key)
				if got != fmt.Sprintf("$%d\r\n%s\r\n", len(want), want) {
					t.Fatalf("%s was acknowledged as %q and reopened as %q", key, want, got)
				}
			}
			torn := 0
			for name := range filesIn(t, dir) {
				if strings.HasPrefix(name, ".keel-torn-tail-") {
					torn++
				}
			}
			t.Logf("%d acknowledged writes present after the kill; torn tails repaired: %d", len(acked), torn)
		})
	}
}

// TestHelperProcessWritesUntilKilled is TestEveryAcknowledgedWriteSurvivesAKill's
// writer: it opens the log it is given and writes to it from four goroutines,
// reporting "ack <key> <i>" once each call has returned, until it is killed.
// Asked to, it starts a rewrite after its first few hundred writes, reports
// "rewriting", and "rewritten" if the rewrite ends; its rewrite's writes each
// take half a second more, through the engine's own I/O hook, so that the
// rewrite lasts seconds rather than milliseconds. In the test process it returns at once.
func TestHelperProcessWritesUntilKilled(t *testing.T) {
	path := os.Getenv("KEEL_CORE_KILL_LOG")
	if path == "" {
		return
	}
	e, err := Open(context.Background(), Options{AppendOnly: true, AppendFilename: path,
		Fsync: FsyncPolicy(os.Getenv("KEEL_CORE_KILL_FSYNC"))})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var out sync.Mutex
	report := func(format string, args ...any) {
		out.Lock()
		defer out.Unlock()
		// Unbuffered: the line is in the pipe before the next call starts.
		fmt.Fprintf(os.Stdout, format+"\n", args...)
	}
	rewrite := os.Getenv("KEEL_CORE_KILL_REWRITE") != ""
	if rewrite {
		holding(e, func() {
			write := e.rewriteFileWrite
			e.rewriteFileWrite = func(f *os.File, b []byte) (int, error) {
				time.Sleep(500 * time.Millisecond)
				return write(f, b)
			}
		})
	}
	for w := range 4 {
		go func() {
			for i := 0; ; i++ {
				key := fmt.Sprintf("w%d:%d", w, i)
				if reply, err := do(context.Background(), e, "SET", key, killValue(i)); err != nil || reply != "+OK\r\n" {
					fmt.Fprintln(os.Stderr, key, reply, err)
					os.Exit(1)
				}
				report("ack %s %d", key, i)
				if rewrite && w == 0 && i == 500 {
					if reply, err := do(context.Background(), e, "BGREWRITEAOF"); err != nil || !strings.Contains(reply, "rewriting started") {
						fmt.Fprintln(os.Stderr, "BGREWRITEAOF", reply, err)
						os.Exit(1)
					}
					report("rewriting")
					go func() {
						for active := true; active; {
							time.Sleep(10 * time.Millisecond)
							holding(e, func() { active = e.RewriteActive() })
						}
						report("rewritten")
					}()
				}
			}
		}()
	}
	select {}
}
