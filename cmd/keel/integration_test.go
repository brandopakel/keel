package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServerProcess(t *testing.T) {
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
	cmd     *exec.Cmd
	addr    string
	log     bytes.Buffer
	stopped bool
}

func startTestServer(t *testing.T, args ...string) *testServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	s := &testServer{addr: fmt.Sprintf("127.0.0.1:%d", port)}
	argv := append([]string{"-test.run=^TestServerProcess$", "--", "-host", "127.0.0.1", "-port", strconv.Itoa(port)}, args...)
	s.cmd = exec.Command(os.Args[0], argv...)
	s.cmd.Env = append(os.Environ(), "KEEL_TEST_SERVER=1", "KEEL_TEST_PASSWORD=integration-secret")
	s.cmd.Stdout = &s.log
	s.cmd.Stderr = &s.log
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.stopped {
			s.cmd.Process.Kill()
			s.cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", s.addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not listen")
	return nil
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
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
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
	for _, threads := range []string{"1", "4"} {
		t.Run(threads, func(t *testing.T) {
			s := startTestServer(t, "-io-threads", threads)
			c, r := connectTest(t, s)
			if got := call(t, c, r, "SET", "large", strings.Repeat("x", 1<<20)); got != "+OK" {
				t.Fatal(got)
			}
			slow, _ := connectTest(t, s)
			io.WriteString(slow, strings.Repeat(request("GET", "large"), 32))
			time.Sleep(100 * time.Millisecond)
			c.SetDeadline(time.Now().Add(2 * time.Second))
			if got := call(t, c, r, "PING"); got != "+PONG" {
				t.Fatal(got)
			}
			s.stop(t)
		})
	}
}

func TestInvalidModesExitPromptly(t *testing.T) {
	for _, args := range [][]string{{"-mode", "net", "-appendonly"}, {"-maxmemory", "18446744073709551615gb"}, {"-mode", "net-nolock"}} {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestServerProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "KEEL_TEST_SERVER=1")
		if err := cmd.Run(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestPendingRepliesSurviveOtherTraffic(t *testing.T) {
	for _, mode := range []string{"kqueue", "kqueue-nobuf"} {
		t.Run(mode, func(t *testing.T) {
			s := startTestServer(t, "-mode", mode)
			c, r := connectTest(t, s)
			value := strings.Repeat("payload-", 1<<18)
			if got := call(t, c, r, "SET", "large", value); got != "+OK" {
				t.Fatal(got)
			}
			slow, reader := connectTest(t, s)
			io.WriteString(slow, strings.Repeat(request("GET", "large"), 8))
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
			}
			if got := call(t, slow, reader, "PING"); got != "+PONG" {
				t.Fatal(got)
			}
			s.stop(t)
		})
	}
}

func TestAOFWriteFailureDoesNotAcknowledge(t *testing.T) {
	t.Setenv("KEEL_TEST_FILE_LIMIT", "1024")
	s := startTestServer(t, "-appendonly", "-appendfsync", "always", "-appendfilename", filepath.Join(t.TempDir(), "limited.aof"))
	c, r := connectTest(t, s)
	io.WriteString(c, request("SET", "large", strings.Repeat("x", 4096)))
	line, err := r.ReadString('\n')
	if err == nil && line == "+OK\r\n" {
		t.Fatal("acknowledged a failed AOF write")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("server did not fail promptly")
	}
}
