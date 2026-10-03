package core

// Command is one parsed request: the name, upper-cased, and its arguments.
type Command struct {
	Cmd  string
	Args []string
	// RESP3 is whether the connection that sent the command negotiated RESP3
	// with HELLO 3, and so whether its reply is framed in RESP3. The zero value
	// is RESP2, which is what every command that does not come from a client
	// is: log replay, replica apply, and anything the server builds itself.
	// The connection sets it as the command runs rather than as it is parsed,
	// because a HELLO earlier in the same pipeline changes it.
	RESP3 bool
}
