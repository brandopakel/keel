package core

// What is declared of a command: its arity, its group, its flags, its ACL
// categories and where its keys are. Redis declares these for each of its
// commands in src/commands/<name>.json, and command_specs.go, which
// scripts/generate-command-specs.py writes from them, holds Redis 8.10.1's
// declarations for the commands Keel has. The commands Redis has no spec
// for - Keel's own, and RedisBloom's, which the module declares in its
// source - are declared here by hand, in the same form.
//
// Phase 4 of the embedding plan (docs/embedding-plan.md, "Step 4.2") makes
// one descriptor table of these and of the handlers, from which COMMAND is
// answered. Until the dispatch tables (commandTable, commandArity,
// commandKeyspace, writeCommands and the key rules) are replaced by it,
// command_specs_test.go holds the two to each other: every arity agrees,
// WRITE is the log's writeCommands, and each key spec is the key rule the
// type check applies.

// commandSpec is what is declared of one command, as Redis declares it.
type commandSpec struct {
	// arity is how many words the command takes, the name included: n
	// means exactly n, -n at least n.
	arity int
	// group is Redis's command group: string, generic, hash, list, set,
	// sorted_set, geo, hyperloglog, server, connection, transactions; and
	// Keel's for its own commands.
	group string
	// flags are Redis's command flags: WRITE, READONLY, DENYOOM, ADMIN,
	// NOSCRIPT, FAST, and the rest of COMMAND INFO's.
	flags []string
	// acl are Redis's ACL categories, which COMMAND INFO reports as
	// @string, @read and the rest.
	acl []string
	// keys are the key specs: where in the arguments the keys are.
	keys []keySpec
}

// keySpec is one of Redis's key specs: how to find a group of keys among a
// command's arguments, and what the command does with them.
type keySpec struct {
	// flags are RO, RW, OW, RM, ACCESS, UPDATE, INSERT, DELETE,
	// NOT_KEY, INCOMPLETE and VARIABLE_FLAGS.
	flags []string
	begin keyBegin
	find  keyFind
}

// keyBegin says where the search for the keys starts: at the argument at
// pos (kind "index"), or after the argument that is keyword, looked for from
// startFrom (kind "keyword"); "unknown" is a search no spec describes.
type keyBegin struct {
	kind      string
	pos       int
	keyword   string
	startFrom int
}

// keyFind says which arguments from there are keys: every step-th up to
// lastKey (kind "range"; -1 is the last argument, and limit divides what is
// left when lastKey is -1), or as many as the argument at keynumIdx says,
// from firstKey, every step-th (kind "keynum"); "unknown" is a rule no spec
// describes.
type keyFind struct {
	kind      string
	lastKey   int
	step      int
	limit     int
	keynumIdx int
	firstKey  int
}

// Key specs the hand-written descriptors share: the first argument, and
// every argument from the first.
var (
	oneKey = []keySpec{{flags: []string{"RW", "ACCESS", "UPDATE"},
		begin: keyBegin{kind: "index", pos: 1}, find: keyFind{kind: "range", lastKey: 0, step: 1}}}
	oneKeyRead = []keySpec{{flags: []string{"RO", "ACCESS"},
		begin: keyBegin{kind: "index", pos: 1}, find: keyFind{kind: "range", lastKey: 0, step: 1}}}
	oneKeyWrite = []keySpec{{flags: []string{"OW", "UPDATE"},
		begin: keyBegin{kind: "index", pos: 1}, find: keyFind{kind: "range", lastKey: 0, step: 1}}}
)

// keelCommandSpecs declares the commands Redis has no spec for: Keel's own,
// in Keel's groups, and RedisBloom's, as the module declares them.
// KEEL.REPL.PULL and KEEL.REPL.PULL2 are a replica's requests, as SYNC and
// PSYNC are Redis's; KEEL.PROMOTE and KEEL.FENCE act on the node, as
// FAILOVER does.
var keelCommandSpecs = map[string]commandSpec{
	"KEEL.DUMP":       {arity: 2, group: "keel", flags: []string{"READONLY"}, acl: []string{"KEYSPACE", "READ"}, keys: oneKeyRead},
	"KEEL.RESTORE":    {arity: 3, group: "keel", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"KEYSPACE", "WRITE"}, keys: oneKeyWrite},
	"MEMKV.DUMP":      {arity: 2, group: "keel", flags: []string{"READONLY"}, acl: []string{"KEYSPACE", "READ"}, keys: oneKeyRead},
	"MEMKV.RESTORE":   {arity: 3, group: "keel", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"KEYSPACE", "WRITE"}, keys: oneKeyWrite},
	"KEEL.PROMOTE":    {arity: 2, group: "keel", flags: []string{"ADMIN", "NOSCRIPT", "STALE"}, acl: []string{"ADMIN", "DANGEROUS"}},
	"KEEL.FENCE":      {arity: 2, group: "keel", flags: []string{"ADMIN", "NOSCRIPT", "STALE"}, acl: []string{"ADMIN", "DANGEROUS"}},
	"KEEL.REPL.PULL":  {arity: 3, group: "keel", flags: []string{"ADMIN", "NOSCRIPT", "NO_MULTI"}, acl: []string{"ADMIN", "DANGEROUS"}},
	"KEEL.REPL.PULL2": {arity: -5, group: "keel", flags: []string{"ADMIN", "NOSCRIPT", "NO_MULTI"}, acl: []string{"ADMIN", "DANGEROUS"}},

	"SRAND": {arity: -2, group: "set", flags: []string{"READONLY"}, acl: []string{"READ", "SET"}, keys: oneKeyRead},

	"MORRIS.INITBYDIM":  {arity: 4, group: "morris", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE"}, keys: oneKeyWrite},
	"MORRIS.INITBYPROB": {arity: 4, group: "morris", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE"}, keys: oneKeyWrite},
	"MORRIS.INCRBY":     {arity: -4, group: "morris", flags: []string{"WRITE"}, acl: []string{"WRITE"}, keys: oneKey},
	"MORRIS.QUERY":      {arity: -3, group: "morris", flags: []string{"READONLY"}, acl: []string{"READ"}, keys: oneKeyRead},
	"MORRIS.INFO":       {arity: 2, group: "morris", flags: []string{"READONLY"}, acl: []string{"READ"}, keys: oneKeyRead},

	// RedisBloom 8.10.1's own declarations (rebloom.c, rm_cms.c): the mode
	// each command is registered with, as flags, and its ACL categories with
	// the module's, each on its first argument.
	"BF.RESERVE": {arity: -4, group: "bf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "FAST", "BLOOM"}, keys: oneKeyWrite},
	"BF.ADD":     {arity: 3, group: "bf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "BLOOM"}, keys: oneKey},
	"BF.MADD":    {arity: -3, group: "bf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "BLOOM"}, keys: oneKey},
	"BF.EXISTS":  {arity: 3, group: "bf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "BLOOM"}, keys: oneKeyRead},
	"BF.MEXISTS": {arity: -3, group: "bf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "BLOOM"}, keys: oneKeyRead},
	"BF.INFO":    {arity: -2, group: "bf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "FAST", "BLOOM"}, keys: oneKeyRead},

	"CF.RESERVE": {arity: -3, group: "cf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "FAST", "CUCKOO"}, keys: oneKeyWrite},
	"CF.ADD":     {arity: 3, group: "cf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "CUCKOO"}, keys: oneKey},
	"CF.ADDNX":   {arity: 3, group: "cf", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "CUCKOO"}, keys: oneKey},
	"CF.EXISTS":  {arity: 3, group: "cf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "CUCKOO"}, keys: oneKeyRead},
	"CF.MEXISTS": {arity: -3, group: "cf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "CUCKOO"}, keys: oneKeyRead},
	"CF.DEL":     {arity: 3, group: "cf", flags: []string{"WRITE", "FAST"}, acl: []string{"WRITE", "CUCKOO"}, keys: oneKey},
	"CF.COUNT":   {arity: 3, group: "cf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "CUCKOO"}, keys: oneKeyRead},
	"CF.INFO":    {arity: 2, group: "cf", flags: []string{"READONLY", "FAST"}, acl: []string{"READ", "FAST", "CUCKOO"}, keys: oneKeyRead},

	"CMS.INITBYDIM":  {arity: 4, group: "cms", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "FAST", "CMS"}, keys: oneKeyWrite},
	"CMS.INITBYPROB": {arity: 4, group: "cms", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "FAST", "CMS"}, keys: oneKeyWrite},
	"CMS.INCRBY":     {arity: -4, group: "cms", flags: []string{"WRITE", "DENYOOM"}, acl: []string{"WRITE", "CMS"}, keys: oneKey},
	"CMS.QUERY":      {arity: -3, group: "cms", flags: []string{"READONLY"}, acl: []string{"READ", "CMS"}, keys: oneKeyRead},
}

// namedCommandSpec is one entry of redisCommandSpecTable: a command's name,
// a subcommand as container|sub, and its spec.
type namedCommandSpec struct {
	name string
	spec commandSpec
}

// redisCommandSpecs is redisCommandSpecTable by name, built by a loop whose
// size does not depend on the table's, so that a command added to the
// generated table adds data and no code. The loop is the initializer's own,
// with the package's init ahead of the command handlers, rather than a
// function among them, so that it moves none of them either.
var redisCommandSpecs = func() map[string]commandSpec {
	specs := make(map[string]commandSpec, len(redisCommandSpecTable))
	for _, entry := range redisCommandSpecTable {
		specs[entry.name] = entry.spec
	}
	return specs
}()

// commandSpecOf is the spec of a command, or of a container's subcommand
// as container|sub, from either table.
func commandSpecOf(name string) (commandSpec, bool) {
	if spec, ok := redisCommandSpecs[name]; ok {
		return spec, true
	}
	spec, ok := keelCommandSpecs[name]
	return spec, ok
}

// hasFlag reports whether the spec carries flag.
func (s commandSpec) hasFlag(flag string) bool {
	for _, f := range s.flags {
		if f == flag {
			return true
		}
	}
	return false
}
