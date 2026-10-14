package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/brandopakel/keel/internal/core"
)

// Commands about the connection rather than the data.
//
// HELLO, CLIENT and QUIT are answered here, next to AUTH, because what they
// read and change - who the client is, what it is called, whether to hang up -
// belongs to one connection, and the command table below this layer knows
// nothing about connections. Every current client library sends at least one
// of them while connecting, and without them a library either fails to connect
// or fails the first time an application sets a client name.

var (
	errHelloNotAuthenticated = errors.New("NOAUTH HELLO must be called with the client already authenticated, " +
		"otherwise the HELLO <proto> AUTH <user> <pass> option can be used to authenticate the client and " +
		"select the RESP protocol version at the same time")
	errWrongPass       = errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	errNoPassword      = errors.New("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
	errNoAuth          = errors.New("NOAUTH Authentication required.")
	errSyntax          = errors.New("ERR syntax error")
	errInvalidName     = errors.New("ERR Client names cannot contain spaces, newlines or special characters.")
	errProtocolVersion = errors.New("ERR Protocol version is not an integer or out of range")
	// NOPROTO is the answer to a protocol version this server does not speak,
	// and the one clients test for before falling back: ioredis checks for it
	// by name, and go-redis falls back on any error. Answering "unknown
	// command" works for a client that is not logging in at the same time;
	// answering NOAUTH makes ioredis give up.
	errNoProto = errors.New("NOPROTO unsupported protocol version")
)

// serverPassword is the password the server's clients log in with, which
// CONFIG SET requirepass changes while it runs. The transport's goroutines
// read it as the engine's sets it, so it is held atomically.
type serverPassword struct{ v atomic.Pointer[string] }

func newServerPassword(password string) *serverPassword {
	p := &serverPassword{}
	p.set(password)
	return p
}

func (p *serverPassword) set(password string) { p.v.Store(&password) }

func (p *serverPassword) get() string { return *p.v.Load() }

// requiredPassword is the password c logs in with: the server's now, as Redis
// checks a client against its default user's password as it is now; empty,
// none.
func (c *client) requiredPassword() string {
	if c.serverPass != nil {
		return c.serverPass.get()
	}
	return c.password
}

// authenticate checks a user and password the way Redis checks them for its
// default user, for AUTH and for HELLO's AUTH option, so the two cannot drift
// apart. There is one user, default. Without a password configured it takes
// any password, as Redis's nopass default user does; with one, only that.
// A failed attempt leaves the connection as it was, logged in or not, as it
// does in Redis.
func (c *client) authenticate(user, password string) error {
	if user != "default" {
		return errWrongPass
	}
	required := c.requiredPassword()
	if required == "" {
		c.authenticated = true
		return nil
	}
	got, want := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(required))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return errWrongPass
	}
	c.authenticated = true
	return nil
}

// auth implements AUTH [username] password, as Redis answers each form.
func (c *client) auth(args []string, w io.Writer) {
	user := "default"
	switch {
	case len(args) > 2:
		responseError(errSyntax, w)
		return
	case len(args) == 2:
		user = args[0]
	case c.requiredPassword() == "":
		// The one-argument form names no user, and Redis refuses it when
		// the default user has no password rather than accept anything.
		responseError(errNoPassword, w)
		return
	}
	if err := c.authenticate(user, args[len(args)-1]); err != nil {
		responseError(err, w)
		return
	}
	w.Write([]byte("+OK\r\n"))
}

// hello implements HELLO [protover [AUTH username password] [SETNAME name]].
//
// HELLO 3 switches the connection to RESP3 and HELLO 2 back to RESP2; without
// a version HELLO reports the protocol in use. The order is Redis's, and it
// matters to clients. The version is checked first, so a client asking for
// one this server does not speak learns NOPROTO before anything about its
// password and can fall back. Only then are the credentials tried, and only an
// authenticated connection gets the reply or a new protocol: a HELLO that
// fails leaves the connection on the protocol it had.
//
// The reply is in the protocol just chosen, which is how a client knows the
// switch took: a map for RESP3, and for RESP2 the same map flattened into an
// array, as Redis sends it.
func (c *client) hello(args []string, w io.Writer) {
	resp3 := c.resp3
	if len(args) > 0 {
		version, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			responseError(errProtocolVersion, w)
			return
		}
		if version != 2 && version != 3 {
			responseError(errNoProto, w)
			return
		}
		resp3 = version == 3
	}
	var user, password, name string
	var withAuth, withName bool
	for i := 1; i < len(args); i++ {
		switch remaining := len(args) - 1 - i; {
		case strings.EqualFold(args[i], "AUTH") && remaining >= 2:
			user, password, withAuth = args[i+1], args[i+2], true
			i += 2
		case strings.EqualFold(args[i], "SETNAME") && remaining >= 1:
			if !validClientName(args[i+1]) {
				responseError(errInvalidName, w)
				return
			}
			name, withName = args[i+1], true
			i++
		default:
			responseError(fmt.Errorf("ERR Syntax error in HELLO option '%s'", core.EchoArgument(args[i])), w)
			return
		}
	}
	if withAuth {
		if err := c.authenticate(user, password); err != nil {
			responseError(err, w)
			return
		}
	}
	if c.requiredPassword() != "" && !c.authenticated {
		responseError(errHelloNotAuthenticated, w)
		return
	}
	if withName {
		c.name = name
	}
	c.resp3 = resp3
	role := "master"
	if c.engine.Configuration().ReplicaOf != "" {
		role = "replica"
	}
	w.Write(c.encode(core.ReplyMap{
		"server", "keel",
		"version", core.RedisCompatibleVersion,
		"proto", int64(c.protocol()),
		"id", int64(c.id),
		"mode", "standalone",
		"role", role,
		"modules", []interface{}{},
	}, false))
}

// protocol is the RESP version the connection speaks, as HELLO and CLIENT
// INFO report it.
func (c *client) protocol() int {
	if c.resp3 {
		return 3
	}
	return 2
}

// encode frames a reply for this connection's protocol.
func (c *client) encode(value interface{}, isSimpleString bool) []byte {
	return core.EncodeAs(value, isSimpleString, c.resp3)
}

// clientCommand implements Redis's CLIENT subcommands but those built on
// client tracking: ID, SETNAME and GETNAME for applications that name their
// connections; SETINFO, which redis-py, node-redis, ioredis and go-redis all
// send on every connection and ignore the failure of; INFO and LIST, which
// describe connections, and KILL, which closes them (client_list.go); PAUSE
// and UNPAUSE (pause.go); REPLY, NO-EVICT and NO-TOUCH, which set the
// connection's modes; UNBLOCK, GETREDIR and TRACKINGINFO, which answer as
// Redis does for connections with nothing blocked and no tracking; and HELP.
//
// Nothing RESP3 makes possible beyond its reply types is offered: push
// messages, and the TRACKING, CACHING and MAINT_NOTIFICATIONS subcommands
// built on them. A subcommand this server does not have gets the error Redis gives for
// one it does not have - Redis 8.10 answers MAINT_NOTIFICATIONS exactly so -
// and redis-py 8, node-redis 6 and go-redis 9, which send it while connecting
// over RESP3, carry on without it.
//
// respond has checked the subcommand and its count against the command table
// by the time this runs; it checks them again so that it is safe on its own,
// and so each case below has the arguments it names.
func (c *client) clientCommand(cmd *core.Command, w io.Writer) {
	if err := core.CommandError(cmd); err != nil {
		responseError(err, w)
		return
	}
	args := cmd.Args
	switch strings.ToUpper(args[0]) {
	case "ID":
		w.Write(c.encode(int64(c.id), false))
	case "SETNAME":
		if !validClientName(args[1]) {
			responseError(errInvalidName, w)
			return
		}
		c.name = args[1]
		w.Write(c.encode("OK", true))
	case "GETNAME":
		if c.name == "" {
			w.Write(c.encode(nil, false))
			return
		}
		w.Write(c.encode(c.name, false))
	case "SETINFO":
		var field *string
		switch strings.ToUpper(args[1]) {
		case "LIB-NAME":
			field = &c.libName
		case "LIB-VER":
			field = &c.libVersion
		default:
			responseError(fmt.Errorf("ERR Unrecognized option '%s'", core.EchoArgument(args[1])), w)
			return
		}
		if !validClientName(args[2]) {
			responseError(fmt.Errorf("ERR %s cannot contain spaces, newlines or special characters.", core.EchoArgument(args[1])), w)
			return
		}
		*field = args[2]
		w.Write(c.encode("OK", true))
	case "INFO":
		c.clientInfo(w)
	case "LIST":
		c.clientList(args, w)
	case "KILL":
		c.clientKill(args, w)
	case "REPLY":
		c.clientReply(args[1], w)
	case "NO-EVICT", "NO-TOUCH":
		on, ok := onOff(args[1])
		if !ok {
			responseError(errSyntax, w)
			return
		}
		// Keel evicts no connections, so NO-EVICT changes nothing but the
		// flag CLIENT LIST shows; NO-TOUCH is read as each command runs.
		if strings.EqualFold(args[0], "no-evict") {
			c.conn.noEvict = on
		} else {
			c.conn.noTouch = on
		}
		w.Write(c.encode("OK", true))
	case "UNBLOCK":
		c.clientUnblock(args, w)
	case "GETREDIR":
		w.Write(c.encode(int64(-1), false))
	case "TRACKINGINFO":
		c.clientTrackingInfo(w)
	case "PAUSE":
		clientPauseCommand(args, w, c.encode)
	case "UNPAUSE":
		unpauseClients()
		w.Write(c.encode("OK", true))
	case "HELP":
		w.Write(clientHelp)
	}
}

// clientHelp is Redis's CLIENT HELP, for the subcommands this server has.
var clientHelp = core.HelpReply("CLIENT",
	"GETREDIR",
	"    Return the client ID we are redirecting to when tracking is enabled.",
	"GETNAME",
	"    Return the name of the current connection.",
	"ID",
	"    Return the ID of the current connection.",
	"INFO",
	"    Return information about the current client connection.",
	"KILL <ip:port>",
	"    Kill connection made from <ip:port>.",
	"KILL <option> <value> [<option> <value> [...]]",
	"    Kill connections. Options are:",
	"    * ADDR (<ip:port>|<unixsocket>:0)",
	"      Kill connections made from the specified address",
	"    * LADDR (<ip:port>|<unixsocket>:0)",
	"      Kill connections made to specified local address",
	"    * TYPE (NORMAL|MASTER|REPLICA|PUBSUB)",
	"      Kill connections by type.",
	"    * USER <username>",
	"      Kill connections authenticated by <username>.",
	"    * SKIPME (YES|NO)",
	"      Skip killing current connection (default: yes).",
	"    * ID <client-id>",
	"      Kill connections by client id.",
	"    * MAXAGE <maxage>",
	"      Kill connections older than the specified age.",
	"LIST [options ...]",
	"    Return information about client connections. Options:",
	"    * TYPE (NORMAL|MASTER|REPLICA|PUBSUB)",
	"      Return clients of specified type.",
	"UNPAUSE",
	"    Stop the current client pause, resuming traffic.",
	"PAUSE <timeout> [WRITE|ALL]",
	"    Suspend all, or just write, clients for <timeout> milliseconds.",
	"REPLY (ON|OFF|SKIP)",
	"    Control the replies sent to the current connection.",
	"SETNAME <name>",
	"    Assign the name <name> to the current connection.",
	"SETINFO <option> <value>",
	"    Set client meta attr. Options are:",
	"    * LIB-NAME: the client lib name.",
	"    * LIB-VER: the client lib version.",
	"UNBLOCK <clientid> [TIMEOUT|ERROR]",
	"    Unblock the specified blocked client.",
	"TRACKINGINFO",
	"    Report tracking status for the current connection.",
	"NO-EVICT (ON|OFF)",
	"    Protect current client connection from eviction.",
	"NO-TOUCH (ON|OFF)",
	"    Will not touch LRU/LFU stats when this mode is on.")

// validClientName is Redis's rule for a client name and library information:
// printable ASCII with no spaces, because CLIENT LIST output separates fields
// with spaces and lines with newlines. An empty name clears it.
func validClientName(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] < '!' || name[i] > '~' {
			return false
		}
	}
	return true
}

// responseError writes err as a RESP error.
func responseError(err error, w io.Writer) {
	w.Write(core.Encode(err, false))
}
