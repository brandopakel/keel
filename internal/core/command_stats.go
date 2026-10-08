package core

import _ "unsafe" // for go:linkname

// nanotime is the runtime's monotonic clock, in nanoseconds: the one
// time.Since reads, without the wall clock time.Now reads beside it.
//
//go:linkname nanotime runtime.nanotime
func nanotime() int64

// commandStat is one command's line of Redis's INFO commandstats.
type commandStat struct {
	calls, failed, rejected uint64
	nanos                   uint64
}
