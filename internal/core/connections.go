package core

import "strings"

// What the transport asks of the engine to describe and steer its
// connections, for CLIENT LIST, CLIENT INFO and CLIENT NO-TOUCH
// (internal/server/client_list.go). The file is named to sort after the
// command handlers, so that it moves none of them in the test binary the
// paired command-path job measures.

// HasSubcommands reports whether name, a command's canonical name, is a
// container whose first argument names a subcommand: CLIENT, CONFIG and the
// rest.
func HasSubcommands(name string) bool { return subcommandsOf(name) != nil }

// CommandFullName is a command's name as CLIENT LIST's cmd= field writes the
// last command a connection sent (catClientInfoString): in lower case, a
// container's subcommand as container|sub. name is the command's canonical
// name and sub the first argument it was sent with, read only for a
// container. It is empty for a command, or a subcommand, Keel does not have,
// which Redis writes as NULL.
func CommandFullName(name, sub string) string {
	if _, ok := commands[name]; !ok {
		return ""
	}
	subs := subcommandsOf(name)
	if subs == nil {
		return strings.ToLower(name)
	}
	for _, s := range subs {
		if strings.EqualFold(s.name, sub) {
			return strings.ToLower(name) + "|" + s.name
		}
	}
	return ""
}

// CommandsProcessed is INFO's total_commands_processed: every command that
// has run, which the transport reads before and after a connection's command
// to count that connection's (CLIENT LIST's tot-cmds).
func (e *Engine) CommandsProcessed() uint64 { return e.totals.commands }

// SetNoTouch sets whether the commands that run from now on leave the keys
// they read as they were for eviction, unaccessed, as CLIENT NO-TOUCH has a
// connection's commands do (lookupKey's LOOKUP_NOTOUCH). The transport sets
// it around each command of a connection in that mode.
func (e *Engine) SetNoTouch(on bool) { e.space.SetNoTouch(on) }

// Len is how many commands the transaction holds queued, CLIENT LIST's
// multi=; none for no transaction.
func (tx *Transaction) Len() int {
	if tx == nil {
		return 0
	}
	return len(tx.commands)
}

// ArgvBytes is the size of the commands the transaction holds queued, their
// names and arguments, as Redis's multi-mem counts them (mstate.argv_len_sums);
// none for no transaction.
func (tx *Transaction) ArgvBytes() int {
	if tx == nil {
		return 0
	}
	n := 0
	for _, cmd := range tx.commands {
		n += len(cmd.Cmd)
		for _, a := range cmd.Args {
			n += len(a)
		}
	}
	return n
}

// ExecReplyBytes is what the reply of the EXEC running has come to before
// the command the connection is answering, its header and the replies of the
// commands before it: what Redis would hold in the connection's reply buffer
// by then (CLIENT LIST's obl).
func (e *Engine) ExecReplyBytes() int { return e.execReplyBytes }
