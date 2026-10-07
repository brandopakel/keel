package main

// Throwaway diagnostic, not for merging (branch test/readiness-diag-fixed):
// the experiment of test/readiness-diag, run on the fix. freePort's probe is
// probePort, which holds syscall.ForkLock for reading from socket to close.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var diagStarts, diagListeningAtStart atomic.Int64

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("KEEL_TEST_SERVER") != "1" && os.Getenv("KEEL_DIAG_CHILD") != "1" {
		fmt.Printf("READINESSDIAG summary goos=%s starts=%d listening_right_after_start=%d of_which_the_server_itself=%d\n", runtime.GOOS, diagStarts.Load(), diagListeningAtStart.Load(), diagServerItself.Load())
	}
	os.Exit(code)
}

// diagListening counts the sockets in LISTEN state on port (Linux).
func diagListening(port int) int {
	n := 0
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		body, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			i := strings.LastIndex(f[1], ":")
			if p, err := strconv.ParseUint(f[1][i+1:], 16, 32); err == nil && int(p) == port {
				n++
			}
		}
	}
	return n
}

// diagAfterStart runs right after the server process has started, before it
// can have bound its port: nothing should listen there.
func diagAfterStart(t *testing.T, port, pid int) {
	diagStarts.Add(1)
	if runtime.GOOS != "linux" {
		return
	}
	if inodes := diagListenerInodes(port); len(inodes) > 0 {
		diagListeningAtStart.Add(1)
		for _, ino := range inodes {
			who := "another process"
			if diagOwns(pid, ino) {
				who = "the server itself"
				diagServerItself.Add(1)
			}
			t.Logf("READINESSDIAG listener inode %d on port %d right after server pid %d started: held by %s; holders: %s", ino, port, pid, who, diagHolders(ino))
		}
	}
}

var diagServerItself atomic.Int64

// diagListenerInodes returns the inodes of the sockets in LISTEN on port.
func diagListenerInodes(port int) []uint64 {
	var out []uint64
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		body, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			i := strings.LastIndex(f[1], ":")
			if p, err := strconv.ParseUint(f[1][i+1:], 16, 32); err == nil && int(p) == port {
				ino, _ := strconv.ParseUint(f[9], 10, 64)
				out = append(out, ino)
			}
		}
	}
	return out
}

func diagOwns(pid int, ino uint64) bool {
	target := fmt.Sprintf("socket:[%d]", ino)
	fds, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", pid))
	for _, fd := range fds {
		if link, err := os.Readlink(fd); err == nil && link == target {
			return true
		}
	}
	return false
}

// diagHolders names every process holding the socket with this inode.
func diagHolders(ino uint64) string {
	target := fmt.Sprintf("socket:[%d]", ino)
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	var owners []string
	seen := map[string]bool{}
	for _, fd := range fds {
		if link, err := os.Readlink(fd); err != nil || link != target {
			continue
		}
		pid := strings.Split(fd, "/")[2]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		cmd, _ := os.ReadFile("/proc/" + pid + "/cmdline")
		args := strings.Fields(strings.ReplaceAll(string(cmd), "\x00", " "))
		if len(args) > 3 {
			args = args[:3]
		}
		owners = append(owners, fmt.Sprintf("pid=%s args=%q", pid, strings.Join(args, " ")))
	}
	if len(owners) == 0 {
		return "none found (closed by now)"
	}
	return strings.Join(owners, "; ")
}

func TestDiagChildExit(t *testing.T) {
	if os.Getenv("KEEL_DIAG_CHILD") == "1" {
		os.Exit(0)
	}
}

// TestDiagInheritedProbe: probePort, closed, then a dial right away
// (immediate) or after a cmd.Start (harness), while other goroutines start
// child processes. Nothing should accept either dial.
func TestDiagInheritedProbe(t *testing.T) {
	if os.Getenv("KEEL_READINESS_DIAG") != "1" {
		t.Skip("diagnostic")
	}
	seconds, _ := strconv.Atoi(os.Getenv("KEEL_DIAG_SECONDS"))
	if seconds == 0 {
		seconds = 30
	}
	spawners := 2 * runtime.NumCPU()
	probers := runtime.NumCPU()
	stop := time.Now().Add(time.Duration(seconds) * time.Second)
	child := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDiagChildExit$")
		cmd.Env = append(os.Environ(), "KEEL_DIAG_CHILD=1")
		return cmd
	}
	var spawned atomic.Int64
	var mu sync.Mutex
	counts := map[string]int{}
	errs := map[string]int{}
	var startDurations []time.Duration
	count := func(k string) { mu.Lock(); counts[k]++; mu.Unlock() }
	var wg sync.WaitGroup
	for i := 0; i < spawners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				if child().Run() == nil {
					spawned.Add(1)
				}
			}
		}()
	}
	var next atomic.Int64
	for i := 0; i < probers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			span := int64(testPortCeiling - testPortFloor)
			for n := i; time.Now().Before(stop); n++ {
				port := testPortFloor + int((int64(os.Getpid())*7919+next.Add(1))%span)
				if !probePort(port, nil) {
					continue
				}
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				if n%2 == 0 {
					count("immediate_probes")
					if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
						count("immediate_accepted")
						c.Close()
					}
					continue
				}
				count("harness_probes")
				cmd := child()
				began := time.Now()
				if err := cmd.Start(); err != nil {
					continue
				}
				took := time.Since(began)
				c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
				mu.Lock()
				startDurations = append(startDurations, took)
				switch {
				case err == nil:
					counts["harness_accepted"]++
					c.Close()
				case errors.Is(err, syscall.ECONNREFUSED):
					counts["harness_refused"]++
				default:
					counts["harness_other"]++
					errs[err.Error()[strings.LastIndex(err.Error(), ":")+1:]]++
				}
				mu.Unlock()
				cmd.Wait()
			}
		}(i)
	}
	wg.Wait()
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	sort.Slice(startDurations, func(a, b int) bool { return startDurations[a] < startDurations[b] })
	d := startDurations
	t.Logf("READINESSDIAG inherited-probe (probePort) summary goos=%s goarch=%s cpus=%d seconds=%d spawners=%d probers=%d children_started=%d %s other_errors=%v",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), seconds, spawners, probers, spawned.Load(), strings.Join(parts, " "), errs)
	if len(d) > 0 {
		t.Logf("READINESSDIAG inherited-probe (probePort) child start durations: n=%d min=%s p50=%s p99=%s max=%s", len(d), d[0], d[len(d)/2], d[len(d)*99/100], d[len(d)-1])
	}
}
