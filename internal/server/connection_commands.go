package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/config"
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
	errWrongPass       = errors.New("WRONGPASS invalid username-password pair")
	errInvalidName     = errors.New("ERR Client names cannot contain spaces, newlines or special characters.")
	errProtocolVersion = errors.New("ERR Protocol version is not an integer or out of range")
	// NOPROTO is the answer clients test for before falling back to RESP2:
	// ioredis checks for it by name, and go-redis falls back on any error.
	// Answering "unknown command" works for a client that is not logging in
	// at the same time; answering NOAUTH, which is what a password used to
	// produce, makes ioredis give up.
	errNoProto = errors.New("NOPROTO unsupported protocol version")
)

// authenticate checks a password the way AUTH does, for AUTH and for HELLO's
// AUTH option, so the two cannot drift apart.
func (c *client) authenticate(password string) error {
	if config.RequirePass == "" {
		return errors.New("ERR AUTH called without a configured password")
	}
	got, want := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(config.RequirePass))
	c.authenticated = subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if !c.authenticated {
		return errWrongPass
	}
	return nil
}

// hello implements HELLO [protover [AUTH username password] [SETNAME name]].
//
// The order is Redis's, and it matters to clients. The protocol version is
// checked first, so a client asking for RESP3 learns NOPROTO before anything
// about its password and can fall back to RESP2 and AUTH. Only then are the
// credentials tried, and only an authenticated connection gets the reply.
func (c *client) hello(args []string, w io.Writer) {
	if len(args) > 0 {
		version, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			responseError(errProtocolVersion, w)
			return
		}
		if version != 2 {
			responseError(errNoProto, w)
			return
		}
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
			responseError(fmt.Errorf("ERR Syntax error in HELLO option '%s'", args[i]), w)
			return
		}
	}
	if withAuth {
		if user != "default" {
			responseError(errWrongPass, w)
			return
		}
		if err := c.authenticate(password); err != nil {
			responseError(err, w)
			return
		}
	}
	if config.RequirePass != "" && !c.authenticated {
		responseError(errHelloNotAuthenticated, w)
		return
	}
	if withName {
		c.name = name
	}
	role := "master"
	if config.ReplicaOf != "" {
		role = "replica"
	}
	// The map RESP3 would send, flattened into an array as Redis does for a
	// RESP2 connection.
	w.Write(core.Encode([]interface{}{
		"server", "keel",
		"version", core.RedisCompatibleVersion,
		"proto", int64(2),
		"id", int64(c.id),
		"mode", "standalone",
		"role", role,
		"modules", []interface{}{},
	}, false))
}

// clientCommand implements the CLIENT subcommands a library sends on its own:
// ID, SETNAME and GETNAME for applications that name their connections, and
// SETINFO, which redis-py, node-redis, ioredis and go-redis all send on every
// connection and ignore the failure of. INFO describes this connection.
// Listing or killing other connections is not offered.
func (c *client) clientCommand(args []string, w io.Writer) {
	if len(args) == 0 {
		responseError(errors.New("ERR wrong number of arguments for 'client' command"), w)
		return
	}
	sub := strings.ToUpper(args[0])
	arity := func(n int) bool {
		if len(args) == n {
			return true
		}
		responseError(fmt.Errorf("ERR wrong number of arguments for 'client|%s' command", strings.ToLower(sub)), w)
		return false
	}
	switch sub {
	case "ID":
		if arity(1) {
			w.Write(core.Encode(int64(c.id), false))
		}
	case "SETNAME":
		if !arity(2) {
			return
		}
		if !validClientName(args[1]) {
			responseError(errInvalidName, w)
			return
		}
		c.name = args[1]
		w.Write(core.Encode("OK", true))
	case "GETNAME":
		if !arity(1) {
			return
		}
		if c.name == "" {
			w.Write(core.Encode(nil, false))
			return
		}
		w.Write(core.Encode(c.name, false))
	case "SETINFO":
		if !arity(3) {
			return
		}
		var field *string
		switch strings.ToUpper(args[1]) {
		case "LIB-NAME":
			field = &c.libName
		case "LIB-VER":
			field = &c.libVersion
		default:
			responseError(fmt.Errorf("ERR Unrecognized option '%.128s'", args[1]), w)
			return
		}
		if !validClientName(args[2]) {
			responseError(fmt.Errorf("ERR %s cannot contain spaces, newlines or special characters.", args[1]), w)
			return
		}
		*field = args[2]
		w.Write(core.Encode("OK", true))
	case "INFO":
		if arity(1) {
			w.Write(core.Encode(fmt.Sprintf("id=%d fd=%d name=%s db=0 resp=2 lib-name=%s lib-ver=%s\n",
				c.id, c.fd, c.name, c.libName, c.libVersion), false))
		}
	default:
		responseError(fmt.Errorf("ERR unknown subcommand '%.128s'. Try CLIENT HELP.", args[0]), w)
	}
}

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
