package server

import (
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
)

// CLIENT PAUSE and UNPAUSE, as Redis 8.10.1 has them (networking.c,
// pauseClientsByClient and processCommand's check): until a time, every
// command a connection sends, or only those that write or may be replicated,
// waits. A connection whose next command waits is postponed, as Redis blocks
// it (BLOCKED_POSTPONE): its commands run no further and its input is not
// read until the pause ends or eases, and it counts in INFO's
// blocked_clients. The commands it sent before that one have run and been
// answered. A command Redis refuses before it pauses - one it does not have,
// the wrong number of arguments, NOAUTH - is refused at once, and a
// replica's pull is never paused, as Redis never pauses its replicas.
//
// While the pause lasts, active expiry is paused too, as Redis pauses it.
// Redis also stops a read from deleting a key whose expiry has passed (it
// reads as gone but stays); Keel still deletes it, which docs/info-
// compatibility.md lists.
//
// The loop's goroutine reads and sets all of it: commands, CLIENT PAUSE
// among them, run there.

// clientPause is the pause CLIENT PAUSE set: until when, and whether every
// command waits or only those that write. A zero end is no pause.
type clientPause struct {
	end time.Time
	all bool
}

var (
	pause clientPause
	// postponed are the connections waiting out the pause, in the order
	// their commands were held.
	postponed []*client
	// pauseEased is set when the pause ended or loosened, so the postponed
	// connections are tried again.
	pauseEased bool
)

// The errors of CLIENT PAUSE, Redis's words.
var (
	errPauseMode           = errors.New("ERR CLIENT PAUSE mode must be WRITE or ALL")
	errTimeoutNotAnInteger = errors.New("ERR timeout is not an integer or out of range")
	errTimeoutNegative     = errors.New("ERR timeout is negative")
	errTimeoutOutOfRange   = errors.New("ERR timeout is out of range")
)

// active reports whether the pause holds commands at now.
func (p clientPause) active(now time.Time) bool { return !p.end.IsZero() && now.Before(p.end) }

// updatePause is Redis's updatePausedActions: a pause whose end has passed is
// cleared, and the connections it held are tried again.
func updatePause(now time.Time) {
	if !pause.end.IsZero() && !pause.active(now) {
		pause = clientPause{}
		pauseEased = true
	}
}

// pauseClients is pauseClientsByClient: the later of the two ends, and ALL if
// either asks for it.
func pauseClients(end time.Time, all bool) {
	now := time.Now()
	updatePause(now)
	if pause.active(now) {
		all = all || pause.all
		if pause.end.After(end) {
			end = pause.end
		}
	}
	pause = clientPause{end: end, all: all}
	if !pause.active(now) {
		pause = clientPause{}
	}
}

// unpauseClients is unpauseActions: the pause ends now.
func unpauseClients() {
	if !pause.end.IsZero() {
		pause = clientPause{}
		pauseEased = true
	}
}

// releasePostponed tries the postponed connections again, if the pause eased:
// each is read again and its commands run, and waits again if they must.
func releasePostponed(mux io_multiplexing.IOMultiplexer) {
	if !pauseEased {
		return
	}
	pauseEased = false
	held := postponed
	postponed = nil
	for _, c := range held {
		if clients[c.fd] != c || !c.postponed {
			continue
		}
		c.postponed = false
		if err := c.setInterest(mux, io_multiplexing.OpRead); err != nil {
			closeClient(c)
			continue
		}
		queueClientRead(c)
	}
}

// blockedClients is how many connections wait out the pause.
func blockedClients() int {
	n := 0
	for _, c := range postponed {
		if clients[c.fd] == c && c.postponed {
			n++
		}
	}
	return n
}

// waitsForPause reports whether cmd, the next command c sends, waits out the
// pause, as Redis's processCommand decides it.
func (c *client) waitsForPause(cmd *core.Command) bool {
	if pause.end.IsZero() || !pause.active(time.Now()) {
		return false
	}
	if strings.HasPrefix(cmd.Cmd, "KEEL.REPL.") {
		// A replica's pull: Redis never pauses its replicas.
		return false
	}
	if core.CommandError(cmd) != nil {
		return false
	}
	if c.password != "" && !c.authenticated && cmd.Cmd != "AUTH" && cmd.Cmd != "HELLO" && cmd.Cmd != "QUIT" {
		return false
	}
	if pause.all {
		return true
	}
	if cmd.Cmd == "EXEC" {
		return c.tx.MayReplicate()
	}
	if c.tx != nil && (core.IsTransactionCommand(cmd.Cmd) || cmd.Cmd == "WATCH" || cmd.Cmd == "QUIT") {
		return false
	}
	return core.MayReplicate(cmd)
}

// postpone holds c until the pause eases.
func (c *client) postpone() {
	c.postponed = true
	postponed = append(postponed, c)
}

// clientPauseCommand is CLIENT PAUSE timeout [WRITE|ALL]: the mode first,
// then the timeout, in milliseconds, as Redis reads them.
func clientPauseCommand(args []string, w io.Writer, encode func(interface{}, bool) []byte) {
	if len(args) > 3 {
		// Redis's CLIENT takes PAUSE with one or two arguments and answers
		// any other count as a subcommand it does not know.
		responseError(subcommandSyntaxError("CLIENT", args[0]), w)
		return
	}
	all := true
	if len(args) == 3 {
		switch {
		case strings.EqualFold(args[2], "write"):
			all = false
		case !strings.EqualFold(args[2], "all"):
			responseError(errPauseMode, w)
			return
		}
	}
	ms, ok := redisInt(args[1])
	switch {
	case !ok:
		responseError(errTimeoutNotAnInteger, w)
		return
	case ms < 0:
		responseError(errTimeoutNegative, w)
		return
	}
	var end time.Time
	if ms > 0 {
		now := time.Now().UnixMilli()
		if ms > math.MaxInt64-now {
			responseError(errTimeoutOutOfRange, w)
			return
		}
		end = time.UnixMilli(now + ms)
	}
	pauseClients(end, all)
	w.Write(encode("OK", true))
}

// redisInt reads a signed 64-bit integer as Redis's string2ll does: an
// optional minus sign and decimal digits, with no plus sign, no leading zero
// and no space.
func redisInt(s string) (int64, bool) {
	if s == "0" {
		return 0, true
	}
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || digits[0] < '1' || digits[0] > '9' || len(digits) > 19 {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
		v = v*10 + uint64(digits[i]-'0')
	}
	if len(digits) < len(s) {
		if v > 1<<63 {
			return 0, false
		}
		return -int64(v), true
	}
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

// subcommandSyntaxError is Redis's addReplySubcommandSyntaxError: the
// subcommand as it was sent, as C's %.128s prints it with carriage returns
// and line feeds made spaces, as every error Redis formats is.
func subcommandSyntaxError(container, sub string) error {
	if i := strings.IndexByte(sub, 0); i >= 0 {
		sub = sub[:i]
	}
	if len(sub) > 128 {
		sub = sub[:128]
	}
	sub = strings.NewReplacer("\r", " ", "\n", " ").Replace(sub)
	return errors.New("ERR unknown subcommand or wrong number of arguments for '" + sub + "'. Try " + container + " HELP.")
}
