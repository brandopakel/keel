package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
)

// startProfiles records local diagnostic files only when explicitly requested.
// A fresh private directory prevents accidental overwrites and exposes no HTTP
// endpoint. SIGUSR1 captures live heap/alloc profiles before connections close;
// final heap/alloc profiles and memory stats describe the shutdown point.
func startProfiles(directory string) (func() error, error) {
	if directory == "" {
		return func() error { return nil }, nil
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, fmt.Errorf("create fresh profile directory: %w", err)
	}
	cpu, err := os.OpenFile(filepath.Join(directory, "cpu.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		cpu.Close()
		return nil, err
	}
	requests := make(chan os.Signal, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var snapshotErr error // Read only after done closes.
	signal.Notify(requests, syscall.SIGUSR1)
	go func() {
		defer close(done)
		sequence := 0
		for {
			select {
			case <-stop:
				return
			case <-requests:
				sequence++
				if err := writeMemoryProfiles(directory, fmt.Sprintf("live-%d-", sequence)); snapshotErr == nil {
					snapshotErr = err
				}
			}
		}
	}()
	return func() error {
		signal.Stop(requests)
		close(stop)
		<-done
		pprof.StopCPUProfile()
		if err := cpu.Close(); err != nil {
			return err
		}
		if snapshotErr != nil {
			return snapshotErr
		}
		return writeMemoryProfiles(directory, "")
	}, nil
}

func writeMemoryProfiles(directory, prefix string) error {
	// Diagnostic captures deliberately collect the live heap after a GC.
	runtime.GC()
	for _, name := range []string{"heap", "allocs"} {
		file, err := os.OpenFile(filepath.Join(directory, prefix+name+".pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		err = pprof.Lookup(name).WriteTo(file, 0)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	data, err := json.MarshalIndent(struct {
		GoVersion string
		Memory    runtime.MemStats
	}{runtime.Version(), memory}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, prefix+"runtime.json"), append(data, '\n'), 0600)
}
