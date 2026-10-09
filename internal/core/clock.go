package core

import _ "unsafe" // for go:linkname

// nanotime is the runtime's monotonic clock, in nanoseconds: the clock
// time.Since reads, without the wall clock that time.Now reads beside it.
//
//go:linkname nanotime runtime.nanotime
func nanotime() int64

// microsBetween is the time between two readings of nanotime as Redis counts
// a command's: the difference of the two instants, each taken in whole
// microseconds, as call() takes ustime() before and after the command.
//
// Redis's default build reads the wall clock there (gettimeofday); Keel reads
// the monotonic clock, so a step of the system clock cannot make a command's
// time negative or huge. That is the one departure from Redis in commandstats
// (docs/info-compatibility.md).
func microsBetween(started, ended int64) uint64 { return uint64(ended/1000 - started/1000) }
