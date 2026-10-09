package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/testlock"
	"github.com/stretchr/testify/require"
)

// killValue is what the writer process sets key i to: its number, and up to
// about a kilobyte more, so that records vary in length and a kill can cut one
// anywhere.
func killValue(i int) string { return strconv.Itoa(i) + ":" + strings.Repeat("x", (i*37)%1000) }

// largeKillValue is killValue past the log buffer's bound, so that every
// record is drained to the log as it is encoded, and a failing disk fails it
// in the middle of a record.
func largeKillValue(i int) string { return killValue(i) + strings.Repeat("y", maxAOFTranscriptBytes) }

// killFailAt is the write of the writer's first goroutine after which its
// disk starts failing: later for small values than for large ones, which
// write megabytes each.
func killFailAt(large bool) int {
	if large {
		return 3
	}
	return 300
}

// TestEveryAcknowledgedWriteSurvivesAKill: a process writes through calls on
// an engine Open made, from four goroutines, and reports each write once its
// call has returned. It is killed with SIGKILL, with no Close, at a point
// nobody chose, and the log it leaves - torn or not - is opened again here.
// Every write it reported is there, under every fsync policy: a call returns
// only once its record is written, which a killed process does not undo, and
// under always only once it is synced. One run is killed while a rewrite it
// started is under way; two while the disk fails, one of them in the middle
// of large records, whose rest is kept for the retry; and one under always
// killed inside a slow sync that has reported calls written behind it, which
// a sync run under the engine's lock could not have: none of those calls may
// have returned, and every write acknowledged before must be there.
func TestEveryAcknowledgedWriteSurvivesAKill(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                           string
		policy                         FsyncPolicy
		rewrite, fail, large, slowSync bool
		acks                           int
	}{
		{"always", FsyncAlways, false, false, false, false, 300},
		{"always, during slow syncs with calls queued behind them", FsyncAlways, false, false, false, true, 100},
		{"everysec", FsyncEverySec, false, false, false, false, 2000},
		{"no", FsyncNever, false, false, false, false, 2000},
		{"everysec, during a rewrite", FsyncEverySec, true, false, false, false, 200},
		{"everysec, during a failed write", FsyncEverySec, false, true, false, false, 100},
		{"everysec, during a failed drain of large records", FsyncEverySec, false, true, true, false, 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.large {
				// Some 100 MiB of log, taken before the log's directory, so
				// that it is released only once those files are gone.
				testlock.HoldDiskHeavy(t)
			}
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "keel.aof")
			writer := exec.Command(os.Args[0], "-test.run=^TestHelperProcessWritesUntilKilled$")
			writer.Env = append(os.Environ(), "KEEL_CORE_KILL_LOG="+path, "KEEL_CORE_KILL_FSYNC="+string(c.policy))
			if c.rewrite {
				writer.Env = append(writer.Env, "KEEL_CORE_KILL_REWRITE=1")
			}
			if c.fail {
				writer.Env = append(writer.Env, "KEEL_CORE_KILL_FAIL=1")
			}
			if c.large {
				writer.Env = append(writer.Env, "KEEL_CORE_KILL_LARGE=1")
			}
			if c.slowSync {
				writer.Env = append(writer.Env, "KEEL_CORE_KILL_SLOW_SYNC=1")
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
			// With a failed write, it comes once c.acks writes have been
			// refused, and only the writes acknowledged before the disk failed
			// are required: one answered OK while it fails is buffered, not
			// written, as Redis's is, and a crash then can lose it.
			acked := map[string]string{}
			// required is what must be in the reopened log: every write
			// acknowledged, or with a failed write, every one acknowledged
			// before the disk failed.
			var required map[string]string
			started, since := !c.rewrite && !c.fail, 0
			// With slow syncs, the kill comes once c.acks writes have been
			// acknowledged and a sync has reported calls written behind it:
			// the kill then lands in that sync, with those calls waiting for
			// the next.
			behindSync := !c.slowSync
			// How many syncs could not take the lock while they ran, and how
			// many could: a sync under the lock never can, and one outside it
			// only fails to while calls hold it, so a run of the first with
			// none of the second is a sync under the lock.
			lockedSyncs, syncs := 0, 0
			lines := bufio.NewScanner(out)
			deadline := time.Now().Add(time.Minute)
			ack := func(fields []string) {
				i, err := strconv.Atoi(fields[2])
				require.NoError(t, err)
				if c.large {
					acked[fields[1]] = largeKillValue(i)
				} else {
					acked[fields[1]] = killValue(i)
				}
			}
			for since < c.acks || !started || !behindSync {
				require.True(t, time.Now().Before(deadline), "the writer reported %d writes in a minute", len(acked))
				require.True(t, lines.Scan(), "the writer stopped: %v", lines.Err())
				fields := strings.Fields(lines.Text())
				switch fields[0] {
				case "ack":
					ack(fields)
					if started && !c.fail {
						since++
					}
				case "rewriting":
					started = true
				case "failing":
					started = true
					required = maps.Clone(acked)
				case "denied":
					since++
				case "syncing":
					n, err := strconv.Atoi(fields[1])
					require.NoError(t, err)
					syncs++
					if since >= c.acks && n > 0 {
						behindSync = true
					}
				case "locked":
					lockedSyncs++
					require.False(t, lockedSyncs >= 20 && syncs == 0, "syncs run under the engine's lock, so no call can write behind one")
				case "rewritten":
					t.Fatal("the rewrite finished before the kill; it is slowed so that it cannot")
				default:
					t.Fatalf("the writer said %q", lines.Text())
				}
			}
			require.NoError(t, writer.Process.Kill())
			// The rest of the pipe is read before Wait, which closes it: read
			// after, a line still in the pipe would be lost, and one the
			// scanner had read only part of would come back as a line of its
			// own. The pipe ends when the dead process's end of it closes.
			for lines.Scan() {
				if fields := strings.Fields(lines.Text()); len(fields) == 3 && fields[0] == "ack" {
					ack(fields)
				}
			}
			_ = writer.Wait()
			killed = true
			if required == nil {
				required = acked
			}

			e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
			for key, want := range required {
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
			t.Logf("%d acknowledged writes, %d of them required, present after the kill; torn tails repaired: %d",
				len(acked), len(required), torn)
		})
	}
}

// TestHelperProcessWritesUntilKilled is TestEveryAcknowledgedWriteSurvivesAKill's
// writer: it opens the log it is given and writes to it from four goroutines,
// reporting "ack <key> <i>" once each call has returned, until it is killed.
// Asked to, it starts a rewrite after its first few hundred writes, reports
// "rewriting", and "rewritten" if the rewrite ends; its rewrite's writes each
// take half a second more, through the engine's own I/O hook, so that the
// rewrite lasts seconds rather than milliseconds. Asked to, its disk starts
// failing after its first goroutine's killFailAt write, reporting "failing",
// and "denied <key>" for each write refused while it fails; and asked to, it
// writes large values (largeKillValue), or syncs slowly. In the test process
// it returns at once.
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
	large := os.Getenv("KEEL_CORE_KILL_LARGE") != ""
	value := killValue
	if large {
		value = largeKillValue
	}
	// Asked to, the disk fails partway through: every write of the log from
	// then on writes half of what it is given and fails.
	var failing atomic.Bool
	if os.Getenv("KEEL_CORE_KILL_FAIL") != "" {
		holding(e, func() {
			write := e.aofWrite
			e.aofWrite = func(f *os.File, b []byte) (int, error) {
				if failing.Load() {
					n, _ := write(f, b[:len(b)/2])
					return n, errors.New("no space left on the test's disk")
				}
				return write(f, b)
			}
		})
	}
	if os.Getenv("KEEL_CORE_KILL_SLOW_SYNC") != "" {
		// Each sync of the log takes up to 20 ms, as a slow disk's might, and
		// says what it found: "syncing <bytes>", how much calls had written
		// behind it by then, read under the engine's lock, which a sync
		// outside the lock can take; or "locked", when it could not take it
		// in those 20 ms - every sync, when syncs run under the lock, as
		// before the change this case is for, and now and then one that
		// calls kept the lock from. The parent kills on a sync with calls
		// written behind it.
		holding(e, func() {
			sync := e.aofSync
			e.aofSync = func(f *os.File) error {
				deadline := time.Now().Add(20 * time.Millisecond)
				var behind uint64
				locked := false
				for time.Now().Before(deadline) {
					if e.mu.TryLock() {
						locked = true
						behind = e.AppendOffset() - e.aof.syncOffset
						e.mu.Unlock()
						if behind > 0 {
							break
						}
					}
					time.Sleep(time.Millisecond)
				}
				if !locked {
					report("locked")
				} else {
					report("syncing %d", behind)
				}
				return sync(f)
			}
		})
	}
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
				reply, err := do(context.Background(), e, "SET", key, value(i))
				switch {
				case errors.Is(err, ErrPersistence) && failing.Load():
					report("denied %s", key)
					time.Sleep(time.Millisecond)
					continue
				case err != nil || reply != "+OK\r\n":
					fmt.Fprintln(os.Stderr, key, reply, err)
					os.Exit(1)
				}
				report("ack %s %d", key, i)
				if w == 0 && i == killFailAt(large) && os.Getenv("KEEL_CORE_KILL_FAIL") != "" {
					failing.Store(true)
					report("failing")
				}
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
