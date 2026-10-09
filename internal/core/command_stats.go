package core

import (
	"fmt"
	"sort"
	"strings"
)

// commandStat is one command's line of INFO commandstats, as Redis keeps it
// on each of its commands: how often it ran, for how many microseconds in
// all, and how often it was refused before it ran or failed as it ran.
type commandStat struct {
	calls, usec, rejected, failed uint64
}

// statNames are the names commandstats reports, Redis's: each command in
// lower case, and each subcommand of a container after its name and a '|'
// (config|get). A command's index entry holds its place here (stat); a
// container's subcommands follow it, in the order of its subcommand table.
var statNames []string

// noStat is the place of a command Redis keeps no line for: one it does not
// have, or a container's subcommand it does not have.
const noStat = ^uint16(0)

// statOf is cmd's place in statNames, or noStat.
func statOf(cmd *Command) uint16 {
	entry, known := commands[cmd.Cmd]
	if !known {
		return noStat
	}
	if entry.container {
		return subcommandStat(cmd, entry)
	}
	return entry.stat
}

// subcommandStat is the place of a container's subcommand: the container's
// own when none is named, which Redis refuses for its count against the
// container, and noStat for one the container does not have.
func subcommandStat(cmd *Command, entry commandEntry) uint16 {
	if len(cmd.Args) == 0 {
		return entry.stat
	}
	for i, sub := range subcommandsOf(cmd.Cmd) {
		if strings.EqualFold(cmd.Args[0], sub.name) {
			return entry.stat + 1 + uint16(i)
		}
	}
	return noStat
}

// noteRun counts one run of the command at stat, from started, a reading of
// nanotime, to now: a call, its time, and whether it failed.
func (e *Engine) noteRun(stat uint16, started int64, failed bool) {
	s := &e.cmdStats[stat]
	s.calls++
	s.usec += microsBetween(started, nanotime())
	if failed {
		s.failed++
	}
	e.totals.commands++
}

// CommandStart is when a command the transport answers itself started: the
// clock, and how many error replies there had been, which tells
// NoteCommandRan whether the command failed, as Redis's call() tells.
type CommandStart struct {
	started int64
	errors  uint64
}

// StartCommand marks the start of a command the transport answers itself,
// such as AUTH, HELLO or CLIENT, for NoteCommandRan.
func (e *Engine) StartCommand() CommandStart {
	return CommandStart{started: nanotime(), errors: e.totals.errors}
}

// NoteCommandRan counts a command the transport ran itself, from start: a
// call, its time, a failure if an error reply was counted since, and one
// more command processed. Commands that run on e count themselves.
func (e *Engine) NoteCommandRan(cmd *Command, start CommandStart) {
	stat := statOf(cmd)
	if stat == noStat {
		e.totals.commands++
		return
	}
	e.noteRun(stat, start.started, e.totals.errors > start.errors)
}

// NoteCommandRefused counts a command the transport refused before it ran,
// such as NOAUTH, toward its rejected_calls. A command, or a subcommand,
// Redis does not have has no line to count it on.
func (e *Engine) NoteCommandRefused(cmd *Command) {
	if stat := statOf(cmd); stat != noStat {
		e.cmdStats[stat].rejected++
	}
}

// commandStatsInfo is INFO's Commandstats section: a line for each command
// that has run or been refused, in Redis's format. Redis lists them in its
// hash table's order, which no client relies on; they come in name order
// here. usec_per_call is computed in single precision, as Redis computes it.
func (e *Engine) commandStatsInfo(b *strings.Builder) {
	b.WriteString("# Commandstats\r\n")
	order := make([]int, 0, len(e.cmdStats))
	for i, s := range e.cmdStats {
		if s.calls != 0 || s.rejected != 0 || s.failed != 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return statNames[order[a]] < statNames[order[b]] })
	for _, i := range order {
		s := e.cmdStats[i]
		perCall := float32(0)
		if s.calls != 0 {
			perCall = float32(s.usec) / float32(s.calls)
		}
		fmt.Fprintf(b, "cmdstat_%s:calls=%d,usec=%d,usec_per_call=%.2f,rejected_calls=%d,failed_calls=%d\r\n",
			statNames[i], s.calls, s.usec, perCall, s.rejected, s.failed)
	}
	b.WriteString("\r\n")
}
