package main

// Throwaway diagnostic, not for merging: which process holds the port a
// readiness probe reached.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// listenerOwners returns the pids holding a listening socket on 127.0.0.1:port
// or 0.0.0.0:port, by /proc on Linux.
func listenerOwners(port int) string {
	if runtime.GOOS != "linux" {
		return "n/a"
	}
	inodes := map[string]bool{}
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
			local := f[1]
			i := strings.LastIndex(local, ":")
			p, err := strconv.ParseUint(local[i+1:], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			inodes["socket:["+f[9]+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return "none"
	}
	var owners []string
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		target, err := os.Readlink(fd)
		if err != nil || !inodes[target] {
			continue
		}
		pid := strings.Split(fd, "/")[2]
		cmd, _ := os.ReadFile("/proc/" + pid + "/cmdline")
		owners = append(owners, fmt.Sprintf("%s(%s)", pid, strings.ReplaceAll(string(cmd), "\x00", " ")))
	}
	return strings.Join(owners, ",")
}
