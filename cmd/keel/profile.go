package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
)

// startProfiles records local diagnostic files only when explicitly requested.
// A fresh private directory prevents accidental overwrites and exposes no HTTP
// endpoint. Heap/alloc profiles and memory stats describe the shutdown point.
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
	return func() error {
		pprof.StopCPUProfile()
		if err := cpu.Close(); err != nil {
			return err
		}
		// These are diagnostic runs: collect the live heap after a completed GC.
		runtime.GC()
		for _, name := range []string{"heap", "allocs"} {
			file, err := os.OpenFile(filepath.Join(directory, name+".pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
		return os.WriteFile(filepath.Join(directory, "runtime.json"), append(data, '\n'), 0600)
	}, nil
}
