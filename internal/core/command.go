package core

import "strings"

// Command is one parsed request: the name, upper-cased, and its arguments.
type Command struct {
	Cmd  string
	Args []string
	// Name is the name as the client spelled it, kept only when that differs
	// from Cmd: Redis echoes the spelling it was sent when it refuses a command
	// it does not have. Empty means Cmd is the spelling, which it is for every
	// command the server builds itself.
	Name string
	// RESP3 is whether the connection that sent the command negotiated RESP3
	// with HELLO 3, and so whether its reply is framed in RESP3. The zero value
	// is RESP2, which is what every command that does not come from a client
	// is: log replay, replica apply, and anything the server builds itself.
	// The connection sets it as the command runs rather than as it is parsed,
	// because a HELLO earlier in the same pipeline changes it.
	RESP3 bool
	// Client is the connection that sent the command, which the slow log
	// records when the command is slow; nil for a command no connection sent.
	Client CommandClient
}

// CommandClient is what the slow log records of a connection: its address,
// as Redis writes a peer's (ip:port, or [ip]:port for IPv6), and the name it
// gave itself with CLIENT SETNAME or HELLO.
type CommandClient interface {
	PeerAddr() string
	ClientName() string
}

// sentName is the command's name as the client spelled it.
func (c *Command) sentName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Cmd
}

// parsedCommand is a request whose name arrived as name: upper-cased into
// Cmd, and kept as sent when upper-casing changed it.
func parsedCommand(name string, args []string) *Command {
	cmd := &Command{Cmd: strings.ToUpper(name), Args: args}
	if cmd.Cmd != name {
		cmd.Name = name
	}
	return cmd
}
