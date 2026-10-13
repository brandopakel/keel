package server

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// warnTCPBacklog says at startup, in Redis's words, when the kernel's
// somaxconn is lower than the backlog asked for, since listen caps the
// backlog there without saying so (Redis's checkTcpBacklogSettings). Reading
// the limit is Linux's /proc; where it cannot be read, nothing is said.
func warnTCPBacklog(backlog int) {
	body, err := os.ReadFile("/proc/sys/net/core/somaxconn")
	if err != nil {
		return
	}
	somaxconn, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err == nil && somaxconn > 0 && somaxconn < backlog {
		log.Printf("WARNING: The TCP backlog setting of %d cannot be enforced because /proc/sys/net/core/somaxconn is set to the lower value of %d.", backlog, somaxconn)
	}
}
