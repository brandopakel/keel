//go:build !amd64

package core

import "time"

var ticksStart = time.Now()

// ticks stands in for the time-stamp counter off amd64 (probe only).
func ticks() uint64 { return uint64(time.Since(ticksStart)) }
