package core

import (
	"errors"
	"fmt"
	"strings"
)

// What Redis answers about a command before it runs it, worded as Redis words
// it, byte for byte.
//
// Redis looks a command up and counts its arguments before anything else:
// before it asks the connection to log in, before a transaction queues the
// command, before a replica refuses a write. A command it does not have, a
// subcommand a container command does not have and the wrong number of
// arguments are answered there, from its command table, and so they are here:
// CommandError is asked by the transport ahead of AUTH, by a transaction as it
// queues, and by EvalAndResponse ahead of everything else, and all three read
// the one table below. Counting in one place is what keeps a transaction from
// queueing a command that would then be refused at EXEC for its arguments.

// commandArity is how many words, the name included, each command accepts, as
// Redis counts them: n means exactly n, -n means at least n.
//
// The counts are Redis's for Redis's commands and RedisBloom's for the BF, CF
// and CMS commands, as each declares them; Keel's own commands are counted the
// same way. Every command any path can name is here: the command table's, the
// connection commands the transport answers itself, and MULTI, EXEC and
// DISCARD. A handler may check more than its count - an upper bound, or that
// its arguments come in pairs - as Redis's and RedisBloom's commands do, and
// it refuses every count the table refuses, in the same words, which
// TestCommandArityIsNeverStricterThanTheHandler checks.
var commandArity = map[string]int{
	"PING": -1, "ECHO": 2, "SELECT": 2, "UNWATCH": 1,

	"SET": -3, "SETNX": 3, "GET": 2, "INCR": 2, "INCRBY": 3, "DECR": 2, "DECRBY": 3, "MGET": -2, "MSET": -3,
	"SETEX": 4, "PSETEX": 4,
	"LCS": -3,

	"DEL": -2, "UNLINK": -2, "EXISTS": -2, "TYPE": 2, "KEYS": 2, "SCAN": -2,
	"TTL": 2, "PTTL": 2, "EXPIRE": -3, "PEXPIREAT": -3,
	"PEXPIRE": -3, "EXPIREAT": -3, "PERSIST": 2,

	"DBSIZE": 1, "FLUSHDB": -1, "MEMORY": -2, "INFO": -1, "CONFIG": -2, "BGREWRITEAOF": 1,
	"KEEL.DUMP": 2, "KEEL.RESTORE": 3, "MEMKV.DUMP": 2, "MEMKV.RESTORE": 3,
	"KEEL.PROMOTE": 2, "KEEL.FENCE": 2, "KEEL.REPL.PULL": 3, "KEEL.REPL.PULL2": -5,

	"HSET": -4, "HSETNX": 4, "HGET": 3, "HMGET": -3,
	"HDEL": -3, "HEXISTS": 3, "HLEN": 2, "HKEYS": 2,
	"HVALS": 2, "HGETALL": 2, "HINCRBY": 4,

	"LPUSH": -3, "RPUSH": -3, "LPOP": -2, "RPOP": -2,
	"LTRIM": 4, "LLEN": 2, "LINDEX": 3, "LSET": 4, "LRANGE": 4,

	"SADD": -3, "SREM": -3, "SCARD": 2, "SMEMBERS": 2,
	"SISMEMBER": 3, "SMISMEMBER": -3, "SPOP": -2,
	"SRANDMEMBER": -2, "SRAND": -2,

	"ZCOUNT": 4, "ZRANGEBYSCORE": -4, "ZREVRANGEBYSCORE": -4,
	"ZINCRBY": 4, "ZPOPMIN": -2, "ZPOPMAX": -2,
	"ZRANGE": -4, "ZADD": -4, "ZRANK": -3, "ZREM": -3, "ZSCORE": 3, "ZCARD": 2,
	"GEOADD": -5, "GEODIST": -4, "GEOHASH": -2,
	"GEOSEARCH": -7, "GEOPOS": -2,

	"BF.RESERVE": -4, "BF.INFO": -2, "BF.ADD": 3,
	"BF.MADD": -3, "BF.EXISTS": 3, "BF.MEXISTS": -3,
	"CMS.INITBYDIM": 4, "CMS.INITBYPROB": 4,
	"CMS.INCRBY": -4, "CMS.QUERY": -3,
	"MORRIS.INITBYDIM": 4, "MORRIS.INITBYPROB": 4,
	"MORRIS.INCRBY": -4, "MORRIS.QUERY": -3, "MORRIS.INFO": 2,
	"PFADD": -2, "PFCOUNT": -2, "PFMERGE": -2,
	"CF.RESERVE": -3, "CF.ADD": 3, "CF.ADDNX": 3,
	"CF.EXISTS": 3, "CF.MEXISTS": -3, "CF.DEL": 3,
	"CF.COUNT": 3, "CF.INFO": 2,

	// Answered by the transport, not the command table: see connectionCommands.
	"AUTH": -2, "HELLO": -1, "QUIT": -1, "CLIENT": -2,
	// Answered by Transact.
	"MULTI": 1, "EXEC": 1, "DISCARD": 1,
}

// connectionCommands are about the connection rather than the data, and the
// transport answers them itself; the command table below this layer knows
// nothing about connections. A transaction queues them like any other command
// and hands them back to the transport when EXEC reaches them.
var connectionCommands = map[string]bool{"AUTH": true, "HELLO": true, "QUIT": true, "CLIENT": true}

// IsConnectionCommand reports whether the transport answers name itself.
func IsConnectionCommand(name string) bool { return connectionCommands[name] }

// subcommand is one of a container command's subcommands: its name as Redis
// names it in an error, and its arity, counted as Redis counts a subcommand's,
// with the container's name and its own both included.
type subcommand struct {
	name  string
	arity int
}

// The container commands are the ones whose first argument names what they
// do. Redis looks the subcommand up as part of looking the command up, so a
// subcommand it does not have is refused when an unknown command would be,
// and each subcommand has its own count. Only the subcommands Keel answers are
// listed; any other gets the error Redis gives for one it does not have.
var (
	clientSubcommands = []subcommand{{"id", 2}, {"setname", 3}, {"getname", 2}, {"setinfo", 4}, {"info", 2}, {"help", 2}}
	memorySubcommands = []subcommand{{"stats", 2}, {"usage", -3}, {"help", 2}}
	configSubcommands = []subcommand{{"get", -3}, {"set", -4}, {"resetstat", 2}, {"rewrite", 2}, {"help", 2}}
	containerCommands = map[string][]subcommand{"CLIENT": clientSubcommands, "MEMORY": memorySubcommands,
		"CONFIG": configSubcommands}
)

// subcommandsOf is a container command's subcommands, or nil for any other
// command: a switch rather than a lookup, since every command asks.
func subcommandsOf(name string) []subcommand {
	switch name {
	case "CLIENT":
		return clientSubcommands
	case "MEMORY":
		return memorySubcommands
	case "CONFIG":
		return configSubcommands
	}
	return nil
}

// commandEntry is one command as dispatch finds it: the handler from
// commandTable, nil for a command the transport or Transact answers, and the
// count from commandArity. container marks CLIENT, MEMORY and CONFIG, whose count
// depends on the subcommand, and namesItself the one handler that repeats the
// name it was sent as - see Engine.runningName. Both are worked out once,
// here, so that a well-formed command is checked with one comparison of its
// count.
// typed and keys are the type check's reading of the tables in keytype.go -
// see checkKeyTypes - worked out here for the same reason.
type commandEntry struct {
	run         func(*Engine, []string) []byte
	arity       int
	typed       string
	keys        keyRule
	container   bool
	namesItself bool
	// reads is whether the type check counts the keys it looks up as
	// keyspace hits and misses: keyspaceReads.
	reads bool
}

// counted reports whether a command of entry's, given args arguments, needs
// no further look before it runs: neither a container nor the wrong count.
func (e commandEntry) counted(args int) bool {
	return !e.container && arityAccepts(e.arity, args)
}

// commands indexes commandArity and commandTable by name together, so the one
// lookup every command makes finds its count and its handler both. The two
// tables stay the source; this is built from them once, at start.
var commands map[string]commandEntry

func init() { indexCommands() }

func indexCommands() {
	commands = make(map[string]commandEntry, len(commandArity))
	for name, arity := range commandArity {
		commands[name] = commandEntry{run: commandTable[name], arity: arity,
			typed: typedKeyspace(name), keys: keyRuleOf(name),
			container: subcommandsOf(name) != nil, namesItself: name == "GEOSEARCH", reads: keyspaceReads[name]}
	}
}

// CommandError is Redis's refusal of cmd before it runs, or nil: the command
// is unknown, a container's subcommand is unknown, or the count of arguments
// is wrong, checked in that order.
func CommandError(cmd *Command) error {
	entry, known := commands[cmd.Cmd]
	return commandRefusal(cmd, entry, known)
}

// commandRefusal is CommandError for a command already looked up.
func commandRefusal(cmd *Command, entry commandEntry, known bool) error {
	if !known {
		return unknownCommand(cmd)
	}
	arity := entry.arity
	if subcommands := subcommandsOf(cmd.Cmd); subcommands != nil && len(cmd.Args) > 0 {
		for _, sub := range subcommands {
			if strings.EqualFold(cmd.Args[0], sub.name) {
				if !arityAccepts(sub.arity, len(cmd.Args)) {
					return wrongArguments(cmd.Cmd + "|" + sub.name)
				}
				return nil
			}
		}
		return unknownSubcommand(cmd)
	}
	if !arityAccepts(arity, len(cmd.Args)) {
		return wrongArguments(cmd.Cmd)
	}
	return nil
}

// arityAccepts reports whether a command of arity can be given args arguments,
// its name not included.
func arityAccepts(arity, args int) bool {
	if arity >= 0 {
		return args+1 == arity
	}
	return args+1 >= -arity
}

// wrongArguments is Redis's refusal of a count of arguments, naming the
// command as its command table does: in lower case, and a subcommand as
// container|subcommand. Every arity error in the server is this one.
func wrongArguments(name string) error {
	return fmt.Errorf("ERR wrong number of arguments for '%s' command", strings.ToLower(name))
}

// Refusal is the reply to cmd refused before it ran. EXEC is the exception,
// as it is in Redis: a refused EXEC discards the transaction it would have
// run, if there is one, and names the reason with EXECABORT.
func Refusal(cmd *Command, err error) []byte {
	if cmd.Cmd == "EXEC" {
		return execAbort(err)
	}
	return Encode(err, false)
}

// echoLimit is how much of a client's own bytes Redis repeats in the errors
// that name what the client sent, its %.128s.
const echoLimit = 128

// appendEcho appends what C's %.*s prints of s with a precision of limit: s up
// to its first NUL byte, where a C string ends, and at most limit bytes of it.
// Carriage returns and line feeds become spaces, as Redis maps them in every
// error so a client's bytes cannot end the reply early. No more of s is read
// than limit bytes, however long s is: an unknown command's name or argument
// can be as large as the query buffer, and the error never copies it.
func appendEcho(dst []byte, s string, limit int) []byte {
	if len(s) > limit {
		s = s[:limit]
	}
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' {
			c = ' '
		}
		dst = append(dst, c)
	}
	return dst
}

// argumentEchoLimit bounds the errors in which Redis repeats a client's
// argument whole, with its %s. Up to it the echo is Redis's byte for byte;
// past it the echo is cut. Redis never takes a longer argument from a
// connection that has not logged in - it is a protocol error there - and the
// cut keeps such an error from copying an argument as large as the query
// buffer after the request has been admitted.
const argumentEchoLimit = 16 << 10

// EchoArgument is s as C's %s prints it, up to its first NUL byte, where a C
// string ends, and no more than argumentEchoLimit bytes of it.
func EchoArgument(s string) string {
	if len(s) > argumentEchoLimit {
		s = s[:argumentEchoLimit]
	}
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

// unknownCommand is Redis's error for a command it does not have: the name as
// it was sent, at most echoLimit bytes of it, and then the arguments while what
// has been written of them is under echoLimit bytes, each cut to the room
// left. A command sent with no arguments is named alone.
func unknownCommand(cmd *Command) error {
	b := append(make([]byte, 0, 64+2*echoLimit+8), "ERR unknown command '"...)
	b = appendEcho(b, cmd.sentName(), echoLimit)
	b = append(b, '\'')
	if len(cmd.Args) > 0 {
		b = append(b, ", with args beginning with: "...)
		start := len(b)
		for _, arg := range cmd.Args {
			written := len(b) - start
			if written >= echoLimit {
				break
			}
			b = append(b, '\'')
			b = appendEcho(b, arg, echoLimit-written)
			b = append(b, "' "...)
		}
	}
	return errors.New(string(b))
}

// unknownSubcommand is Redis's error for a container command's subcommand it
// does not have, which points at the container's HELP.
func unknownSubcommand(cmd *Command) error {
	b := append(make([]byte, 0, 48+echoLimit+len(cmd.Cmd)), "ERR unknown subcommand '"...)
	b = appendEcho(b, cmd.Args[0], echoLimit)
	b = append(b, "'. Try "...)
	b = append(b, cmd.Cmd...)
	b = append(b, " HELP."...)
	return errors.New(string(b))
}

// HelpReply is Redis's answer to a container's HELP: a header naming the
// container, the lines describing each subcommand, and HELP itself last, each
// line a simple string in either protocol. lines are the subcommands' own, in
// Redis's words, for the subcommands this server has.
func HelpReply(container string, lines ...string) []byte {
	lines = append([]string{container + " <subcommand> [<arg> [value] [opt] ...]. Subcommands are:"},
		append(lines, "HELP", "    Print this help.")...)
	b := appendArrayHeader(nil, len(lines))
	for _, line := range lines {
		b = appendSimpleString(b, line)
	}
	return b
}
