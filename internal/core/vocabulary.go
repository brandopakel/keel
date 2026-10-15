package core

import (
	"sort"
	"strings"

	"github.com/brandopakel/keel/internal/resp"
)

// COMMAND: the commands Keel has, as Redis 8.10.1's COMMAND describes its own
// (server.c, commandCommand and the rest), answered from what Redis declares
// of each (command_specs.go, generated from src/commands) and what Keel
// declares of its own (command_specs_keel.go).
//
// COMMAND INFO is Redis's, derived as Redis derives it: the flags its spec
// declares and movablekeys where its key specs leave the keys' positions to
// the arguments (populateCommandLegacyRangeSpec), the ACL categories declared
// and those its flags imply (setImplicitACLCategories), the first key, last
// key and step a client without key specs reads, the tips and the key specs.
// COMMAND DOCS reports the facts of each command and none of Redis's text:
// its summary, complexity, history and replacement, its arguments' summaries
// and its key specs' notes are left out (docs/info-compatibility.md says
// why). COMMAND GETKEYS finds keys as Redis's getKeysFromCommandWithSpecs
// does.
//
// The file is named to sort after the command handlers, so that it moves none
// of them in the test binary the paired command-path job measures.

// commandHelp is Redis's COMMAND HELP, built when asked for, so that it
// adds nothing to the package's init.
func commandHelp() []byte {
	return HelpReply("COMMAND",
		"(no subcommand)",
		"    Return details about all Redis commands.",
		"COUNT",
		"    Return the total number of commands in this Redis server.",
		"LIST",
		"    Return a list of all commands in this Redis server.",
		"INFO [<command-name> ...]",
		"    Return details about multiple Redis commands.",
		"    If no command names are given, documentation details for all",
		"    commands are returned.",
		"DOCS [<command-name> ...]",
		"    Return documentation details about multiple Redis commands.",
		"    If no command names are given, documentation details for all",
		"    commands are returned.",
		"GETKEYS <full-command>",
		"    Return the keys from a full Redis command.",
		"GETKEYSANDFLAGS <full-command>",
		"    Return the keys and the access flags from a full Redis command.")
}

// The errors of COMMAND GETKEYS and GETKEYSANDFLAGS, Redis's words, written
// as constants so that they add nothing to the package's init.
const (
	errGetKeysInvalidCommand   = "ERR Invalid command specified"
	errGetKeysNoKeys           = "ERR The command has no key arguments"
	errGetKeysArity            = "ERR Invalid number of arguments specified for command"
	errGetKeysInvalidArguments = "ERR Invalid arguments specified for command"
)

// commandFlagOrder is COMMAND INFO's flags, in the order Redis writes them
// (addReplyFlagsForCommand), by the names its specs declare them under;
// MAY_REPLICATE, SENTINEL, ONLY_SENTINEL, PROTECTED and
// TOUCHES_ARBITRARY_KEYS are hidden, as Redis hides them.
var commandFlagOrder = []string{"WRITE", "READONLY", "DENYOOM", "MODULE", "ADMIN", "PUBSUB", "NOSCRIPT",
	"BLOCKING", "LOADING", "STALE", "SKIP_MONITOR", "SKIP_SLOWLOG", "ASKING", "FAST", "NO_AUTH",
	"NO_MANDATORY_KEYS", "NO_ASYNC_LOADING", "NO_MULTI", "MOVABLEKEYS", "ALLOW_BUSY", "SCRIPT_RUNNER"}

// aclCategoryOrder is Redis's ACL categories in the order COMMAND INFO
// writes them (acl.c, ACLDefaultCommandCategories), followed by RedisBloom's
// as the module adds them.
var aclCategoryOrder = []string{"KEYSPACE", "READ", "WRITE", "SET", "SORTEDSET", "LIST", "HASH", "STRING",
	"ARRAY", "BITMAP", "HYPERLOGLOG", "GEO", "STREAM", "PUBSUB", "ADMIN", "FAST", "SLOW", "BLOCKING",
	"DANGEROUS", "CONNECTION", "TRANSACTION", "SCRIPTING", "BLOOM", "CUCKOO", "CMS", "TOPK", "TDIGEST"}

// keyFlagOrder is a key spec's flags in the order Redis writes them
// (addReplyFlagsForKeyArgs), with the name it writes each as.
var keyFlagOrder = [][2]string{{"RO", "RO"}, {"RW", "RW"}, {"OW", "OW"}, {"RM", "RM"}, {"ACCESS", "access"},
	{"UPDATE", "update"}, {"INSERT", "insert"}, {"DELETE", "delete"}, {"NOT_KEY", "not_key"},
	{"INCOMPLETE", "incomplete"}, {"VARIABLE_FLAGS", "variable_flags"}}

// redisBloomGroup is whether group is one of the groups of the commands of
// the RedisBloom module, which Redis loads as the module "bf".
func redisBloomGroup(group string) bool {
	switch group {
	case "bf", "cf", "cms":
		return true
	}
	return false
}

// introspected is one command as COMMAND describes it: its name as Redis
// writes it, in lower case and a subcommand as container|sub, its spec, and
// its subcommands, those Keel has.
type introspected struct {
	name  string
	spec  commandSpec
	facts commandFacts
	subs  []introspected
}

// introspect is a command Keel has, by its name in any case, with its
// subcommands; a subcommand as container|sub. ok is false for one it does not
// have.
func introspect(name string) (introspected, bool) {
	upper := strings.ToUpper(name)
	container, sub, isSub := strings.Cut(upper, "|")
	if _, ok := commandArity[container]; !ok {
		return introspected{}, false
	}
	if isSub {
		for _, s := range subcommandsOf(container) {
			if strings.EqualFold(s.name, sub) {
				full := container + "|" + strings.ToUpper(s.name)
				spec, ok := commandSpecOf(full)
				return introspected{name: strings.ToLower(full), spec: spec, facts: commandFactsOf(full)}, ok
			}
		}
		return introspected{}, false
	}
	spec, ok := commandSpecOf(container)
	if !ok {
		return introspected{}, false
	}
	c := introspected{name: strings.ToLower(container), spec: spec, facts: commandFactsOf(container)}
	for _, s := range subcommandsOf(container) {
		if sub, ok := introspect(container + "|" + s.name); ok {
			c.subs = append(c.subs, sub)
		}
	}
	return c, true
}

// everyCommand is every command Keel has, by name.
func everyCommand() []introspected {
	names := make([]string, 0, len(commandArity))
	for name := range commandArity {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]introspected, 0, len(names))
	for _, name := range names {
		if c, ok := introspect(name); ok {
			out = append(out, c)
		}
	}
	return out
}

// isModule reports whether the command is a module's: RedisBloom's.
func (c introspected) isModule() bool { return redisBloomGroup(c.spec.group) }

// legacyKeys is the first key, last key and step Redis reports for clients
// that read no key specs, and whether its key specs leave the keys'
// positions to the arguments (movablekeys): populateCommandLegacyRangeSpec.
func (s commandSpec) legacyKeys() (first, last, step int, movable bool) {
	if len(s.keys) == 0 {
		return 0, 0, 0, false
	}
	plain := func(k keySpec) bool { return k.begin.kind == "index" && k.find.kind == "range" }
	if len(s.keys) == 1 && plain(s.keys[0]) {
		k := s.keys[0]
		last = k.find.lastKey
		if last >= 0 {
			last += k.begin.pos
		}
		return k.begin.pos, last, k.find.step, k.hasFlag("INCOMPLETE")
	}
	first = 1 << 31
	var lastAbs, prevLast uint32
	for _, k := range s.keys {
		if !plain(k) {
			movable = true
			continue
		}
		if k.find.step != 1 || (prevLast != 0 && int(prevLast) != k.begin.pos-1) {
			movable = true
			continue
		}
		if k.hasFlag("INCOMPLETE") {
			movable = true
		}
		first = min(first, k.begin.pos)
		abs := k.find.lastKey
		if abs >= 0 {
			abs += k.begin.pos
		}
		// Redis compares the last keys unsigned, so -1 (the last argument)
		// is the largest.
		lastAbs = max(lastAbs, uint32(abs))
		prevLast = lastAbs
	}
	if first == 1<<31 {
		return 0, 0, 0, true
	}
	last = int(int32(lastAbs))
	// The range Redis keeps holds lastkey relative to firstkey, and COMMAND
	// INFO adds firstkey back.
	return first, last, 1, movable
}

// hasFlag reports whether a key spec declares flag, upper case.
func (k keySpec) hasFlag(flag string) bool {
	for _, f := range k.flags {
		if f == flag {
			return true
		}
	}
	return false
}

// commandFlags are COMMAND INFO's flags of c, in Redis's order.
func (c introspected) commandFlags() []string {
	declared := map[string]bool{}
	for _, f := range c.spec.flags {
		declared[f] = true
	}
	if c.isModule() {
		declared["MODULE"] = true
	}
	if _, _, _, movable := c.spec.legacyKeys(); movable {
		declared["MOVABLEKEYS"] = true
	}
	var out []string
	for _, f := range commandFlagOrder {
		if declared[f] {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// categories are c's ACL categories, declared and implied by its flags
// (setImplicitACLCategories), in Redis's order.
func (c introspected) categories() []string {
	has := map[string]bool{}
	for _, a := range c.spec.acl {
		has[a] = true
	}
	s := c.spec
	if s.hasFlag("WRITE") {
		has["WRITE"] = true
	}
	if s.hasFlag("READONLY") && !has["SCRIPTING"] {
		has["READ"] = true
	}
	if s.hasFlag("ADMIN") {
		has["ADMIN"], has["DANGEROUS"] = true, true
	}
	if s.hasFlag("PUBSUB") {
		has["PUBSUB"] = true
	}
	if s.hasFlag("FAST") {
		has["FAST"] = true
	}
	if s.hasFlag("BLOCKING") {
		has["BLOCKING"] = true
	}
	if !has["FAST"] {
		has["SLOW"] = true
	}
	var out []string
	for _, a := range aclCategoryOrder {
		if has[a] {
			out = append(out, "@"+strings.ToLower(a))
		}
	}
	return out
}

// keyFlags are a key spec's flags as Redis writes them, in its order.
func keyFlags(flags []string) []string {
	var out []string
	for _, f := range keyFlagOrder {
		for _, g := range flags {
			if g == f[0] {
				out = append(out, f[1])
				break
			}
		}
	}
	return out
}

// statusSet writes names as a set of simple strings.
func statusSet(r *resp.Reply, names []string) {
	r.Set(len(names))
	for _, n := range names {
		r.Status(n)
	}
}

// writeCommandInfo is addReplyCommandInfo: ten fields of c.
func writeCommandInfo(r *resp.Reply, c introspected) {
	first, last, step, _ := c.spec.legacyKeys()
	r.Array(10)
	r.Bulk(c.name)
	r.Int(int64(c.spec.arity))
	statusSet(r, c.commandFlags())
	r.Int(int64(first))
	r.Int(int64(last))
	r.Int(int64(step))
	statusSet(r, c.categories())
	r.Set(len(c.facts.tips))
	for _, tip := range c.facts.tips {
		r.Bulk(tip)
	}
	writeKeySpecs(r, c.spec.keys)
	if len(c.subs) == 0 {
		r.Set(0)
		return
	}
	r.Array(len(c.subs))
	for _, sub := range c.subs {
		writeCommandInfo(r, sub)
	}
}

// writeKeySpecs is addReplyCommandKeySpecs, without the notes, which are
// Redis's text.
func writeKeySpecs(r *resp.Reply, keys []keySpec) {
	r.Set(len(keys))
	for _, k := range keys {
		r.Map(3)
		r.Bulk("flags")
		statusSet(r, keyFlags(k.flags))
		r.Bulk("begin_search")
		r.Map(2)
		r.Bulk("type")
		r.Bulk(k.begin.kind)
		r.Bulk("spec")
		switch k.begin.kind {
		case "index":
			r.Map(1)
			r.Bulk("index")
			r.Int(int64(k.begin.pos))
		case "keyword":
			r.Map(2)
			r.Bulk("keyword")
			r.Bulk(k.begin.keyword)
			r.Bulk("startfrom")
			r.Int(int64(k.begin.startFrom))
		default:
			r.Map(0)
		}
		r.Bulk("find_keys")
		r.Map(2)
		r.Bulk("type")
		r.Bulk(k.find.kind)
		r.Bulk("spec")
		switch k.find.kind {
		case "range":
			r.Map(3)
			r.Bulk("lastkey")
			r.Int(int64(k.find.lastKey))
			r.Bulk("keystep")
			r.Int(int64(k.find.step))
			r.Bulk("limit")
			r.Int(int64(k.find.limit))
		case "keynum":
			r.Map(3)
			r.Bulk("keynumidx")
			r.Int(int64(k.find.keynumIdx))
			r.Bulk("firstkey")
			r.Int(int64(k.find.firstKey))
			r.Bulk("keystep")
			r.Int(int64(k.find.step))
		default:
			r.Map(0)
		}
	}
}

// writeCommandDocs is addReplyCommandDocs, with the facts and none of the
// text: since, group, module, doc flags, the version it was deprecated in,
// its arguments and its subcommands.
func writeCommandDocs(r *resp.Reply, c introspected) {
	s := c.facts
	n := 1
	for _, present := range []bool{s.since != "", c.isModule(), len(s.docFlags) > 0, s.deprecatedSince != "",
		len(s.args) > 0, len(c.subs) > 0} {
		if present {
			n++
		}
	}
	r.Map(n)
	if s.since != "" {
		r.Bulk("since")
		r.Bulk(s.since)
	}
	r.Bulk("group")
	if c.isModule() {
		r.Bulk("module")
	} else {
		r.Bulk(c.spec.group)
	}
	if c.isModule() {
		r.Bulk("module")
		r.Bulk("bf")
	}
	if len(s.docFlags) > 0 {
		r.Bulk("doc_flags")
		var names []string
		for _, f := range []string{"DEPRECATED", "SYSCMD"} {
			for _, g := range s.docFlags {
				if g == f {
					names = append(names, strings.ToLower(f))
				}
			}
		}
		statusSet(r, names)
	}
	if s.deprecatedSince != "" {
		r.Bulk("deprecated_since")
		r.Bulk(s.deprecatedSince)
	}
	if len(s.args) > 0 {
		r.Bulk("arguments")
		writeArguments(r, s.args)
	}
	if len(c.subs) > 0 {
		r.Bulk("subcommands")
		r.Map(len(c.subs))
		for _, sub := range c.subs {
			r.Bulk(sub.name)
			writeCommandDocs(r, sub)
		}
	}
}

// writeArguments is addReplyCommandArgList, without the arguments' summaries.
func writeArguments(r *resp.Reply, args []commandArg) {
	r.Array(len(args))
	for _, a := range args {
		nested := a.typ == "oneof" || a.typ == "block"
		n := 3
		for _, present := range []bool{a.keySpec != -1, a.token != "", a.since != "", a.deprecatedSince != "",
			len(a.flags) > 0} {
			if present {
				n++
			}
		}
		r.Map(n)
		r.Bulk("name")
		r.Bulk(a.name)
		r.Bulk("type")
		r.Bulk(a.typ)
		if !nested {
			r.Bulk("display_text")
			if a.display != "" {
				r.Bulk(a.display)
			} else {
				r.Bulk(a.name)
			}
		}
		if a.keySpec != -1 {
			r.Bulk("key_spec_index")
			r.Int(int64(a.keySpec))
		}
		if a.token != "" {
			r.Bulk("token")
			r.Bulk(a.token)
		}
		if a.since != "" {
			r.Bulk("since")
			r.Bulk(a.since)
		}
		if a.deprecatedSince != "" {
			r.Bulk("deprecated_since")
			r.Bulk(a.deprecatedSince)
		}
		if len(a.flags) > 0 {
			r.Bulk("flags")
			statusSet(r, a.flags)
		}
		if nested {
			r.Bulk("arguments")
			writeArguments(r, a.args)
		}
	}
}

// cmdCOMMAND implements COMMAND and its COUNT, LIST, INFO, DOCS, GETKEYS,
// GETKEYSANDFLAGS and HELP. The subcommand and its count have been checked
// against the command table by the time this runs; see containerCommands.
func (e *Engine) cmdCOMMAND(args []string) []byte {
	if err := CommandError(&Command{Cmd: "COMMAND", Args: args}); err != nil {
		return e.encode(err, false)
	}
	reply := e.reply()
	r := &reply
	if len(args) == 0 {
		all := everyCommand()
		r.Array(len(all))
		for _, c := range all {
			writeCommandInfo(r, c)
		}
		return r.Bytes()
	}
	switch strings.ToLower(args[0]) {
	case "help":
		return commandHelp()
	case "count":
		return r.Int(int64(len(everyCommand()))).Bytes()
	case "list":
		return e.commandList(r, args[1:])
	case "info":
		if len(args) == 1 {
			all := everyCommand()
			r.Array(len(all))
			for _, c := range all {
				writeCommandInfo(r, c)
			}
			return r.Bytes()
		}
		r.Array(len(args) - 1)
		for _, name := range args[1:] {
			if c, ok := introspect(name); ok {
				writeCommandInfo(r, c)
			} else {
				r.Null()
			}
		}
		return r.Bytes()
	case "docs":
		var found []introspected
		if len(args) == 1 {
			found = everyCommand()
		} else {
			for _, name := range args[1:] {
				if c, ok := introspect(name); ok {
					found = append(found, c)
				}
			}
		}
		r.Map(len(found))
		for _, c := range found {
			r.Bulk(c.name)
			writeCommandDocs(r, c)
		}
		return r.Bytes()
	}
	// GETKEYS and GETKEYSANDFLAGS.
	return e.commandGetKeys(r, args[1:], strings.EqualFold(args[0], "getkeysandflags"))
}

// commandList is COMMAND LIST [FILTERBY MODULE name | ACLCAT category |
// PATTERN pattern]: every command's name and every subcommand's, or those the
// filter keeps.
func (e *Engine) commandList(r *resp.Reply, args []string) []byte {
	var kind, arg string
	for i := 0; i < len(args); i++ {
		if !strings.EqualFold(args[i], "filterby") || len(args)-1-i != 2 {
			return e.encode(errSyntax, false)
		}
		switch strings.ToLower(args[i+1]) {
		case "module", "aclcat", "pattern":
			kind, arg = strings.ToLower(args[i+1]), args[i+2]
		default:
			return e.encode(errSyntax, false)
		}
		i += 2
	}
	keep := func(c introspected) bool {
		switch kind {
		case "module":
			return c.isModule() && strings.EqualFold(arg, "bf")
		case "aclcat":
			for _, cat := range c.categories() {
				if strings.EqualFold(cat[1:], arg) {
					return true
				}
			}
			return false
		case "pattern":
			return globMatch(strings.ToLower(arg), c.name)
		}
		return true
	}
	var names []string
	for _, c := range everyCommand() {
		if keep(c) {
			names = append(names, c.name)
		}
		for _, sub := range c.subs {
			if keep(sub) {
				names = append(names, sub.name)
			}
		}
	}
	r.Array(len(names))
	for _, name := range names {
		r.Bulk(name)
	}
	return r.Bytes()
}

// commandGetKeys is COMMAND GETKEYS and GETKEYSANDFLAGS (getKeysSubcommandImpl):
// the keys of the command args would be, and with flags, how it uses each.
func (e *Engine) commandGetKeys(r *resp.Reply, args []string, withFlags bool) []byte {
	c, ok := introspect(args[0])
	if ok && len(c.subs) > 0 && len(args) >= 2 {
		// A container's subcommand is its second word (lookupCommand).
		c, ok = introspect(args[0] + "|" + args[1])
	} else if ok && strings.Contains(args[0], "|") {
		ok = false
	}
	switch {
	case !ok:
		return appendError(nil, errGetKeysInvalidCommand)
	case !c.hasKeys():
		return appendError(nil, errGetKeysNoKeys)
	case (c.spec.arity > 0 && c.spec.arity != len(args)) || len(args) < -c.spec.arity:
		return appendError(nil, errGetKeysArity)
	}
	keys := c.findKeys(args)
	if len(keys) == 0 {
		if c.spec.hasFlag("NO_MANDATORY_KEYS") {
			return r.Array(0).Bytes()
		}
		return appendError(nil, errGetKeysInvalidArguments)
	}
	r.Array(len(keys))
	for _, k := range keys {
		if !withFlags {
			r.Bulk(args[k.pos])
			continue
		}
		r.Array(2)
		r.Bulk(args[k.pos])
		statusSet(r, keyFlags(k.flags))
	}
	return r.Bytes()
}

// foundKey is a key COMMAND GETKEYS found: its position among the command's
// words, the name first, and its key spec's flags.
type foundKey struct {
	pos   int
	flags []string
}

// hasKeys is doesCommandHaveKeys: a function of its own that finds them, or
// a key spec that is not NOT_KEY.
func (c introspected) hasKeys() bool {
	if c.facts.getKeys != "" {
		return true
	}
	for _, k := range c.spec.keys {
		if !k.hasFlag("NOT_KEY") {
			return true
		}
	}
	return false
}

// findKeys is getKeysFromCommandWithSpecs: the key specs, when they say all
// there is to say, and otherwise the command's own function.
func (c introspected) findKeys(argv []string) []foundKey {
	hasKeySpec, variable := false, false
	for _, k := range c.spec.keys {
		if !k.hasFlag("NOT_KEY") {
			hasKeySpec = true
		}
		if k.hasFlag("VARIABLE_FLAGS") {
			variable = true
		}
	}
	if hasKeySpec && !variable {
		if keys, ok := keysFromSpecs(c.spec, argv); ok {
			return keys
		}
	}
	if proc := getKeysProc(c.facts.getKeys); proc != nil {
		return proc(argv)
	}
	return nil
}

// keysFromSpecs is getKeysUsingKeySpecs: each key spec's keys, in order, or
// ok false if one cannot be followed, as an arity or syntax error leaves it.
func keysFromSpecs(s commandSpec, argv []string) ([]foundKey, bool) {
	var out []foundKey
	argc := len(argv)
	for _, k := range s.keys {
		if k.hasFlag("NOT_KEY") {
			continue
		}
		first := 0
		switch k.begin.kind {
		case "index":
			first = k.begin.pos
		case "keyword":
			start, end := k.begin.startFrom, argc-1
			if start <= 0 {
				start, end = argc+k.begin.startFrom, 1
			}
			for i := start; i != end; {
				if i >= argc || i < 1 {
					break
				}
				if strings.EqualFold(argv[i], k.begin.keyword) {
					first = i + 1
					break
				}
				if start <= end {
					i++
				} else {
					i--
				}
			}
			if first == 0 {
				continue
			}
		default:
			return nil, false
		}
		var last, step int
		switch k.find.kind {
		case "range":
			step = k.find.step
			switch {
			case k.find.lastKey >= 0:
				last = first + k.find.lastKey
			case k.find.limit == 0:
				last = argc + k.find.lastKey
			default:
				last = first + ((argc-first)/k.find.limit + k.find.lastKey)
			}
		case "keynum":
			step = k.find.step
			idx := first + k.find.keynumIdx
			if idx >= argc || idx < 0 {
				return nil, false
			}
			n, ok := redisInteger(argv[idx])
			if !ok || n < 0 {
				return nil, false
			}
			first += k.find.firstKey
			last = first + (int(n)-1)*step
		default:
			return nil, false
		}
		if last >= argc || last < first || first >= argc {
			return nil, false
		}
		for i := first; i <= last; i += step {
			if i >= argc || i < first {
				continue
			}
			out = append(out, foundKey{pos: i, flags: k.flags})
		}
		if k.hasFlag("INCOMPLETE") {
			return nil, false
		}
	}
	return out, true
}

// commandFactsOf is what else Redis declares of a command, by its name as
// redisCommandSpecs holds it, from redisCommandFacts; none for Keel's own
// commands and RedisBloom's.
func commandFactsOf(name string) commandFacts {
	i := sort.Search(len(redisCommandFacts), func(i int) bool { return redisCommandFacts[i].name >= name })
	if i < len(redisCommandFacts) && redisCommandFacts[i].name == name {
		return redisCommandFacts[i]
	}
	return commandFacts{}
}

// getKeysProc is Keel's own of the function Redis finds a command's keys
// with where its key specs cannot say, by the name of Redis's (db.c), or nil
// for one Keel lacks.
func getKeysProc(name string) func(argv []string) []foundKey {
	switch name {
	case "setGetKeys":
		return setGetKeys
	case "pfmergeGetKeys":
		return pfmergeGetKeys
	case "lmpopGetKeys":
		return lmpopGetKeys
	case "zunionInterDiffGetKeys":
		return zunionInterDiffGetKeys
	case "zunionInterDiffStoreGetKeys":
		return zunionInterDiffStoreGetKeys
	case "zmpopGetKeys":
		return zmpopGetKeys
	case "georadiusGetKeys":
		return georadiusGetKeys
	case "delexGetKeys":
		return delexGetKeys
	}
	return nil
}

// lmpopGetKeys: LMPOP's numkeys keys, after the count, when its key spec
// cannot be followed.
func lmpopGetKeys(argv []string) []foundKey { return genericGetKeys(0, 1, 2, 1, argv) }

// zunionInterDiffGetKeys: ZUNION's, ZINTER's, ZDIFF's and ZINTERCARD's
// numkeys keys, after the count.
func zunionInterDiffGetKeys(argv []string) []foundKey { return genericGetKeys(0, 1, 2, 1, argv) }

// zunionInterDiffStoreGetKeys: the STORE forms' destination, then the
// numkeys keys after the count.
func zunionInterDiffStoreGetKeys(argv []string) []foundKey { return genericGetKeys(1, 2, 3, 1, argv) }

// zmpopGetKeys: ZMPOP's numkeys keys, after the count.
func zmpopGetKeys(argv []string) []foundKey { return genericGetKeys(0, 1, 2, 1, argv) }

// georadiusGetKeys: GEORADIUS's and GEORADIUSBYMEMBER's key, read, and the
// key the last STORE or STOREDIST names, overwritten. Redis looks for the
// option from the fifth argument for both commands (db.c,
// georadiusGetKeys), and so does Keel.
func georadiusGetKeys(argv []string) []foundKey {
	keys := []foundKey{{pos: 1, flags: []string{"RO", "ACCESS"}}}
	stored := -1
	for i := 5; i < len(argv); i++ {
		if (strings.EqualFold(argv[i], "store") || strings.EqualFold(argv[i], "storedist")) && i+1 < len(argv) {
			stored = i + 1
			i++
		}
	}
	if stored != -1 {
		keys = append(keys, foundKey{pos: stored, flags: []string{"OW", "UPDATE"}})
	}
	return keys
}

// genericGetKeys is Redis's: a count at keyCountOfs read as C's atoi reads
// it, then that many keys from firstKeyOfs, keyStep apart, and the store key
// at storeKeyOfs if there is one, every flag left empty. A count that does
// not fit the arguments finds none.
func genericGetKeys(storeKeyOfs, keyCountOfs, firstKeyOfs, keyStep int, argv []string) []foundKey {
	if keyCountOfs >= len(argv) {
		return nil
	}
	num := atoi(argv[keyCountOfs])
	if num < 1 || num > int64((len(argv)-firstKeyOfs)/keyStep) {
		return nil
	}
	keys := make([]foundKey, 0, num+1)
	for i := int64(0); i < num; i++ {
		keys = append(keys, foundKey{pos: firstKeyOfs + int(i)*keyStep, flags: []string{}})
	}
	if storeKeyOfs != 0 {
		keys = append(keys, foundKey{pos: storeKeyOfs, flags: []string{}})
	}
	return keys
}

// atoi reads s as C's atoi does: spaces skipped, a sign, then digits up to
// the first that is not one; nothing read is 0.
func atoi(s string) int64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || (s[i] >= '\t' && s[i] <= '\r')) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var n int64
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		if n > (1<<62)/10 {
			break
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		return -n
	}
	return n
}

// setGetKeys: SET's key is overwritten, or read and written with GET or a
// condition on its value.
func setGetKeys(argv []string) []foundKey {
	actual, logical := "OW", []string{"UPDATE"}
	for _, arg := range argv[3:] {
		switch {
		case len(arg) == 3 && strings.EqualFold(arg, "get"):
			actual, logical = "RW", []string{"ACCESS", "UPDATE"}
		case strings.EqualFold(arg, "ifeq"), strings.EqualFold(arg, "ifne"),
			strings.EqualFold(arg, "ifdeq"), strings.EqualFold(arg, "ifdne"):
			actual = "RW"
		}
	}
	return []foundKey{{pos: 1, flags: append([]string{actual}, logical...)}}
}

// delexGetKeys: DELEX's key is removed, or read and removed with a
// condition on its value.
func delexGetKeys(argv []string) []foundKey {
	actual := "RM"
	for _, arg := range argv[2:] {
		if strings.EqualFold(arg, "ifeq") || strings.EqualFold(arg, "ifne") ||
			strings.EqualFold(arg, "ifdeq") || strings.EqualFold(arg, "ifdne") {
			actual = "RW"
		}
	}
	return []foundKey{{pos: 1, flags: []string{actual, "DELETE"}}}
}

// pfmergeGetKeys: the destination is read and inserted into, the sources
// read.
func pfmergeGetKeys(argv []string) []foundKey {
	keys := []foundKey{{pos: 1, flags: []string{"RW", "ACCESS", "INSERT"}}}
	for i := 2; i < len(argv); i++ {
		keys = append(keys, foundKey{pos: i, flags: []string{"RO", "ACCESS"}})
	}
	return keys
}
