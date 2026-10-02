package core

// Command is one parsed request: the name, upper-cased, and its arguments.
type Command struct {
	Cmd  string
	Args []string
}
