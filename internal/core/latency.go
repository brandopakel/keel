package core

import (
	"fmt"
	"sort"
	"strings"
)

// latencyHelp is Redis's LATENCY HELP.
var latencyHelp = HelpReply("LATENCY",
	"DOCTOR",
	"    Return a human readable latency analysis report.",
	"GRAPH <event>",
	"    Return an ASCII latency graph for the <event> class.",
	"HISTORY <event>",
	"    Return time-latency samples for the <event> class.",
	"LATEST",
	"    Return the latest latency samples for all events.",
	"RESET [<event> ...]",
	"    Reset latency data of one or more <event> classes.",
	"    (default: reset all data for all event classes)",
	"HISTOGRAM [COMMAND ...]",
	"    Return a cumulative distribution of latencies in the format of a histogram for the specified command names.",
	"    If no commands are specified then all histograms are replied.")

// latencyPercentiles are latency-tracking-info-percentiles, Redis's default
// of 50 99 99.9, as INFO latencystats names them.
var latencyPercentiles = []struct {
	label string
	value float64
}{{"50", 50}, {"99", 99}, {"99.9", 99.9}}

// cmdLATENCY implements LATENCY: HISTOGRAM from each command's latency
// histogram, which every command's time is recorded in as Redis records it
// with latency-tracking on, its default; and the latency monitor's
// subcommands (watch_latency.go).
//
// The subcommand and its count have been checked against the command table by
// the time this runs; see containerCommands.
func (e *Engine) cmdLATENCY(args []string) []byte {
	if err := CommandError(&Command{Cmd: "LATENCY", Args: args}); err != nil {
		return e.encode(err, false)
	}
	switch strings.ToLower(args[0]) {
	case "histogram":
		return e.encode(e.latencyHistograms(args[1:]), false)
	case "help":
		return latencyHelp
	}
	return e.encode(e.latencyMonitorReply(args), false)
}

// latencyHistograms is LATENCY HISTOGRAM's reply: for each command named, or
// every command when none is, that has run, its calls and its cumulative
// distribution, as Redis's fillCommandCDF writes them. A container named
// brings its subcommands; a name Redis does not have, nothing.
func (e *Engine) latencyHistograms(names []string) ReplyMap {
	var stats []uint16
	if len(names) == 0 {
		for i := range statNames {
			stats = append(stats, uint16(i))
		}
		sort.Slice(stats, func(a, b int) bool { return statNames[stats[a]] < statNames[stats[b]] })
	}
	for _, name := range names {
		stats = append(stats, statsNamed(name)...)
	}
	out := ReplyMap{}
	for _, stat := range stats {
		h := e.latency[stat]
		if h == nil {
			continue
		}
		buckets := ReplyMap{}
		previous := int64(0)
		for _, b := range h.logBuckets(1024) {
			if b.cumulativeCount > previous {
				buckets = append(buckets, b.highestEquivalent/1000, b.cumulativeCount)
			}
			previous = b.cumulativeCount
		}
		out = append(out, statNames[stat], ReplyMap{"calls", h.totalCount, "histogram_usec", buckets})
	}
	return out
}

// statsNamed are the places in statNames that LATENCY HISTOGRAM finds for
// name, as Redis's lookupCommandBySds finds a command: by its name, or a
// subcommand's as container|subcommand, without regard to case; a container
// brings its subcommands after it.
func statsNamed(name string) []uint16 {
	lower := strings.ToLower(name)
	for i, n := range statNames {
		if n != lower {
			continue
		}
		stats := []uint16{uint16(i)}
		for j := i + 1; j < len(statNames) && strings.HasPrefix(statNames[j], lower+"|"); j++ {
			stats = append(stats, uint16(j))
		}
		return stats
	}
	return nil
}

// recordLatency records a command's time in microseconds in its latency
// histogram, in nanoseconds as Redis's call() records it.
func (e *Engine) recordLatency(stat uint16, micros uint64) {
	h := e.latency[stat]
	if h == nil {
		h = newLatencyHistogram()
		e.latency[stat] = h
	}
	h.record(int64(micros) * 1000)
}

// latencyStatsInfo is INFO's Latencystats section: each command that has run,
// with its latency at Redis's default percentiles, in microseconds with three
// decimals, as Redis's fillPercentileDistributionLatencies writes it.
func (e *Engine) latencyStatsInfo(b *strings.Builder) {
	b.WriteString("# Latencystats\r\n")
	order := make([]int, 0, len(e.latency))
	for i, h := range e.latency {
		if h != nil {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return statNames[order[a]] < statNames[order[b]] })
	for _, i := range order {
		fmt.Fprintf(b, "latency_percentiles_usec_%s:", statNames[i])
		for j, p := range latencyPercentiles {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, "p%s=%.3f", p.label, float64(e.latency[i].valueAtPercentile(p.value))/1000.0)
		}
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
}
