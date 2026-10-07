package main

// Throwaway diagnostic, not for merging (branch test/readiness-diag).
//
// Question: what does a test server's readiness check reach, when the next
// dial is refused or reset? Three pieces of evidence:
//
//  1. In situ: freePort records the inode of its probe listener; right after
//     the server process starts, and again when the readiness dial succeeds,
//     /proc/net/tcp says which listening socket is on the port and /proc says
//     who holds it (Linux).
//  2. TestDiagInheritedProbe: freePort's probe-then-close, with other
//     goroutines starting child processes as parallel tests do; counts how
//     often a closed probe still accepts a connection, and names its holder.
//  3. TestDiagBacklogOverflow: what a dial sees when a listener's backlog is
//     full (refused, or a dropped SYN).

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var (
	diagProbeInodes  sync.Map // port -> probe listener inode (Linux)
	diagStarts       atomic.Int64
	diagStaleAtStart atomic.Int64
	diagReadiness    atomic.Int64
	diagReachedOwn   atomic.Int64
	diagReachedProbe atomic.Int64
	diagReachedGone  atomic.Int64
	diagReachedOther atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("KEEL_TEST_SERVER") != "1" && os.Getenv("KEEL_DIAG_CHILD") != "1" {
		fmt.Printf("READINESSDIAG summary goos=%s starts=%d stale_probe_listening_after_start=%d readiness=%d reached_own_server=%d reached_probe=%d reached_listener_already_gone=%d reached_other=%d\n",
			runtime.GOOS, diagStarts.Load(), diagStaleAtStart.Load(), diagReadiness.Load(), diagReachedOwn.Load(), diagReachedProbe.Load(), diagReachedGone.Load(), diagReachedOther.Load())
	}
	os.Exit(code)
}

// diagInode returns the inode of a listener's socket.
func diagInode(l net.Listener) uint64 {
	raw, err := l.(*net.TCPListener).SyscallConn()
	if err != nil {
		return 0
	}
	var ino uint64
	raw.Control(func(fd uintptr) {
		var st syscall.Stat_t
		if syscall.Fstat(int(fd), &st) == nil {
			ino = uint64(st.Ino)
		}
	})
	return ino
}

func diagRecordProbe(port int, l net.Listener) {
	if runtime.GOOS == "linux" {
		diagProbeInodes.Store(port, diagInode(l))
	}
}

// diagListeners returns the inodes of the sockets in LISTEN state on port.
func diagListeners(port int) []uint64 {
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
			p, err := strconv.ParseUint(f[1][i+1:], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			ino, _ := strconv.ParseUint(f[9], 10, 64)
			out = append(out, ino)
		}
	}
	return out
}

// diagHolders names every process holding the socket with this inode: pid,
// parent, state and command line.
func diagHolders(ino uint64) string {
	target := fmt.Sprintf("socket:[%d]", ino)
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	var owners []string
	seen := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(fd)
		if err != nil || link != target {
			continue
		}
		pid := strings.Split(fd, "/")[2]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		stat, _ := os.ReadFile("/proc/" + pid + "/stat")
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		state, ppid := "?", "?"
		if len(fields) > 1 {
			state, ppid = fields[0], fields[1]
		}
		cmd, _ := os.ReadFile("/proc/" + pid + "/cmdline")
		exe, _ := os.Readlink("/proc/" + pid + "/exe")
		args := strings.Fields(strings.ReplaceAll(string(cmd), "\x00", " "))
		if len(args) > 4 {
			args = args[:4]
		}
		owners = append(owners, fmt.Sprintf("pid=%s ppid=%s state=%s exe=%s args=%q", pid, ppid, state, filepath.Base(exe), strings.Join(args, " ")))
	}
	if len(owners) == 0 {
		return "no holder found (closed by now)"
	}
	return strings.Join(owners, "; ")
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

func diagProbe(port int) uint64 {
	v, ok := diagProbeInodes.Load(port)
	if !ok {
		return 0
	}
	return v.(uint64)
}

// diagAfterStart runs right after the server process has started, which is
// long before it can have bound its port.
func diagAfterStart(t *testing.T, port, pid int) {
	diagStarts.Add(1)
	if runtime.GOOS != "linux" {
		return
	}
	probe := diagProbe(port)
	for _, ino := range diagListeners(port) {
		if ino == probe {
			diagStaleAtStart.Add(1)
			t.Logf("READINESSDIAG stale probe: port %d's probe listener (inode %d) still listens after server pid %d started; holders: %s", port, ino, pid, diagHolders(ino))
		} else {
			t.Logf("READINESSDIAG at start: port %d has listener inode %d (probe %d), server pid %d; holders: %s", port, ino, probe, pid, diagHolders(ino))
		}
	}
}

// diagAtReadiness runs when the readiness dial has succeeded.
func diagAtReadiness(t *testing.T, port, pid int, c net.Conn) {
	diagReadiness.Add(1)
	if runtime.GOOS != "linux" {
		return
	}
	probe := diagProbe(port)
	inodes := diagListeners(port)
	switch {
	case len(inodes) == 0:
		diagReachedGone.Add(1)
		t.Logf("READINESSDIAG readiness on port %d (local %s) succeeded, but nothing listens there now; server pid %d, probe inode %d", port, c.LocalAddr(), pid, probe)
	case len(inodes) == 1 && inodes[0] == probe:
		diagReachedProbe.Add(1)
		t.Logf("READINESSDIAG readiness on port %d (local %s) reached freePort's probe (inode %d), not server pid %d; holders: %s", port, c.LocalAddr(), probe, pid, diagHolders(probe))
	case len(inodes) == 1 && diagOwns(pid, inodes[0]):
		diagReachedOwn.Add(1)
	default:
		diagReachedOther.Add(1)
		for _, ino := range inodes {
			t.Logf("READINESSDIAG readiness on port %d (local %s): listener inode %d (probe %d), server pid %d; holders: %s", port, c.LocalAddr(), ino, probe, pid, diagHolders(ino))
		}
	}
}

// TestDiagChildExit is the child that TestDiagInheritedProbe starts: it
// exits at once.
func TestDiagChildExit(t *testing.T) {
	if os.Getenv("KEEL_DIAG_CHILD") == "1" {
		os.Exit(0)
	}
}

// TestDiagInheritedProbe does what freePort and launchTestServer do -
// listen on a port, close it, start a child process, dial the port - while
// other goroutines start child processes the way parallel tests start their
// servers. Nothing should be listening at that dial; it counts how often
// something is, and on Linux names the holder of that listener.
func TestDiagInheritedProbe(t *testing.T) {
	if os.Getenv("KEEL_READINESS_DIAG") != "1" {
		t.Skip("diagnostic")
	}
	seconds, _ := strconv.Atoi(os.Getenv("KEEL_DIAG_SECONDS"))
	if seconds == 0 {
		seconds = 30
	}
	spawners, _ := strconv.Atoi(os.Getenv("KEEL_DIAG_SPAWNERS"))
	if spawners == 0 {
		spawners = 2 * runtime.NumCPU()
	}
	probers := runtime.NumCPU()
	stop := time.Now().Add(time.Duration(seconds) * time.Second)
	child := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDiagChildExit$")
		cmd.Env = append(os.Environ(), "KEEL_DIAG_CHILD=1")
		return cmd
	}
	var spawned, probes, immediate, afterStart, refused, other atomic.Int64
	var mu sync.Mutex
	var hits []string
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
			for time.Now().Before(stop) {
				port := testPortFloor + int((int64(os.Getpid())*7919+next.Add(1))%span)
				l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					continue
				}
				ino := uint64(0)
				if runtime.GOOS == "linux" {
					ino = diagInode(l)
				}
				l.Close()
				probes.Add(1)
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				record := func(when string, c net.Conn) {
					holders := "n/a"
					if runtime.GOOS == "linux" {
						holders = diagHolders(ino)
					} else if out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN").CombinedOutput(); err == nil || len(out) > 0 {
						holders = strings.TrimSpace(string(out))
					}
					mu.Lock()
					if len(hits) < 40 {
						hits = append(hits, fmt.Sprintf("%s: port %d (probe inode %d, local %s) accepted after Close; holders: %s", when, port, ino, c.LocalAddr(), holders))
					}
					mu.Unlock()
				}
				// The dial right after Close.
				if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
					immediate.Add(1)
					record("immediately", c)
					c.Close()
					continue
				}
				// The harness's order: start a process, then dial.
				cmd := child()
				if err := cmd.Start(); err != nil {
					continue
				}
				c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
				switch {
				case err == nil:
					afterStart.Add(1)
					record("after a child started", c)
					c.Close()
				case errors.Is(err, syscall.ECONNREFUSED):
					refused.Add(1)
				default:
					other.Add(1)
				}
				cmd.Wait()
			}
		}(i)
	}
	wg.Wait()
	for _, h := range hits {
		t.Logf("READINESSDIAG inherited-probe hit: %s", h)
	}
	t.Logf("READINESSDIAG inherited-probe summary goos=%s goarch=%s cpus=%d seconds=%d spawners=%d probers=%d children_started=%d probes=%d accepted_immediately_after_close=%d accepted_after_child_start=%d refused_after_child_start=%d other=%d",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), seconds, spawners, probers, spawned.Load(), probes.Load(), immediate.Load(), afterStart.Load(), refused.Load(), other.Load())
}

// TestDiagBacklogOverflow listens with a small backlog, never accepts, and
// dials more connections than the backlog holds: is an overflowing dial
// refused, or does its SYN go unanswered?
func TestDiagBacklogOverflow(t *testing.T) {
	if os.Getenv("KEEL_READINESS_DIAG") != "1" {
		t.Skip("diagnostic")
	}
	for _, name := range []string{"/proc/sys/net/core/somaxconn", "/proc/sys/net/ipv4/tcp_abort_on_overflow", "/proc/sys/net/ipv4/tcp_syncookies", "/proc/sys/net/ipv4/ip_local_port_range"} {
		if b, err := os.ReadFile(name); err == nil {
			t.Logf("READINESSDIAG %s = %s", name, strings.TrimSpace(string(b)))
		}
	}
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("sysctl", "kern.ipc.somaxconn", "net.inet.ip.portrange.first").CombinedOutput()
		t.Logf("READINESSDIAG %s", strings.TrimSpace(string(out)))
	}
	for _, backlog := range []int{1, 2} {
		for _, closeEach := range []bool{false, true} {
			port := freePort(t)
			fd, err := listenRaw(port, backlog)
			if err != nil {
				t.Fatal(err)
			}
			var results []string
			var conns []net.Conn
			for i := 0; i < 8; i++ {
				start := time.Now()
				c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1500*time.Millisecond)
				el := time.Since(start).Round(time.Millisecond)
				switch {
				case err == nil:
					results = append(results, fmt.Sprintf("%d:ok(%s)", i+1, el))
					if closeEach {
						c.Close()
					} else {
						conns = append(conns, c)
					}
				case errors.Is(err, syscall.ECONNREFUSED):
					results = append(results, fmt.Sprintf("%d:refused(%s)", i+1, el))
				default:
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						results = append(results, fmt.Sprintf("%d:timeout(%s)", i+1, el))
					} else {
						results = append(results, fmt.Sprintf("%d:%v(%s)", i+1, err, el))
					}
				}
			}
			for _, c := range conns {
				c.Close()
			}
			syscall.Close(fd)
			t.Logf("READINESSDIAG backlog goos=%s backlog=%d client_closes_each=%v, never accepted: %s", runtime.GOOS, backlog, closeEach, strings.Join(results, " "))
		}
	}
}

// listenRaw listens as the server's listenTCP does: a raw socket with
// SO_REUSEADDR, bound to 127.0.0.1:port, with this backlog.
func listenRaw(port, backlog int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return -1, err
	}
	if err := syscall.Listen(fd, backlog); err != nil {
		return -1, err
	}
	return fd, nil
}
