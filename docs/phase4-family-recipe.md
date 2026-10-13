# Converting a command family (phase 4)

How a family of commands is converted to typed operations, and how the
commands Redis has and Keel lacks are then added to it. Strings were the
template (docs/embedding-plan.md, step 4.3); this is what was done for them,
written so that keys and TTL, hashes, lists, sets, sorted sets and the rest
can be converted the same way, by different people at the same time.

Read first: AGENTS.md; docs/embedding-plan.md's "Phase 4: operations" and
its steps 4.0 to 4.3; docs/redis-parity-plan.md; docs/error-replies.md;
docs/resp3.md; docs/redis-internals-for-keel.md for what Redis does;
`internal/core/strings.go` and `internal/core/commands_dict.go` as the worked
example.

## The rules

- **Nothing a client or a log sees changes** in a conversion: every reply in
  both protocols, every error and its order, every log record, byte for
  byte. The command corpus (`internal/core/command_golden_test.go`) and the
  persistence, replication and RESP3 tests hold you to it.
- **Nothing allocates more.** The paired command-path job fails on any row
  that allocates more per operation.
- **Follow Redis 8.10.1** for anything a client sees, including in the
  commands you add (its source is the reference; `src/commands/*.json` is
  the spec of each command). Where Keel keeps something better than an
  accidental Redis flaw, document it as a departure citing Redis's file and
  line, and list it in the PR under "Improvements over Redis".
- **A change to a reply recaptures the corpus in the same PR.** The corpus
  pins develop's replies, so a PR that changes one on purpose - a new
  command, a new error, a new INFO field, a new line of a HELP - fails it
  until it recaptures: set `commandGoldenRevision` and
  `connectionGoldenRevision` to the develop revision the PR is based on,
  run `python3 scripts/capture-command-goldens.py --out dist/<name>`, and
  put the fixture's diff in the PR body as the change's wire-visible
  effect. Every INFO-stack PR does this too.

## Converting a family

One PR per family. Base it on develop after the previous family's conversion
has merged, or coordinate: two families converting at once touch
`eval.go`'s tables (until step 4.2b replaces them with the descriptor
table), `aof.go`'s `writeCommands` and `keytype.go`'s tables, in different
lines.

1. **Read the corpus for the family** (`internal/core/testdata/commands-*/`
   `<family>.txt`): every reply and record your conversion must reproduce.
   If a path of a handler is not in it - an error, an option, a reply shape
   - add the case to `command_golden_corpus_test.go` and recapture **before**
   converting, in a commit of its own, so the conversion is held to it.

2. **Write the typed operations** in `internal/core/<family>.go` (strings:
   `strings.go`). One function per thing the family does, named `op<Name>`:
   - Go types in (a key, a member, an `int64`, a request struct for
     options), a typed result and an error out. The errors are the existing
     sentinels, whose text is Redis's wire error (`errWrongType`,
     `errNotAnInteger`, ...); add one as a package-level `var` (and to
     `packageVars` in `engine_census_test.go`) rather than building it per
     call.
   - **Assume the scope's checks.** An operation runs inside a command's
     scope, after its arity, a replica's refusal and the type check of the
     keys its descriptor constrains. Do not look up what type holds a key
     again; refuse only what the operation alone can know.
   - **Stage the record of every write**, with `e.aofRecord(parts...)`,
     `e.aofRecordInt(name, key, n)` (an integer last part, written in
     decimal) or `e.aofRecordArgs(name, args)` (a name and a whole argument
     list). Staging allocates nothing. Stage exactly the records the corpus
     shows (`~` lines), in the same order: for a command logged as it
     arrived, that is its name and arguments; a replaced key's `DEL` comes
     first (`dropOtherType`). A write that declines (NX, XX), or that finds
     nothing to change, stages nothing. Never rely on the command being
     logged as it arrived: a Go caller has no command.
   - Keep what the handler did to the stores, in the same order: the reads
     that reap an expired key, the `Resize` after a change, dropping an
     emptied collection.

3. **Rewrite the handlers as the command language** in the family's
   `commands_*.go`. Each write's handler first declares that it records
   itself, `e.recordsItself()`: the log then takes exactly what its
   operation staged, and nothing when that is nothing, on every path of the
   handler, so a path that changes nothing without calling the operation (a
   missing key) is not logged as it arrived. Then read the arguments in Redis's order (a malformed option
   before a malformed number, the count before the key; `argumentsBeforeType`
   in `keytype.go` must still find the same parser), call the operation, and
   answer through the reply sink:

   ```go
   r := e.reply()
   if value, ok := e.opGet(args[0]); ok {
       return r.Bulk(value).Bytes()
   }
   return r.Null().Bytes()
   ```

   `resp.Reply` frames the reply in the command's protocol and admits it
   before it allocates. `Status("OK")`, `Int(0)`, `Int(1)`, `Null()`,
   `NullArray()`, `Bool(...)`, `EmptyMap()` and `EmptySet()` are the shared
   constants and allocate nothing; `Bulk` sizes, admits and allocates once;
   `Error(err)` answers an error; `Double`, `Verbatim`, `Array`, `Set`,
   `Map` and `Pair` build the rest. A reply of many values is sized first
   and `Reserve`d once, then filled. A reply that must be admitted before a
   write (SET's `GET`) is built before the operation runs: split the lookup
   from the write (`setTarget`, then `opSet`). The collection replies
   (`encodeLookupArray`, `encodeWalkReply`, `scoredReply`) may stay as they
   are until a family moves them into `Reply` with a measurement.

4. **Check it locally** (at most two minutes, through the wrapper, AGENTS.md):

   ```sh
   python3 scripts/run-local-validation.py --out dist/<name> --seconds 120 -- \
     go test -count=1 ./internal/resp ./internal/golden ./internal/server ./internal/core
   ```

   and compare allocations per operation of the family's benchmark rows
   against the base, with `go test -run '^$' -bench '^BenchmarkCommandPath(WithLog|WithReplica)?$/^(<ROWS>)$' -benchmem`
   in each tree's `internal/core`. Look at `go build -gcflags=-m` for your
   handlers: a `resp.Reply` must not be "moved to heap".

5. **Measure on GitHub.** The PR runs the paired command-path job against
   develop. Dispatch two more against the exact base SHA:

   ```sh
   gh workflow run command-path.yml -f baseline=<base sha> -f candidate=<head sha> -f rounds=12
   ```

   The gate: a median paired ratio of at most 1.0204 (the plan's 0.98, as
   candidate over baseline time) and no row allocating more. A row over 1.04
   is repeated; one that stays over 1.04 is shown to be code placement with a
   control build (phase, exact-address or reverse control; see the plan's
   phase 3 write-up), or fixed.

   The job measures core's **test** binary, and in-package `_test.go` files
   are compiled into package core, in file order. Test code in a file that
   sorts before the handlers (`command_golden_test.go` sorts before
   `commands_*.go`) moves every handler after it: #170, which changed no
   non-test code, read a median of 1.025 because its corpus moved every
   handler 69,920 bytes and flipped their 64-byte phase. Before reading a
   result as the change's, build both sides' test binaries
   (`GOOS=linux GOARCH=amd64 go test -c -trimpath ./internal/core`) and
   compare the hot functions with `go tool objdump` (instructions) and
   `go tool nm` (addresses and their phase). Then the matched memtier job, with the log
   off and under everysec:

   ```sh
   gh workflow run general-validation.yml --ref <branch> -f matched=true -f baseline=<base sha>
   ```

6. **Document it**: a paragraph in the plan's phase 4 section, saying what
   moved, what was measured, and any decision the code needed.

## Adding the commands the family lacks

After the family's conversion has merged, in a PR of its own
(docs/redis-parity-plan.md lists what each family lacks):

1. **Read Redis.** The command's spec, `src/commands/<name>.json` (arity,
   flags, key specs, arguments, reply schema), and its implementation (the
   `*Command` function in `src/t_<family>.c`): the order it checks its
   arguments in, every error and its words, what it propagates to the AOF
   and replicas (`alsoPropagate`, `rewriteClientCommandVector`), and its
   RESP3 reply type.
2. **Write the operation and its command language**, as above. Its record is
   what replays to the same state: Redis's propagation is the guide (GETDEL
   is logged as DEL, INCRBYFLOAT as SET with KEEPTTL, GETEX's expiry as
   PEXPIREAT or PERSIST).
3. **Register it** wherever the dispatch reads, until step 4.2b's descriptor
   table: `commandTable` (eval.go), `commandArity` (command_check.go, Redis's
   arity), `commandKeyspace` and, if needed, the key rules (keytype.go),
   `writeCommands` (aof.go) for a write (a replica then refuses it with
   READONLY), and `argumentsBeforeType` if Redis reads an argument before the
   key. Then run `scripts/generate-command-specs.py
   --redis-src <redis-8.10.1>` so `command_specs.go` declares it, and check
   `TestCommandSpecsAgreeWithTheTables`.
4. **Pin it**: cases in the corpus for every reply shape and error, in both
   protocols, with its records; recapture. The coverage tests fail until the
   command is sent and, for a write, recorded, and until every new error
   text is answered.
5. **Hold it to Redis**: add its cases to `scripts/error-parity.py`
   (`GENERIC`, its arity comes from Redis itself) and its steps to
   `scripts/differential.py`; both run against Redis 8.10.1 in CI's
   differential job, byte for byte, in both protocols. Check the census
   (`scripts/command-census.py`) counts it.
6. **Restart and replicate it**: a write needs a restart check (its record
   replays), a rewrite check (the rewrite writes the key back the same) and
   a replica check, which the persistence tests and the differential's
   crash restarts cover once the command is in them.
7. **Document it** in README's integration contract and the parity plan's
   table.

## The PR

Bodies have "What changes", "Server-visible change" (or "none"), "Tests",
"Evidence" (paired run IDs with medians, repeated rows, matched run IDs),
"Failures" (every failed or cancelled run and why), "Throwaway branches"
(control branches, never merged), and "Improvements over Redis" when one
applies. Commits end with the co-author line the session gives.
