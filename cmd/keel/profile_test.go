package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProfilesCaptureLiveServerAndRefuseOverwrite(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "profiles")
	server := startTestServer(t, "-profile-dir", directory)
	connection, reader := connectTest(t, server)
	if call(t, connection, reader, "SET", "profile", "owned") != "+OK" {
		t.Fatal("profiled server did not write")
	}
	if err := server.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "live-1-runtime.json")
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		if json.Valid(data) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !json.Valid(data) || call(t, connection, reader, "GET", "profile") != "owned" {
		t.Fatal("live snapshot missing or server stopped serving")
	}
	server.stop(t)
	for _, name := range []string{"cpu.pprof", "heap.pprof", "allocs.pprof", "runtime.json", "live-1-heap.pprof", "live-1-allocs.pprof"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Size() == 0 || info.Mode().Perm() != 0600 {
			t.Fatalf("missing/nonprivate profile %s: %v", name, err)
		}
	}
	if _, err := startProfiles(directory); err == nil {
		t.Fatal("existing profile directory must not be overwritten")
	}
}
