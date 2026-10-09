package core

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// slowlog is Redis's slow log: the commands that took slowerThan
// microseconds or more, newest first, at most maxLen of them, each as Redis
// 8.10.1's slowlogCreateEntry records it. Its settings are Redis's defaults,
// which CONFIG GET reports.
type slowlog struct {
	entries []slowlogEntry // newest first
	nextID  int64
	// Redis's slowlog-log-slower-than, slowlog-max-len,
	// slowlog-entry-max-argc and slowlog-entry-max-string-len.
	slowerThan   int64
	maxLen       int
	maxArgc      int
	maxStringLen int
	// INFO stats' slowlog_commands_count and the time of what it counts, in
	// microseconds.
	count, usecSum, usecMax uint64
}

// slowlogEntry is one entry of the slow log, the seven fields SLOWLOG GET
// gives: id, when it ran (Unix seconds), how long it took (microseconds), its
// arguments as logged, the client's address and name, and how many
// arguments it really had.
type slowlogEntry struct {
	id       int64
	time     int64
	duration uint64
	args     []string
	peer     string
	name     string
	argc     int
}

func newSlowlog() slowlog {
	return slowlog{slowerThan: 10000, maxLen: 128, maxArgc: 32, maxStringLen: 128}
}

// noteSlow is slowlogPushCurrentCommand: cmd, which ran at stat and took
// micros, joins the slow log if it took long enough. EXEC never does, as
// Redis's does not (CMD_SKIP_SLOWLOG); what it ran does, one by one.
func (e *Engine) noteSlow(cmd *Command, stat uint16, micros uint64) {
	l := &e.slowlog
	if l.slowerThan < 0 || l.maxLen == 0 || int64(micros) < l.slowerThan || cmd.Cmd == "EXEC" {
		return
	}
	entry := slowlogEntry{id: l.nextID, time: time.Now().Unix(), duration: micros, argc: len(cmd.Args) + 1}
	l.nextID++
	if cmd.Client != nil {
		entry.peer, entry.name = cmd.Client.PeerAddr(), cmd.Client.ClientName()
	}
	argv := append([]string{cmd.sentName()}, cmd.Args...)
	redactSlowlogArgs(cmd.Cmd, argv)
	n := min(len(argv), l.maxArgc)
	entry.args = make([]string, n)
	for j := 0; j < n; j++ {
		switch arg := argv[j]; {
		case n != len(argv) && j == n-1:
			// The last place left says how many arguments did not fit.
			entry.args[j] = fmt.Sprintf("... (%d more arguments)", len(argv)-n+1)
		case len(arg) > l.maxStringLen:
			entry.args[j] = arg[:l.maxStringLen] + fmt.Sprintf("... (%d more bytes)", len(arg)-l.maxStringLen)
		default:
			entry.args[j] = arg
		}
	}
	l.entries = append([]slowlogEntry{entry}, l.entries...)
	if len(l.entries) > l.maxLen {
		l.entries = l.entries[:l.maxLen]
	}
	l.count++
	l.usecSum += micros
	l.usecMax = max(l.usecMax, micros)
	s := &e.cmdStats[stat]
	s.slowlogCount++
	s.slowlogUsecSum += micros
	s.slowlogUsecMax = max(s.slowlogUsecMax, micros)
}

// sensitiveSettings are the settings whose values Redis's CONFIG SET leaves
// out of the slow log (SENSITIVE_CONFIG).
var sensitiveSettings = map[string]bool{"masteruser": true, "masterauth": true, "requirepass": true,
	"tls-key-file-pass": true, "tls-client-key-file-pass": true}

// redacted is what Redis logs in place of a secret.
const redacted = "(redacted)"

// redactSlowlogArgs replaces the secrets in argv, the command's name and its
// arguments, as Redis's commands redact them for the slow log: AUTH's
// arguments, the user and password after HELLO's AUTH, and the value of a
// sensitive setting in CONFIG SET.
//
// Redis redacts them only once the command has got as far as reading them, so
// an AUTH of more than two arguments, a HELLO of an unsupported protocol
// version, or a CONFIG SET of an odd count logs the password as sent. Keel
// redacts them whatever the command goes on to do (docs/info-compatibility.md).
func redactSlowlogArgs(name string, argv []string) {
	switch name {
	case "AUTH":
		for j := 1; j < len(argv); j++ {
			argv[j] = redacted
		}
	case "HELLO":
		for j := 2; j+2 < len(argv); j++ {
			if strings.EqualFold(argv[j], "AUTH") {
				argv[j+1], argv[j+2] = redacted, redacted
				j += 2
			}
		}
	case "CONFIG":
		if len(argv) < 2 || !strings.EqualFold(argv[1], "SET") {
			return
		}
		for j := 2; j+1 < len(argv); j += 2 {
			if sensitiveSettings[strings.ToLower(argv[j])] {
				argv[j+1] = redacted
			}
		}
	}
}

// slowlogHelp is Redis's SLOWLOG HELP.
var slowlogHelp = HelpReply("SLOWLOG",
	"GET [<count>]",
	"    Return top <count> entries from the slowlog (default: 10, -1 mean all).",
	"    Entries are made of:",
	"    id, timestamp, time in microseconds, arguments array, client IP and port,",
	"    client name",
	"LEN",
	"    Return the length of the slowlog.",
	"RESET",
	"    Reset the slowlog.")

var errSlowlogCount = errors.New("ERR count should be greater than or equal to -1")

// cmdSLOWLOG implements SLOWLOG GET, LEN, RESET and HELP, as Redis does.
//
// The subcommand and its count have been checked against the command table by
// the time this runs; see containerCommands.
func (e *Engine) cmdSLOWLOG(args []string) []byte {
	if err := CommandError(&Command{Cmd: "SLOWLOG", Args: args}); err != nil {
		return e.encode(err, false)
	}
	l := &e.slowlog
	switch strings.ToLower(args[0]) {
	case "len":
		return e.encode(int64(len(l.entries)), false)
	case "reset":
		l.entries = nil
		return e.encode("OK", true)
	case "get":
		if len(args) > 2 {
			// Redis's addReplySubcommandSyntaxError, for a GET it cannot read.
			sub := args[0]
			if len(sub) > 128 {
				sub = sub[:128]
			}
			return e.encode(fmt.Errorf("ERR unknown subcommand or wrong number of arguments for '%s'. Try SLOWLOG HELP.", sub), false)
		}
		count := 10
		if len(args) == 2 {
			// Read as Redis's string2ll reads it. Redis gives the one message for
			// a count it cannot read and one out of its range
			// (getRangeLongFromObjectOrReply with its own message).
			n, valid := redisInteger(args[1])
			if !valid || n < -1 {
				return e.encode(errSlowlogCount, false)
			}
			count = int(min(n, int64(len(l.entries))))
			if n == -1 {
				count = len(l.entries)
			}
		}
		count = min(count, len(l.entries))
		out := make([]interface{}, 0, count)
		for _, entry := range l.entries[:count] {
			args := make([]interface{}, len(entry.args))
			for j, a := range entry.args {
				args[j] = a
			}
			out = append(out, []interface{}{entry.id, entry.time, int64(entry.duration), args, entry.peer, entry.name,
				int64(entry.argc)})
		}
		return e.encode(out, false)
	}
	return slowlogHelp
}
