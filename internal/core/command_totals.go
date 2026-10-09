package core

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"
)

// commandTotals are the counts INFO stats and INFO errorstats report as
// Redis reports them (docs/info-compatibility.md, part c1):
// total_commands_processed, instantaneous_ops_per_sec, total_error_replies
// and the errorstat_ lines. None of them reads a clock per command.
//
// A command is counted when it runs, as Redis's call() counts it: one refused
// before it runs (an unknown name, the wrong count, NOAUTH, a replica's
// READONLY) is not, and one that fails as it runs (WRONGTYPE, a value that is
// not an integer) is. A command queued by MULTI is counted when EXEC runs it,
// and EXEC with it. An error reply is counted however it came about, refusals
// included, inside EXEC's reply too. The transport counts the commands and
// replies it answers itself (NoteCommand, NoteErrorReply); the log's replay
// counts nothing, as Redis's replay, which bypasses call(), counts nothing.
type commandTotals struct {
	commands uint64
	errors   uint64
	// hits and misses are keyspace_hits and keyspace_misses: each key a
	// command looks up to read, as Redis's lookupKeyRead counts it (see
	// keyspaceReads).
	hits, misses uint64
	// errorPrefixes counts error replies by their first word, the prefix
	// Redis keeps them under; at most maxErrorPrefixes of them, as Redis
	// keeps. A reply under one more replaces them all with
	// errorStatsDisabled, count 1, and nothing more is kept by prefix until
	// CONFIG RESETSTAT, though total_error_replies goes on counting.
	errorPrefixes  map[string]uint64
	errorsDisabled bool
	// ops is instantaneous_ops_per_sec's sampler, and the rest the network
	// rates', instantaneous_input_kbps and its kind, which sample the
	// transport's byte counts as Redis's serverCron samples its own.
	ops                            instantaneousMetric
	netIn, netOut, replIn, replOut instantaneousMetric
}

const (
	// maxErrorPrefixes is Redis's ERROR_STATS_NUMBER.
	maxErrorPrefixes = 128
	// errorStatsDisabled is the one prefix Redis keeps once there have been
	// maxErrorPrefixes others.
	errorStatsDisabled = "ERRORSTATS_DISABLED"
)

// instantaneousMetric is Redis's: a rate sampled every 100 ms against the
// monotonic clock, reported as the mean of the last 16 samples.
type instantaneousMetric struct {
	samples   [16]int64
	next      int
	lastAt    time.Time
	lastValue uint64
}

// instantaneousPeriod is how often Redis's serverCron samples a rate
// (run_with_period(100)).
const instantaneousPeriod = 100 * time.Millisecond

// sample records value at now, if a period has passed since the last sample,
// as Redis's trackInstantaneousMetric does: per second, over the time since
// the last sample. The first sample only sets where the next one counts from.
func (m *instantaneousMetric) sample(value uint64, now time.Time) {
	if !m.lastAt.IsZero() {
		elapsed := now.Sub(m.lastAt)
		if elapsed < instantaneousPeriod {
			return
		}
		m.samples[m.next] = int64(value-m.lastValue) * int64(time.Second) / int64(elapsed)
		m.next = (m.next + 1) % len(m.samples)
	}
	m.lastAt, m.lastValue = now, value
}

// sampledAt is whether the last sample was taken at now, so that the
// samplers that follow ops take theirs in the same periods.
func (m *instantaneousMetric) sampledAt(now time.Time) bool { return m.lastAt.Equal(now) }

// kbps is Redis's instantaneous_*_kbps: the rate in bytes, divided by 1024 in
// single precision, as Redis divides it.
func (m *instantaneousMetric) kbps() float32 { return float32(m.rate()) / 1024 }

// rate is Redis's getInstantaneousMetric: the mean of the samples, the ones
// not yet taken counting as zero.
func (m *instantaneousMetric) rate() int64 {
	var sum int64
	for _, s := range m.samples {
		sum += s
	}
	return sum / int64(len(m.samples))
}

// noteLookup counts a key a command looked up to read: a hit if it is held,
// whatever its type, and a miss if it is not, an expired key included, as
// Redis's lookupKeyRead counts them.
func (e *Engine) noteLookup(held bool) {
	if held {
		e.totals.hits++
	} else {
		e.totals.misses++
	}
}

// NoteErrorReply counts an error reply the transport sent itself, such as
// NOAUTH or a protocol error, toward total_error_replies and errorstats.
// Replies e produces count themselves. A reply that is not an error is not
// counted.
func (e *Engine) NoteErrorReply(reply []byte) {
	if len(reply) > 0 && reply[0] == '-' {
		e.noteError(reply)
	}
}

// noteError counts reply, an error, as Redis's afterErrorReply counts one:
// under the word after its '-', when a space ends it within the first 32
// bytes, and otherwise under ERR.
func (e *Engine) noteError(reply []byte) {
	t := &e.totals
	t.errors++
	if t.errorsDisabled {
		return
	}
	prefix := "ERR"
	head := reply[:min(len(reply), 32)]
	if space := bytes.IndexByte(head, ' '); space > 0 {
		prefix = string(head[1:space])
	}
	if t.errorPrefixes == nil {
		t.errorPrefixes = make(map[string]uint64)
	}
	if _, seen := t.errorPrefixes[prefix]; !seen && len(t.errorPrefixes) >= maxErrorPrefixes {
		// Redis logs the prefixes it had and then keeps only this one.
		clear(t.errorPrefixes)
		t.errorPrefixes[errorStatsDisabled] = 1
		t.errorsDisabled = true
		return
	}
	t.errorPrefixes[prefix]++
}

// SampleCommandRate takes instantaneous_ops_per_sec's sample, if a period has
// passed since the last one. The event loop calls it once a turn and the
// driver once a cycle, as Redis's serverCron samples it.
func (e *Engine) SampleCommandRate() {
	now := time.Now()
	t := &e.totals
	t.ops.sample(t.commands, now)
	if e.clientBuffers == nil {
		return
	}
	if !t.ops.sampledAt(now) {
		return
	}
	// Redis samples input and output with replication's included, and
	// replication's alone beside them.
	s := e.clientBuffers()
	t.netIn.sample(s.NetInputBytes+s.NetReplInputBytes, now)
	t.netOut.sample(s.NetOutputBytes+s.NetReplOutputBytes, now)
	t.replIn.sample(s.NetReplInputBytes, now)
	t.replOut.sample(s.NetReplOutputBytes, now)
}

// resetStats is CONFIG RESETSTAT: every statistic Redis's resetServerStats
// resets that Keel reports, the transport's through its hook. What is not a
// count, such as used_memory_peak, which Redis keeps too, is left alone.
func (e *Engine) resetStats() {
	e.totals = commandTotals{}
	clear(e.cmdStats)
	clear(e.latency)
	// The slow log's counts, not its entries, which SLOWLOG RESET clears.
	e.slowlog.count, e.slowlog.usecSum, e.slowlog.usecMax = 0, 0, 0
	e.expiredKeys = 0
	e.space.ResetEvicted()
	e.aof.rewrites = 0
	e.rewriteOutcome.failures = 0
	if e.statsReset != nil {
		e.statsReset()
	}
}

// SetStatsReset installs on e what CONFIG RESETSTAT calls to reset the
// transport's own statistics, total_connections_received and
// rejected_connections, or removes it when f is nil.
func (e *Engine) SetStatsReset(f func()) { e.statsReset = f }

// errorStatsInfo is INFO's Errorstats section: one line per prefix, in byte
// order, as Redis's radix tree gives them, with the characters INFO cannot
// carry written as underscores, as Redis's getSafeInfoString writes them.
func (e *Engine) errorStatsInfo(b *strings.Builder) {
	b.WriteString("# Errorstats\r\n")
	prefixes := make([]string, 0, len(e.totals.errorPrefixes))
	for p := range e.totals.errorPrefixes {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	safe := strings.NewReplacer("#", "_", ":", "_", "\n", "_", "\r", "_")
	for _, p := range prefixes {
		fmt.Fprintf(b, "errorstat_%s:count=%d\r\n", safe.Replace(p), e.totals.errorPrefixes[p])
	}
	b.WriteString("\r\n")
}
