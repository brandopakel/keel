package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

func TestServerProcess(t *testing.T) {
	// Not parallel: it is the entry point of a test server's own process,
	// which runs it alone, and in the test process it returns at once.
	if os.Getenv("KEEL_TEST_SERVER") != "1" {
		return
	}
	for i, s := range os.Args {
		if s == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	if limit := os.Getenv("KEEL_TEST_FILE_LIMIT"); limit != "" {
		n, err := strconv.ParseUint(limit, 10, 64)
		if err != nil {
			panic(err)
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: n}); err != nil {
			panic(err)
		}
	}
	flag.CommandLine = flag.NewFlagSet("keel", flag.ExitOnError)
	main()
	os.Exit(0)
}

type testServer struct {
	cmd            *exec.Cmd
	addr           string
	log            bytes.Buffer
	stopped        bool
	startupElapsed time.Duration
}

func startTestServer(t *testing.T, args ...string) *testServer {
	t.Helper()
	return startTestServerWithin(t, 5*time.Second, args...)
}

// startTestServerWithin lets large replay fixtures specify their recovery budget
// while ordinary startups and all command deadlines keep their existing limits.
func startTestServerWithin(t *testing.T, startupTimeout time.Duration, args ...string) *testServer {
	t.Helper()
	return launchTestServer(t, startupTimeout, nil, args...)
}

// startLimitedTestServer starts a server whose process may write no file
// larger than fileLimit bytes (TestServerProcess sets RLIMIT_FSIZE), so that
// a test can fail its disk. The limit is in that server's environment alone:
// t.Setenv would set it for the test process, which every test running beside
// it shares, and Go refuses t.Setenv in a parallel test.
func startLimitedTestServer(t *testing.T, fileLimit string, args ...string) *testServer {
	t.Helper()
	return launchTestServer(t, 5*time.Second, []string{"KEEL_TEST_FILE_LIMIT=" + fileLimit}, args...)
}

// launchTestServer starts the server as a process of its own, with env added
// to its environment, and waits for it to listen.
func launchTestServer(t *testing.T, startupTimeout time.Duration, env []string, args ...string) *testServer {
	t.Helper()
	port := freePort(t)
	s := &testServer{addr: fmt.Sprintf("127.0.0.1:%d", port)}
	argv := append([]string{"-test.run=^TestServerProcess$", "--", "-host", "127.0.0.1", "-port", strconv.Itoa(port)}, args...)
	s.cmd = exec.Command(os.Args[0], argv...)
	s.cmd.Env = append(append(os.Environ(), "KEEL_TEST_SERVER=1", "KEEL_TEST_PASSWORD=integration-secret"), env...)
	s.cmd.Stdout = &s.log
	s.cmd.Stderr = &s.log
	started := time.Now()
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.stopped {
			if t.Failed() {
				s.captureFailure(t)
			} else {
				_ = s.cmd.Process.Kill()
				_ = s.cmd.Wait()
			}
			s.stopped = true
		}
	})
	deadline := started.Add(startupTimeout)
	var last error
	for time.Now().Before(deadline) {
		if last = answersPing(s.addr, deadline); last == nil {
			s.startupElapsed = time.Since(started)
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not answer within %s (elapsed %s): %v", startupTimeout, time.Since(started), last)
	return nil
}

// refusedTestServer runs a server that must refuse to start, as a process of
// its own on a port of its own, and returns what it printed. It fails the test
// if the server answers, or does not exit with status 1 within a few seconds.
func refusedTestServer(t *testing.T, args ...string) string {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	argv := append([]string{"-test.run=^TestServerProcess$", "--", "-host", "127.0.0.1", "-port", strconv.Itoa(port)}, args...)
	cmd := exec.Command(os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "KEEL_TEST_SERVER=1", "KEEL_TEST_PASSWORD=integration-secret")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("a refused start should exit with status 1, not %v:\n%s", err, log.String())
		}
	case <-time.After(5 * time.Second):
		answered := answersPing(addr, time.Now().Add(time.Second)) == nil
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the server started (answering: %t) where it should have refused:\n%s", answered, log.String())
	}
	return log.String()
}

// answersPing reports whether the server at addr answers a PING on a new
// connection, by deadline: +PONG, or -NOAUTH when it has a password.
//
// A connection alone does not show that the server is up. The kernel completes
// a handshake for any listening socket, and a copy of freePort's probe can
// still be listening after freePort has closed it (see probePort). The copy
// never accepts, and when it closes the kernel resets what is queued on it.
// A readiness check that only dialled could reach the copy: the test's next
// dial was then refused, because the server had not bound the port yet
// (TestServerStartsWithItsListenerAndTransportFlags, run 37583994573), or its
// first read on a connection queued on the copy was reset
// (TestRESP2WireBytesUnchanged, run 37584062962). Only the server answers.
func answersPing(addr string, deadline time.Time) error {
	c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.WriteString(c, request("PING")); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if line != "+PONG\r\n" && !strings.HasPrefix(line, "-NOAUTH ") {
		return fmt.Errorf("PING answered %q", line)
	}
	return nil
}

// TestReadinessNeedsTheServersAnswer: a socket that completes handshakes and
// never answers - a forked child's copy of freePort's probe, or anything else
// on the port - is not a server that is ready, and the server is.
func TestReadinessNeedsTheServersAnswer(t *testing.T) {
	t.Parallel()
	silent, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if c, err := net.DialTimeout("tcp", silent.Addr().String(), time.Second); err != nil {
		t.Fatalf("the kernel did not complete a handshake for a listener that never accepts: %v", err)
	} else {
		c.Close()
	}
	if err := answersPing(silent.Addr().String(), time.Now().Add(200*time.Millisecond)); err == nil {
		t.Fatal("a listener that never answered counted as a ready server")
	}
	// The servers of every other test answer +PONG; one with a password
	// answers -NOAUTH, and is as ready.
	s := startTestServer(t, "-requirepass-env", "KEEL_TEST_PASSWORD")
	if err := answersPing(s.addr, time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("a server with a password: %v", err)
	}
	s.stop(t)
}

// TestPortProbeShutsOutForks: no process can be forked while freePort's probe
// listens, so no child has a copy of its socket to keep listening after
// freePort closes it; and a port another listener holds is not free.
//
// Not parallel: a test starting a server holds ForkLock while it forks, and
// the TryLock below would then fail for that rather than for the probe.
func TestPortProbeShutsOutForks(t *testing.T) {
	port := freePort(t)
	couldFork := false
	if !probePort(port, func() {
		if couldFork = syscall.ForkLock.TryLock(); couldFork {
			syscall.ForkLock.Unlock()
		}
	}) {
		t.Fatalf("port %d, free a moment ago, could not be probed", port)
	}
	if couldFork {
		t.Fatal("a fork could start while the port probe listened, and its child would keep a copy of the probe")
	}
	held, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if probePort(port, nil) {
		t.Fatalf("the probe took port %d while another listener held it", port)
	}
}

// Test servers listen on ports from [testPortFloor, testPortCeiling), which is
// below the range the kernel hands out by itself (Linux's
// ip_local_port_range starts at 32768 by default, macOS's at 49152).
const (
	testPortFloor   = 20000
	testPortCeiling = 32000
)

// nextTestPort counts the ports freePort has handed out in this process.
// freePort starts from an offset taken from the process id, so that two test
// processes at once rarely meet.
var nextTestPort atomic.Int64

// freePort returns a free port that no other test server in the process has
// had, and that nothing else can be given before the server binds it.
//
// A port the kernel picks for a listener on :0, closed so that the server can
// bind it, was free only for that moment. The kernel hands just-closed ports
// out again, to the next listener on :0 and to outgoing connections, in this
// process or another. Tests run side by side, and a race-instrumented server
// takes a while to start, so another test's probe would take the port first,
// several times a run (a CI diagnostic counted 7 to 14 in each three-pass race
// run). Then a test's readiness check reached that probe instead of its server.
// The next dial was refused, or a connection the probe had queued was reset.
//
// Ports below the kernel's own range cannot be handed out that way. Within the
// process each is tried once, in turn, and kept only if a listener can bind it
// (probePort), which skips any that something else holds.
func freePort(t *testing.T) int {
	t.Helper()
	requireTestPortsBelowEphemeral(t)
	span := int64(testPortCeiling - testPortFloor)
	offset := int64(os.Getpid()) * 7919 % span
	for tries := int64(0); tries < span; tries++ {
		port := testPortFloor + int((offset+nextTestPort.Add(1)-1)%span)
		if probePort(port, nil) {
			return port
		}
	}
	t.Fatalf("no free port in [%d, %d) for a test server", testPortFloor, testPortCeiling)
	return 0
}

// probePort reports whether a listener can take 127.0.0.1:port, by listening
// on it and closing it again. during, when not nil, runs while it listens.
//
// No process may be forked while the probe exists, so it holds
// syscall.ForkLock for reading throughout, as the lock's documentation asks of
// code that creates descriptors; every fork in the process takes it for
// writing. A child forked while the probe listened would have a copy of the
// socket until its exec closed it. Close-on-exec closes it in the child only
// at exec, so the copy outlived freePort's Close, and stayed in LISTEN, on the
// port this test's server was about to bind. Tests start servers side by side,
// so forks are frequent. On CI (throwaway branch test/readiness-diag), 1 to 3
// in every hundred probes were still listening right after Close, held by a
// child of the test process that had not finished its exec, and on Linux about
// 3 in a thousand still accepted a dial made after a cmd.Start, as
// launchTestServer's readiness check is.
//
// The socket is made with syscall calls rather than net.Listen. On macOS
// net.Listen takes ForkLock for reading itself, and taking it again while a
// fork waits to take it for writing would deadlock.
func probePort(port int, during func()) bool {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	// As the server does (listenTCP), so that a port it could bind is not
	// refused for a closed connection's TIME_WAIT.
	if syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) != nil ||
		syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}) != nil ||
		syscall.Listen(fd, 1) != nil {
		return false
	}
	if during != nil {
		during()
	}
	return true
}

// requireTestPortsBelowEphemeral fails t when Linux is set to hand out ports
// at or below testPortCeiling, which would let the kernel take a test server's
// port again; the check freePort exists for would then not hold.
func requireTestPortsBelowEphemeral(t *testing.T) {
	t.Helper()
	body, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return // not Linux: macOS hands out ports from 49152
	}
	fields := strings.Fields(string(body))
	if len(fields) == 2 {
		if low, err := strconv.Atoi(fields[0]); err == nil && low < testPortCeiling {
			t.Fatalf("the kernel hands out ports from %d (ip_local_port_range %q), inside the test servers' range [%d, %d)",
				low, strings.TrimSpace(string(body)), testPortFloor, testPortCeiling)
		}
	}
}

// captureFailure terminates only the test's owned server and reads its log
// after Wait has joined the output copier. Register it after client cleanups
// when a failure needs the sockets to remain open in the captured state.
func (s *testServer) captureFailure(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	state, _ := exec.CommandContext(ctx, "ps", "-o", "pid,pcpu,rss,state,etime", "-p", strconv.Itoa(s.cmd.Process.Pid)).CombinedOutput()
	cancel()
	t.Logf("failed server process state:\n%s", state)
	if runtime.GOOS == "darwin" {
		// The process's state is its busiest thread's, so U says only that some
		// thread is in uninterruptible kernel wait. Per-thread states and the
		// kernel's network buffers say which, and whether the buffers had run
		// out: a loop parked in kevent is idle, a thread at U in write(2) is the
		// kernel's doing.
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		threads, _ := exec.CommandContext(ctx, "ps", "-M", "-o", "pid,pcpu,state,utime,stime", "-p", strconv.Itoa(s.cmd.Process.Pid)).CombinedOutput()
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		buffers, _ := exec.CommandContext(ctx, "netstat", "-mm").CombinedOutput()
		cancel()
		t.Logf("failed server threads:\n%s\nkernel network buffers:\n%s", threads, buffers)
		// SIGQUIT cannot always unwind the loop's running thread. Sample only
		// this owned failed process before terminating it; bound diagnostic time.
		path := filepath.Join(t.TempDir(), "server.sample")
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		_ = exec.CommandContext(ctx, "/usr/bin/sample", strconv.Itoa(s.cmd.Process.Pid), "1", "1", "-file", path).Run()
		cancel()
		if data, err := os.ReadFile(path); err == nil {
			t.Logf("failed server sample:\n%s", data)
		}
	}
	_ = s.cmd.Process.Signal(syscall.SIGQUIT)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.stopped = true
	t.Logf("failed server diagnostics:\n%s", s.log.String())
}

func (s *testServer) stop(t *testing.T) {
	t.Helper()
	s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		s.stopped = true
		if err != nil {
			t.Fatalf("shutdown: %v\n%s", err, s.log.String())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown timeout")
	}
}
func connectTest(t *testing.T, s *testServer) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", s.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// The reader has to wrap the same value: bufio over the raw connection
	// would read past the refresh and keep the deadline that was never set.
	conn := &idleConn{Conn: c, idle: 5 * time.Second}
	return conn, bufio.NewReader(conn)
}

// idleConn gives each read and write its own deadline, so the limit is that no
// single operation hangs rather than that the whole test finishes in time.
//
// One deadline set at connect is a budget for everything the connection goes on
// to do, which makes every test sharing it a race against one clock.
// TestAsyncAppendPipelineAndRestart/always spends that budget on a hundred
// appendfsync-always round trips and a rewrite, and on a slow runner it loses -
// reporting an i/o timeout that says nothing about the server. A per-operation
// deadline still catches a server that stops answering, which is what the
// deadline was for.
//
// A caller that sets its own deadline means it, so refreshing stops there:
// TestSlowReaderDoesNotBlockOtherClients tightens the deadline deliberately to
// prove another client is not blocked, and a wrapper that kept widening it again
// would quietly delete the assertion.
type idleConn struct {
	net.Conn
	idle                  time.Duration
	mu                    sync.Mutex
	readFixed, writeFixed bool
}

func (c *idleConn) SetDeadline(at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetDeadline(at); err != nil {
		return err
	}
	c.readFixed, c.writeFixed = true, true
	return nil
}
func (c *idleConn) SetReadDeadline(at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetReadDeadline(at); err != nil {
		return err
	}
	c.readFixed = true
	return nil
}
func (c *idleConn) SetWriteDeadline(at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetWriteDeadline(at); err != nil {
		return err
	}
	c.writeFixed = true
	return nil
}
func (c *idleConn) refresh(read bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if read && !c.readFixed {
		return c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	if !read && !c.writeFixed {
		return c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	return nil
}
func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.refresh(true); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.refresh(false); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func request(parts ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return b.String()
}
func reply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(line, "$") {
		n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			t.Fatal(err)
		}
		if n >= 0 {
			p := make([]byte, n+2)
			if _, err := io.ReadFull(r, p); err != nil {
				t.Fatal(err)
			}
			return string(p[:n])
		}
	}
	return strings.TrimSpace(line)
}
func call(t *testing.T, c net.Conn, r *bufio.Reader, parts ...string) string {
	t.Helper()
	if _, err := io.WriteString(c, request(parts...)); err != nil {
		t.Fatal(err)
	}
	return reply(t, r)
}

func TestAuthenticatedPersistenceAndTornTail(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.aof")
	args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", path, "-requirepass-env", "KEEL_TEST_PASSWORD"}
	s := startTestServer(t, args...)
	c, r := connectTest(t, s)
	if got := call(t, c, r, "SET", "private", "value"); !strings.HasPrefix(got, "-NOAUTH") {
		t.Fatal(got)
	}
	if got := call(t, c, r, "AUTH", "wrong"); !strings.HasPrefix(got, "-WRONGPASS") {
		t.Fatal(got)
	}
	if got := call(t, c, r, "AUTH", "default", "integration-secret"); got != "+OK" {
		t.Fatal(got)
	}
	if got := call(t, c, r, "SET", "private", "value", "NX", "PX", "60000"); got != "+OK" {
		t.Fatal(got)
	}
	if got := call(t, c, r, "HSET", "hash", "field", "value"); got != ":1" {
		t.Fatal(got)
	}
	if got := call(t, c, r, "PEXPIRE", "hash", "60000"); got != ":1" {
		t.Fatal(got)
	}
	s.stop(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("*3\r\n$3\r\nSET\r\n$6\r\nbroken")
	f.Close()
	for i := 0; i < 2; i++ {
		s = startTestServer(t, args...)
		c, r = connectTest(t, s)
		call(t, c, r, "AUTH", "integration-secret")
		if got := call(t, c, r, "GET", "private"); got != "value" {
			t.Fatal(got)
		}
		if got := call(t, c, r, "HGET", "hash", "field"); got != "value" {
			t.Fatal(got)
		}
		if got := call(t, c, r, "SET", "after", "repair"); got != "+OK" {
			t.Fatal(got)
		}
		s.stop(t)
	}
}

func TestSlowReaderDoesNotBlockOtherClients(t *testing.T) {
	// Not parallel: it fills the kernel's socket buffers, which every process
	// on the machine shares, and requires other clients to be answered while it
	// does, within deadlines that a test filling them beside it would stretch.
	for _, threads := range []string{"1", "4"} {
		t.Run(threads, func(t *testing.T) {
			growLoopbackSendBuffers(t, 1)
			s := startTestServer(t, "-io-threads", threads)
			c, r := connectTest(t, s)
			if got := call(t, c, r, "SET", "large", strings.Repeat("x", 1<<20)); got != "+OK" {
				t.Fatal(got)
			}
			slow, _ := connectTest(t, s)
			tcp := slow.(*idleConn).Conn.(*net.TCPConn)
			if err := tcp.SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					s.captureFailure(t)
				}
			})
			if _, err := io.WriteString(slow, strings.Repeat(request("GET", "large"), 32)); err != nil {
				t.Fatal(err)
			}
			// Do not infer backpressure from a sleep. Observe user-space replies
			// retained after the nonblocking flush, before testing PING.
			observed := false
			deadline := time.Now().Add(2 * time.Second)
			if err := c.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			for time.Now().Before(deadline) {
				stats := call(t, c, r, "INFO", "clients")
				for _, line := range strings.Split(stats, "\r\n") {
					if raw, ok := strings.CutPrefix(line, "retained_reply_bytes:"); ok {
						queued, err := strconv.Atoi(raw)
						if err != nil {
							t.Fatal(err)
						}
						observed = queued > 64<<10
					}
				}
				if observed {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !observed {
				t.Fatal("slow client never retained queued replies")
			}
			if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if got := call(t, c, r, "PING"); got != "+PONG" {
				t.Fatal(got)
			}
			s.stop(t)
		})
	}
}

func TestInvalidModesExitPromptly(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-mode", "net", "-appendonly"}, {"-maxmemory", "18446744073709551615gb"}, {"-mode", "net-nolock"}} {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestServerProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "KEEL_TEST_SERVER=1")
		if err := cmd.Run(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestPendingRepliesSurviveOtherTraffic(t *testing.T) {
	// Not parallel: it fills the kernel's socket buffers, which every process
	// on the machine shares, and requires other clients to be answered while it
	// does, within deadlines that a test filling them beside it would stretch.
	for _, mode := range []string{"kqueue", "kqueue-nobuf"} {
		t.Run(mode, func(t *testing.T) {
			s := startTestServer(t, "-mode", mode)
			c, r := connectTest(t, s)
			value := strings.Repeat("payload-", 1<<18)
			if got := call(t, c, r, "SET", "large", value); got != "+OK" {
				t.Fatal(got)
			}
			slow, reader := connectTest(t, s)
			progress := &replyProgressReader{Reader: slow}
			reader = bufio.NewReader(progress)
			completed := 0
			// Capture before connectTest's socket cleanups: a stack collected after
			// both peers close only shows the server's now-idle event loop.
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("pending reply progress: complete=%d/8 bytes=%d reads=%d last_read_age=%s buffered=%d", completed, progress.bytes, progress.reads, time.Since(progress.lastRead), reader.Buffered())
					s.captureFailure(t)
				}
			})
			if _, err := io.WriteString(slow, strings.Repeat(request("GET", "large"), 8)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			for i := 0; i < 20; i++ {
				if got := call(t, c, r, "PING"); got != "+PONG" {
					t.Fatal(got)
				}
			}
			for i := 0; i < 8; i++ {
				if got := reply(t, reader); got != value {
					t.Fatalf("reply %d corrupted", i)
				}
				completed++
			}
			if got := call(t, slow, reader, "PING"); got != "+PONG" {
				t.Fatal(got)
			}
			s.stop(t)
		})
	}
}

// replyProgressReader records transport progress without changing read sizes or
// deadlines. The test and its cleanup use it on the same goroutine.
type replyProgressReader struct {
	io.Reader
	bytes, reads int
	lastRead     time.Time
}

func (r *replyProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.reads++
	r.bytes += n
	if n > 0 {
		r.lastRead = time.Now()
	}
	return n, err
}

func TestAOFWriteFailureDoesNotAcknowledge(t *testing.T) {
	t.Parallel()
	for _, async := range []bool{false, true} {
		t.Run(strconv.FormatBool(async), func(t *testing.T) {
			t.Parallel()
			args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", filepath.Join(t.TempDir(), "limited.aof")}
			if async {
				args = append(args, "-aof-async-append")
			}
			s := startLimitedTestServer(t, "1024", args...)
			c, r := connectTest(t, s)
			defer c.Close()
			io.WriteString(c, request("SET", "large", strings.Repeat("x", 4096)))
			line, err := r.ReadString('\n')
			if err == nil && line == "+OK\r\n" {
				t.Fatal("acknowledged a failed AOF write")
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("server did not fail promptly")
			}
		})
	}
}

func TestIdleActiveExpiry(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	c, r := connectTest(t, s)
	defer c.Close()
	if got := call(t, c, r, "SET", "idle", "value", "PX", "50"); got != "+OK" {
		t.Fatal(got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// INFO does not read the key or trigger lazy expiry.
		info := call(t, c, r, "INFO", "stats")
		if strings.Contains(info, "expired_keys:1\r\n") {
			s.stop(t)
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("idle TTL was not actively reclaimed")
}

// Accepted connections are counted in INFO rather than logged: a line per
// accept floods the log of any application with a pool or short-lived clients.
func TestConnectionsCountedNotLogged(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	c, r := connectTest(t, s)
	before := connectionsReceived(t, call(t, c, r, "INFO", "stats"))
	for i := 0; i < 3; i++ {
		other, otherReader := connectTest(t, s)
		if got := call(t, other, otherReader, "PING"); got != "+PONG" {
			t.Fatal(got)
		}
		other.Close()
	}
	if got := connectionsReceived(t, call(t, c, r, "INFO", "stats")) - before; got != 3 {
		t.Fatalf("total_connections_received rose by %d over three connections", got)
	}
	s.stop(t)
	if strings.Contains(s.log.String(), "new client") {
		t.Fatalf("connection logged:\n%s", s.log.String())
	}
}

func connectionsReceived(t *testing.T, info string) int {
	t.Helper()
	for _, line := range strings.Split(info, "\r\n") {
		if v, ok := strings.CutPrefix(line, "total_connections_received:"); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no total_connections_received in INFO stats:\n%s", info)
	return 0
}

func TestAsyncAppendPipelineAndRestart(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"always", "everysec", "no"} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			args := []string{"-appendonly", "-aof-async-append", "-appendfsync", policy, "-appendfilename", filepath.Join(t.TempDir(), "log")}
			s := startTestServer(t, args...)
			c, r := connectTest(t, s)
			for i := 0; i < 100; i++ {
				io.WriteString(c, request("SET", "k", strconv.Itoa(i))+request("GET", "k"))
				if got := reply(t, r); got != "+OK" {
					t.Fatal(got)
				}
				if got := reply(t, r); got != strconv.Itoa(i) {
					t.Fatal(got)
				}
			}
			if got := call(t, c, r, "BGREWRITEAOF"); !strings.HasPrefix(got, "+") {
				t.Fatal(got)
			}
			c.Close()
			s.stop(t)
			s = startTestServer(t, args...)
			c, r = connectTest(t, s)
			if got := call(t, c, r, "GET", "k"); got != "99" {
				t.Fatal(got)
			}
			c.Close()
			s.stop(t)
		})
	}
}

func TestReplicaFullSyncWritesAndReconnect(t *testing.T) {
	t.Parallel()
	primaryArgs := []string{"-appendonly", "-aof-async-append", "-replication-feed", "-requirepass-env", "KEEL_TEST_PASSWORD", "-appendfilename", filepath.Join(t.TempDir(), "primary")}
	primary := startTestServer(t, primaryArgs...)
	pc, pr := connectTest(t, primary)
	call(t, pc, pr, "AUTH", "integration-secret")
	call(t, pc, pr, "SET", "replicated", "before")
	replicaArgs := []string{"-appendonly", "-aof-async-append", "-requirepass-env", "KEEL_TEST_PASSWORD", "-primary-password-env", "KEEL_TEST_PASSWORD", "-replicaof", primary.addr, "-appendfilename", filepath.Join(t.TempDir(), "replica")}
	replica := startTestServer(t, replicaArgs...)
	rc, rr := connectTest(t, replica)
	call(t, rc, rr, "AUTH", "integration-secret")
	await := func(expected string) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			if call(t, rc, rr, "GET", "replicated") == expected {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("replica did not catch up", expected)
	}
	await("before")
	if got := call(t, rc, rr, "SET", "replicated", "illegal"); !strings.Contains(got, "READONLY") {
		t.Fatal(got)
	}
	call(t, pc, pr, "SET", "replicated", "after")
	await("after")
	rc.Close()
	replica.stop(t)
	call(t, pc, pr, "SET", "replicated", "while-offline")
	replica = startTestServer(t, replicaArgs...)
	rc, rr = connectTest(t, replica)
	call(t, rc, rr, "AUTH", "integration-secret")
	await("while-offline")
	pc.Close()
	primary.stop(t) // fence the old writer before manual promotion
	rc.SetDeadline(time.Now().Add(10 * time.Second))
	deadline := time.Now().Add(7 * time.Second)
	stale := false
	for time.Now().Before(deadline) {
		if strings.Contains(call(t, rc, rr, "GET", "replicated"), "MASTERDOWN") {
			stale = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !stale {
		t.Fatal("replica served stale state beyond its window")
	}
	rc.Close()
	replica.stop(t) // clean shutdown flushes the complete applied state
	promoted := startTestServer(t, "-appendonly", "-aof-async-append", "-requirepass-env", "KEEL_TEST_PASSWORD", "-appendfilename", replicaArgs[len(replicaArgs)-1])
	c, r := connectTest(t, promoted)
	call(t, c, r, "AUTH", "integration-secret")
	if got := call(t, c, r, "GET", "replicated"); got != "while-offline" {
		t.Fatal(got)
	}
	if got := call(t, c, r, "SET", "replicated", "promoted"); got != "+OK" {
		t.Fatal(got)
	}
	c.Close()
	promoted.stop(t)
}
