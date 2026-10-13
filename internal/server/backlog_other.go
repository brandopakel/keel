//go:build !linux

package server

// warnTCPBacklog says nothing here: the kernel's limit on the backlog is
// read only on Linux, as Redis reads it from /proc (checkTcpBacklogSettings
// reads it from sysctl on the BSDs too, which Keel does not yet).
func warnTCPBacklog(int) {}
