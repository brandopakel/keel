package server

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"github.com/brandopakel/keel/internal/resp"
)

// CLIENT LIST, INFO, KILL, REPLY, NO-EVICT, NO-TOUCH, UNBLOCK, GETREDIR and
// TRACKINGINFO, as Redis 8.10.1 has them (networking.c, clientCommand and
// catClientInfoString).
//
// CLIENT LIST and INFO describe a connection in Redis's line, field for
// field and in Redis's order. The fields Keel has no counterpart for read as
// Redis reads them for a connection without the feature: db=0, no
// subscriptions (sub, psub, ssub), no watched keys, no tracking (redir=-1),
// the default user, and io-thread=0, since Keel's I/O threads take whichever
// connections a turn has rather than owning some. The buffers are Keel's own:
// qbuf is what the connection sent that has not all run, obl what it is owed
// and has not been sent, and tot-mem the input and replies it holds and its
// own record; Keel writes a connection's reply from one buffer, so oll, omem
// and the shared and unshared halves of omem are 0, and rbs and rbp are that
// buffer's size now, as Keel keeps no peak. docs/info-compatibility.md lists
// them.
//
// CLIENT KILL marks the connections it kills, as Redis's CLIENT_CLOSE_ASAP
// does, and the loop closes them at the end of the turn (reapKilled): none of
// their commands runs after the kill, and nothing more is sent them. The
// connection that sends it is closed once its reply has gone, as Redis closes
// it after the reply.
//
// The loop's goroutine reads and sets all of it, as commands run there; the
// I/O threads count each connection's reads and writes in their own phase.

// connState is what CLIENT LIST reports of a connection beyond its identity,
// and the modes CLIENT REPLY, NO-EVICT, NO-TOUCH and KILL set.
type connState struct {
	// created is when the connection was accepted (age); laddr its local
	// address, looked up the first time it is asked for.
	created time.Time
	laddr   string
	// lastCmd is the canonical name of the last command it sent, and lastSub
	// a container's subcommand as it was sent (cmd).
	lastCmd, lastSub string
	// Counted as Redis counts them: the bytes read from and written to the
	// connection, the commands each read parsed and how many reads parsed
	// any, and the commands that ran (tot-cmds). Its reads are the client's
	// own count, which the I/O threads' stats read too (read-events).
	netIn, netOut          uint64
	pipelineSum, pipelines uint64
	commands               uint64
	// held is the size of the commands the last read parsed, while they have
	// not all run: Redis's query buffer holds them until then (qbuf). next
	// is the place in cmds of the command running (argv-mem).
	held, next int
	// While one of its commands runs: what the commands before it in the run
	// have staged and what it has written so far, which Redis would hold in
	// the connection's reply buffer (obl), and the count of commands run when
	// it began, so that what EXEC has run so far counts (tot-cmds).
	staged  int
	capture *captureWriter
	ranAt   uint64
	// exec is the transaction EXEC is running, which CLIENT LIST reports as
	// Redis does, though the connection has set it aside (AnswerConnection).
	exec *core.Transaction
	// The modes: replies dropped until CLIENT REPLY ON (replyOff), dropped
	// for the command running (replySkip) or the next (replySkipNext);
	// CLIENT NO-EVICT and NO-TOUCH; and killed by CLIENT KILL.
	replyOff, replySkip, replySkipNext bool
	noEvict, noTouch, killed           bool
}

// clientStructBytes is what a connection's own record takes, for tot-mem.
const clientStructBytes = int(unsafe.Sizeof(client{}))

// The errors of CLIENT LIST, KILL and UNBLOCK, Redis's words.
var (
	errInvalidClientID   = errors.New("ERR Invalid client ID")
	errClientIDRange     = errors.New("ERR client-id should be greater than 0")
	errMaxAgeNotInteger  = errors.New("ERR maxage is not an integer or out of range")
	errMaxAgeRange       = errors.New("ERR maxage should be greater than 0")
	errNoSuchClient      = errors.New("ERR No such client")
	errUnblockReason     = errors.New("ERR CLIENT UNBLOCK reason should be TIMEOUT or ERROR")
	errValueNotAnInteger = errors.New("ERR value is not an integer or out of range")
)

// The client types CLIENT LIST TYPE and CLIENT KILL TYPE name (getClientType):
// Keel's connections are normal ones, and replicas once they pull the log.
// Keel has no subscriptions, and a replica's link to its primary is not one
// of the server's connections, so none is of type pubsub or master.
const (
	clientNormal = iota
	clientReplica
	clientPubSub
	clientMaster
)

// clientTypeByName is getClientTypeByName: -1 for a name it does not know.
func clientTypeByName(name string) int {
	switch strings.ToLower(name) {
	case "normal":
		return clientNormal
	case "slave", "replica":
		return clientReplica
	case "pubsub":
		return clientPubSub
	case "master":
		return clientMaster
	}
	return -1
}

// clientType is getClientType.
func (c *client) clientType() int {
	if c.replica {
		return clientReplica
	}
	return clientNormal
}

// errUnknownClientType is Redis's refusal of a type it does not know.
func errUnknownClientType(name string) error {
	return fmt.Errorf("ERR Unknown client type '%s'", core.EchoArgument(name))
}

// localAddr is the connection's own address, as Redis's getClientSockname
// writes it: looked up once and kept.
func (c *client) localAddr() string {
	if c.conn.laddr == "" {
		if sa, err := syscall.Getsockname(c.fd); err == nil {
			c.conn.laddr = peerAddr(sa)
		}
		if c.conn.laddr == "" {
			c.conn.laddr = "?:0"
		}
	}
	return c.conn.laddr
}

// noteCommand records cmd as the last command c sent, as Redis's
// processCommand sets lastcmd before it runs or queues it.
func (c *client) noteCommand(cmd *core.Command) {
	c.conn.lastCmd, c.conn.lastSub = cmd.Cmd, ""
	if len(cmd.Args) > 0 && core.HasSubcommands(cmd.Cmd) {
		c.conn.lastSub = cmd.Args[0]
	}
}

// infoLine appends catClientInfoString's line for c to b, as at now. running
// is the connection whose command asks.
func (c *client) infoLine(b []byte, now time.Time, running *client) []byte {
	flags := make([]byte, 0, 8)
	if c.replica {
		flags = append(flags, 'S')
	}
	tx := c.tx
	if tx == nil {
		tx = c.conn.exec
	}
	if tx != nil {
		flags = append(flags, 'x')
	}
	if c.postponed {
		flags = append(flags, 'b')
	}
	if c.closeAfterWrite {
		flags = append(flags, 'c')
	}
	if c.conn.killed {
		flags = append(flags, 'A')
	}
	if c.conn.noEvict {
		flags = append(flags, 'e')
	}
	if c.conn.noTouch {
		flags = append(flags, 'T')
	}
	if len(flags) == 0 {
		flags = append(flags, 'N')
	}
	events := ""
	switch c.interest {
	case io_multiplexing.OpRead:
		events = "r"
	case io_multiplexing.OpWrite:
		events = "w"
	}
	multi := -1
	if tx != nil {
		multi = tx.Len()
	}
	qbuf, qbufFree := c.conn.held, 0
	if c.buf != nil {
		qbuf += c.buf.size()
		qbufFree = cap(c.buf.data) - len(c.buf.data)
	} else if qbuf > 0 {
		qbufFree = max(readChunkSize-qbuf, 0)
	}
	argvMem := 0
	if c.conn.exec != nil {
		// Redis's EXEC holds every queued command's arguments as its own.
		argvMem = c.conn.exec.ArgvBytes()
	} else if c.conn.next < len(c.cmds) {
		for _, cmd := range c.cmds[c.conn.next:] {
			argvMem += len(cmd.Cmd)
			for _, a := range cmd.Args {
				argvMem += len(a)
			}
		}
	}
	obl, commands := len(c.out), c.conn.commands
	if c == running {
		obl = c.conn.staged
		if c.conn.capture != nil {
			obl += len(c.conn.capture.p)
		}
		if c.conn.exec != nil {
			obl += c.engine.ExecReplyBytes()
		}
		commands += c.engine.CommandsProcessed() - c.conn.ranAt
	}
	cmd := "NULL"
	if c.conn.lastCmd != "" {
		if name := core.CommandFullName(c.conn.lastCmd, c.conn.lastSub); name != "" {
			cmd = name
		}
	}
	b = fmt.Appendf(b, "id=%d addr=%s laddr=%s fd=%d name=%s age=%d idle=%d flags=%s db=0 sub=0 psub=0 ssub=0 multi=%d watch=0 "+
		"qbuf=%d qbuf-free=%d argv-mem=%d multi-mem=%d rbs=%d rbp=%d obl=%d oll=0 omem=0 omem-shared=0 omem-unshared=0 "+
		"tot-mem=%d events=%s cmd=%s user=default redir=-1 resp=%d lib-name=%s lib-ver=%s io-thread=0 "+
		"tot-net-in=%d tot-net-out=%d tot-cmds=%d read-events=%d avg-pipeline-len-sum=%d avg-pipeline-len-cnt=%d",
		c.id, c.addr, c.localAddr(), c.fd, c.name, now.Unix()-c.conn.created.Unix(), now.Unix()-c.lastProgress.Unix(),
		flags, multi, qbuf, qbufFree, argvMem, tx.ArgvBytes(), cap(c.out), cap(c.out), obl,
		c.accountedInput+c.accountedReply+clientStructBytes, events, cmd, c.protocol(), c.libName, c.libVersion,
		c.conn.netIn, c.conn.netOut, commands, c.reads, c.conn.pipelineSum, c.conn.pipelines)
	return b
}

// connectedInOrder is every connection, in the order they connected, which is
// the order Redis lists its clients in.
func connectedInOrder() []*client {
	all := make([]*client, 0, len(clients))
	for _, c := range clients {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	return all
}

// clientByID is lookupClientByID: the connection with id, or nil.
func clientByID(id uint64) *client {
	for _, c := range clients {
		if c.id == id {
			return c
		}
	}
	return nil
}

// clientInfo is CLIENT INFO: this connection's line.
func (c *client) clientInfo(w io.Writer) {
	line := c.infoLine(nil, time.Now(), c)
	w.Write(c.encode(core.ReplyVerbatim(string(append(line, '\n'))), false))
}

// clientList is CLIENT LIST [TYPE type | ID id [id ...]], as Redis reads its
// options: TYPE with exactly one argument, ID with one or more, and any
// other form a syntax error.
func (c *client) clientList(args []string, w io.Writer) {
	now := time.Now()
	var out []byte
	switch {
	case len(args) == 3 && strings.EqualFold(args[1], "type"):
		kind := clientTypeByName(args[2])
		if kind == -1 {
			responseError(errUnknownClientType(args[2]), w)
			return
		}
		for _, other := range connectedInOrder() {
			if other.clientType() == kind {
				out = append(other.infoLine(out, now, c), '\n')
			}
		}
	case len(args) > 2 && strings.EqualFold(args[1], "id"):
		ids := make([]uint64, 0, len(args)-2)
		for _, a := range args[2:] {
			id, ok := redisInt(a)
			if !ok {
				responseError(errInvalidClientID, w)
				return
			}
			ids = append(ids, uint64(id))
		}
		for _, id := range ids {
			if other := clientByID(id); other != nil {
				out = append(other.infoLine(out, now, c), '\n')
			}
		}
	case len(args) != 1:
		responseError(errSyntax, w)
		return
	default:
		for _, other := range connectedInOrder() {
			out = append(other.infoLine(out, now, c), '\n')
		}
	}
	w.Write(c.encode(core.ReplyVerbatim(string(out)), false))
}

// clientKill is CLIENT KILL addr, Redis's old form, which answers OK or that
// there is no such client and may kill the connection that sends it, and
// CLIENT KILL with filters, which answers how many it killed and spares the
// sender unless SKIPME no. A filter's value is read as Redis reads it, and
// a connection is killed if it passes every filter given.
func (c *client) clientKill(args []string, w io.Writer) {
	var (
		addr, laddr       string
		hasAddr, hasLaddr bool
		kind              = -1
		id                uint64
		maxAge            int64
		skipMe            = true
	)
	if len(args) == 2 {
		addr, hasAddr, skipMe = args[1], true, false
	} else {
		for i := 1; i < len(args); i += 2 {
			more := i+1 < len(args)
			option := args[i]
			switch {
			case more && strings.EqualFold(option, "id"):
				v, ok := redisInt(args[i+1])
				if !ok || v < 1 {
					responseError(errClientIDRange, w)
					return
				}
				id = uint64(v)
			case more && strings.EqualFold(option, "maxage"):
				v, ok := redisInt(args[i+1])
				if !ok {
					responseError(errMaxAgeNotInteger, w)
					return
				}
				if v <= 0 {
					responseError(errMaxAgeRange, w)
					return
				}
				maxAge = v
			case more && strings.EqualFold(option, "type"):
				if kind = clientTypeByName(args[i+1]); kind == -1 {
					responseError(errUnknownClientType(args[i+1]), w)
					return
				}
			case more && strings.EqualFold(option, "addr"):
				addr, hasAddr = args[i+1], true
			case more && strings.EqualFold(option, "laddr"):
				laddr, hasLaddr = args[i+1], true
			case more && strings.EqualFold(option, "user"):
				// Keel has one user, Redis's default, which every connection is.
				if args[i+1] != "default" {
					responseError(fmt.Errorf("ERR No such user '%s'", core.EchoArgument(args[i+1])), w)
					return
				}
			case more && strings.EqualFold(option, "skipme"):
				switch {
				case strings.EqualFold(args[i+1], "yes"):
					skipMe = true
				case strings.EqualFold(args[i+1], "no"):
					skipMe = false
				default:
					responseError(errSyntax, w)
					return
				}
			default:
				responseError(errSyntax, w)
				return
			}
		}
	}
	now := time.Now().Unix()
	killed, killSelf := 0, false
	for _, other := range connectedInOrder() {
		switch {
		case other.conn.killed:
			continue
		case hasAddr && other.addr != addr:
			continue
		case hasLaddr && other.localAddr() != laddr:
			continue
		case kind != -1 && other.clientType() != kind:
			continue
		case id != 0 && other.id != id:
			continue
		case other == c && skipMe:
			continue
		case maxAge != 0 && now-other.conn.created.Unix() < maxAge:
			continue
		}
		if other == c {
			killSelf = true
		} else {
			other.conn.killed = true
			killedClients = append(killedClients, other)
		}
		killed++
	}
	if len(args) == 2 {
		if killed == 0 {
			responseError(errNoSuchClient, w)
		} else {
			w.Write(c.encode("OK", true))
		}
	} else {
		w.Write(c.encode(int64(killed), false))
	}
	if killSelf {
		c.closeAfterWrite = true
	}
}

// killedClients are the connections CLIENT KILL killed this turn, which
// reapKilled closes once the turn's replies are out.
var killedClients []*client

// dropKilled leaves out of cs the connections CLIENT KILL killed, which are
// sent nothing more.
func dropKilled(cs []*client) []*client {
	if len(killedClients) == 0 {
		return cs
	}
	live := cs[:0]
	for _, c := range cs {
		if !c.conn.killed {
			live = append(live, c)
		}
	}
	clear(cs[len(live):])
	return live
}

// reapKilled closes the connections CLIENT KILL killed, and tells forget of
// each, as sweepStalledClients closes the connections it gives up on.
func reapKilled(forget func(*client)) {
	for _, c := range killedClients {
		if clients[c.fd] != c {
			continue
		}
		closeClient(c)
		if forget != nil {
			forget(c)
		}
	}
	clear(killedClients)
	killedClients = killedClients[:0]
}

// onOff is a mode's ON or OFF, case aside; ok is false for anything else.
func onOff(arg string) (on, ok bool) {
	switch {
	case strings.EqualFold(arg, "on"):
		return true, true
	case strings.EqualFold(arg, "off"):
		return false, true
	}
	return false, false
}

// clientReply is CLIENT REPLY ON|OFF|SKIP. OFF and SKIP are not answered,
// since what they turn off includes their own reply.
func (c *client) clientReply(mode string, w io.Writer) {
	switch {
	case strings.EqualFold(mode, "on"):
		c.conn.replyOff, c.conn.replySkip = false, false
		w.Write(c.encode("OK", true))
	case strings.EqualFold(mode, "off"):
		c.conn.replyOff = true
	case strings.EqualFold(mode, "skip"):
		if !c.conn.replyOff {
			c.conn.replySkipNext = true
		}
	default:
		responseError(errSyntax, w)
	}
}

// gateReply applies CLIENT REPLY to the reply of the command that just ran,
// as Redis's addReply and resetClient do: dropped while replies are off or
// this command's is skipped, and a SKIP just given skips the next. silenced
// reports a command answered with nothing on purpose, so that executeRun
// does not take it for one that failed to answer.
func (c *client) gateReply(w *captureWriter) (silenced bool) {
	silence := c.conn.replyOff || c.conn.replySkip
	c.conn.replySkip = false
	if c.conn.replySkipNext {
		c.conn.replySkip, c.conn.replySkipNext = true, false
	}
	if silence {
		w.p = nil
		return true
	}
	return len(w.p) == 0
}

// run runs one of c's commands into w, as executeRun runs each: noted as the
// last command the connection sent, counted toward its tot-cmds, reading
// keys without touching them under CLIENT NO-TOUCH, and answered as CLIENT
// REPLY says. What EXEC runs is not noted, as Redis's execCommand leaves
// lastcmd EXEC. Keel has no TOUCH, the
// one command NO-TOUCH leaves alone in Redis.
func (c *client) run(cmd *core.Command, w *captureWriter) (silenced bool) {
	c.noteCommand(cmd)
	ran := c.engine.CommandsProcessed()
	c.conn.ranAt, c.conn.capture = ran, w
	noTouch := c.conn.noTouch
	if noTouch {
		c.engine.SetNoTouch(true)
	}
	respond(c, cmd, w)
	if noTouch {
		c.engine.SetNoTouch(false)
	}
	c.conn.capture = nil
	c.conn.commands += c.engine.CommandsProcessed() - ran
	if c.conn.replyOff || c.conn.replySkip || c.conn.replySkipNext {
		return c.gateReply(w)
	}
	return false
}

// clientUnblock is CLIENT UNBLOCK id [TIMEOUT|ERROR]. Keel has no blocking
// commands, and Redis never unblocks a connection CLIENT PAUSE holds, so no
// connection can be unblocked: the reason and the ID are checked, in Redis's
// order, and the answer is 0.
func (c *client) clientUnblock(args []string, w io.Writer) {
	if len(args) > 3 {
		responseError(subcommandSyntaxError("CLIENT", args[0]), w)
		return
	}
	if len(args) == 3 && !strings.EqualFold(args[2], "timeout") && !strings.EqualFold(args[2], "error") {
		responseError(errUnblockReason, w)
		return
	}
	if _, ok := redisInt(args[1]); !ok {
		responseError(errValueNotAnInteger, w)
		return
	}
	w.Write(c.encode(int64(0), false))
}

// clientTrackingInfo is CLIENT TRACKINGINFO for a connection without
// tracking, which every Keel connection is: flags off, no redirect, no
// prefixes.
func (c *client) clientTrackingInfo(w io.Writer) {
	f := resp.Framing{RESP3: c.resp3}
	b := f.AppendMapHeader(nil, 3)
	b = resp.AppendBulkString(b, "flags")
	b = f.AppendSetHeader(b, 1)
	b = resp.AppendBulkString(b, "off")
	b = resp.AppendBulkString(b, "redirect")
	b = resp.AppendInteger(b, -1)
	b = resp.AppendBulkString(b, "prefixes")
	b = resp.AppendArrayHeader(b, 0)
	w.Write(b)
}
