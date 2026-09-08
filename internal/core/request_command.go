package core

// The common GET and SET shapes can own their argument headers in the same
// allocation as the command. Both layouts round to the same total size as the
// separate Command and []string allocations, but require one fewer GC object.
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
