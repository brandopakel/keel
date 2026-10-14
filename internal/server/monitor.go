package server

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// MONITOR and RESET, as Redis 8.10.1 has them (server.c, monitorCommand and
// call's replicationFeedMonitors; networking.c, resetCommand and
// clearClientConnectionState).
//
// A connection in MONITOR mode is sent a line for each command any
// connection runs, as it runs: its time, the database and the sender's
// address, and its arguments, quoted as Redis quotes them (sdscatrepr). A
// command Redis keeps from monitors is not sent - one its spec marks ADMIN
// or SKIP_MONITOR, CONFIG and CLIENT LIST among them - and neither is one
// refused before it would run; what EXEC runs is sent before EXEC itself.
// AUTH's arguments and HELLO's AUTH user and password are sent as
// "(redacted)", whether or not the command succeeded: Redis redacts them only
// once the command has read them, so a HELLO it refuses earlier is sent with
// the password as written, which Keel does not do. The lines are queued
// with the connection, and the loop sends them with the turn's replies, in
// the order the commands ran.

// monitors are the connections in MONITOR mode, in the order they asked;
// fedMonitors those holding lines not yet sent.
var monitors, fedMonitors []*client

// errMonitorDenyBlocking is Redis's refusal of MONITOR inside EXEC.
var errMonitorDenyBlocking = errors.New("ERR MONITOR isn't allowed for DENY BLOCKING client")

// errResetNotNormal is Redis's refusal of RESET on a replica's connection.
var errResetNotNormal = errors.New("ERR can only reset normal client connections")

// monitor is MONITOR: the connection is sent every command from now on. A
// connection already monitoring is answered nothing, as Redis answers it.
func (c *client) monitor(w io.Writer) {
	if c.conn.exec != nil {
		responseError(errMonitorDenyBlocking, w)
		return
	}
	if c.conn.monitor || c.replica {
		return
	}
	c.conn.monitor = true
	monitors = append(monitors, c)
	w.Write(c.encode("OK", true))
}

// stopMonitoring takes c out of MONITOR mode, as RESET and a close do.
func (c *client) stopMonitoring() {
	if !c.conn.monitor {
		return
	}
	c.conn.monitor, c.conn.feed = false, nil
	for i, m := range monitors {
		if m == c {
			monitors = append(monitors[:i], monitors[i+1:]...)
			break
		}
	}
	monitors = clearTail(monitors)
}

// clearTail drops the reference past a slice's end that removing one leaves.
func clearTail(cs []*client) []*client {
	if len(cs) < cap(cs) {
		cs[:cap(cs)][len(cs)] = nil
	}
	return cs
}

// Executed feeds what EXEC runs to the monitors, as it runs (core's
// ExecObserver).
func (c *client) Executed(cmd *core.Command) {
	if len(monitors) > 0 {
		feedMonitors(c, cmd)
	}
}

// feedMonitors queues cmd's line, sent by c, for every monitor, unless Redis
// keeps it from them.
func feedMonitors(c *client, cmd *core.Command) {
	if !core.ShownToMonitors(cmd) {
		return
	}
	line := monitorLine(nil, time.Now(), c.addr, cmd)
	for _, m := range monitors {
		if m.conn.killed {
			continue
		}
		if len(m.conn.feed) == 0 {
			fedMonitors = append(fedMonitors, m)
		}
		m.conn.feed = append(m.conn.feed, line...)
	}
}

// monitorLine appends replicationFeedMonitors' line for cmd, sent from addr
// at now: a status reply of the time in seconds and microseconds, the
// database and the address in brackets, and each argument quoted.
func monitorLine(b []byte, now time.Time, addr string, cmd *core.Command) []byte {
	us := now.UnixMicro()
	b = append(b, '+')
	b = strconv.AppendInt(b, us/1e6, 10)
	b = append(b, '.')
	frac := strconv.AppendInt(nil, us%1e6, 10)
	for i := len(frac); i < 6; i++ {
		b = append(b, '0')
	}
	b = append(b, frac...)
	b = append(b, " [0 "...)
	b = append(b, addr...)
	b = append(b, "] "...)
	b = appendRepr(b, cmd.SentName())
	redact := redactedArguments(cmd)
	for i, arg := range cmd.Args {
		b = append(b, ' ')
		if redact(i) {
			arg = "(redacted)"
		}
		b = appendRepr(b, arg)
	}
	return append(b, "\r\n"...)
}

// redactedArguments says which of cmd's arguments are sent to monitors as
// "(redacted)": AUTH's, and HELLO's AUTH user and password.
func redactedArguments(cmd *core.Command) func(int) bool {
	switch cmd.Cmd {
	case "AUTH":
		return func(int) bool { return true }
	case "HELLO":
		hidden := map[int]bool{}
		for i := 1; i < len(cmd.Args); i++ {
			if strings.EqualFold(cmd.Args[i], "AUTH") {
				hidden[i+1], hidden[i+2] = true, true
				i += 2
			}
		}
		return func(i int) bool { return hidden[i] }
	}
	return func(int) bool { return false }
}

// appendRepr appends s quoted as Redis's sdscatrepr quotes it: backslash and
// double quote escaped, \n \r \t \a \b by name, and any other byte outside
// printable ASCII as \xHH.
func appendRepr(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\', '"':
			b = append(b, '\\', ch)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '\a':
			b = append(b, '\\', 'a')
		case '\b':
			b = append(b, '\\', 'b')
		default:
			if ch >= ' ' && ch <= '~' {
				b = append(b, ch)
			} else {
				b = append(b, '\\', 'x', hex[ch>>4], hex[ch&0xf])
			}
		}
	}
	return append(b, '"')
}

// deliverMonitorFeeds puts the lines queued for each monitor this turn after
// what it is already owed, and adds a monitor owed nothing else to writable,
// which it returns. A monitor whose reads the loop holds while the log's
// worker flushes (paused) and that is owed nothing keeps its lines for a
// later turn. A monitor owed more than the output limit is killed, and the
// loop closes it at the next turn's end.
func deliverMonitorFeeds(writable []*client, arena *replyArena, paused map[int]*client) []*client {
	if len(fedMonitors) == 0 {
		return writable
	}
	kept := fedMonitors[:0]
	for _, m := range fedMonitors {
		feed := m.conn.feed
		if len(feed) == 0 || clients[m.fd] != m || m.conn.killed || !m.conn.monitor {
			m.conn.feed = nil
			continue
		}
		staged := false
		for _, w := range writable {
			if w == m {
				staged = true
				break
			}
		}
		switch {
		case staged:
			owed := m.out
			if m.inArena {
				owed = arena.buf[m.outStart:m.outEnd]
			}
			m.out = append(append(make([]byte, 0, len(owed)+len(feed)), owed...), feed...)
			m.inArena = false
		case len(m.out) > 0:
			m.out = append(append(make([]byte, 0, len(m.out)+len(feed)), m.out...), feed...)
			accountClient(m)
		case paused[m.fd] == m:
			kept = append(kept, m)
			continue
		default:
			m.out, m.inArena = feed, false
			writable = append(writable, m)
		}
		m.conn.feed = nil
		if len(m.out) > maxOutputBuffer {
			// A monitor that does not read what it is sent is closed, as
			// Redis closes one past its output buffer's limit.
			m.conn.killed = true
			killedClients = append(killedClients, m)
		}
	}
	clear(fedMonitors[len(kept):])
	fedMonitors = kept
	return dropKilled(writable)
}

// reset is RESET: the connection as it was when it connected, but for its
// library's name and version, as Redis's clearClientConnectionState leaves
// it. It leaves MONITOR mode, discards a transaction, speaks RESP2, needs
// AUTH again where the server has a password, and turns off CLIENT REPLY's
// OFF and SKIP, NO-EVICT and NO-TOUCH; its name is cleared.
func (c *client) reset(w io.Writer) {
	if c.replica {
		responseError(errResetNotNormal, w)
		return
	}
	c.stopMonitoring()
	c.tx = nil
	c.resp3 = false
	c.authenticated = c.requiredPassword() == ""
	c.name = ""
	c.conn.replyOff, c.conn.replySkipNext = false, false
	c.conn.noEvict, c.conn.noTouch = false, false
	w.Write([]byte("+RESET\r\n"))
}
