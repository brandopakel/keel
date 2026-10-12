package resp

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
}

// SentName is the command's name as the client spelled it.
func (c *Command) SentName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Cmd
}

// ParsedCommand is a request whose name arrived as name: upper-cased into
// Cmd, and kept as sent when upper-casing changed it.
func ParsedCommand(name string, args []string) *Command {
	cmd := &Command{Cmd: strings.ToUpper(name), Args: args}
	if cmd.Cmd != name {
		cmd.Name = name
	}
	return cmd
}

// The common GET and SET shapes can own their argument headers in the same
// allocation as the command. On supported 64-bit targets both layouts round to
// the same total size as separate Command and []string allocations, but require
// one fewer GC object.
// The returned slice has exact capacity and strings still own decoded bytes.
type decodedCommandOneArg struct {
	command Command
	args    [1]string
}

type decodedCommandTwoArgs struct {
	command Command
	args    [2]string
}

func allocateDecodedCommand(arguments int) *Command {
	switch arguments {
	case 1:
		p := new(decodedCommandOneArg)
		p.command.Args = p.args[:]
		return &p.command
	case 2:
		p := new(decodedCommandTwoArgs)
		p.command.Args = p.args[:]
		return &p.command
	default:
		return &Command{Args: make([]string, arguments)}
	}
}
