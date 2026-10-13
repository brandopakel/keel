# Changelog

Notable changes to Keel, newest first. Releases up to `v0.1.0-alpha.4` are
described in [docs/alpha-release-notes.md](docs/alpha-release-notes.md), which
also carries each release's validation evidence. Keel follows pre-1.0
semantic versioning: a minor release may change the public Go API, with
migration notes; the append-only log is the stronger promise, and every v0
release reads every earlier v0 log.

## Unreleased: v0.2.0-alpha.1

The first release of Keel as an embeddable Go library, and the server
changes since `v0.1.0-alpha.4`.

### Added

- **The embeddable package, `github.com/brandopakel/keel`** (experimental
  until v1). `keel.Open` runs an instance in-process: Redis's types and
  semantics, an estimated memory bound with LRU, LFU or random eviction, and
  an optional append-only log in the server's format, replayed on open and
  locked to one instance. Methods take a context and Go types and run the
  same typed operations as the server's commands, logged as those commands
  are; errors keep Redis's text and match categories with `errors.Is`. See
  [docs/public-api.md](docs/public-api.md).
  - Strings, keys and expiry: `Get`, `Set` (with `WithTTL`, `WithExpireAt`,
    `WithKeepTTL`), `SetNX`, `SetXX`, `GetDel`, `MGet` and `MSet` (maps),
    `GetBytes` and `SetBytes`, `Incr`, `Decr`, `IncrBy`, `DecrBy`,
    `IncrByFloat`, `Append`, `StrLen`, `GetRange`, `SetRange`, `Del`,
    `Exists`, `Type`, `Rename`, `RenameNX`, `DBSize`, `FlushDB`, `Expire`,
    `ExpireAt`, `Persist`, `TTL`, `ExpireTime`.
  - `Atomic`, Redis's `MULTI` and `EXEC`: commands queued on a `Tx` run
    together, logged as one block, with no rollback.
  - `Scan`, an iterator over `SCAN`'s steps that never holds the lock while
    the loop's body runs.
  - Keel-native methods: `GetOrSet` (cache-aside, one load per key for
    concurrent misses), `CompareAndSwap` and `DeleteIf` (Redis 8's `SET
    IFEQ` and `DELEX IFEQ`), and `Stats`, the counters `INFO` renders.
  - `Do`, experimental: any command, in the command language, for the
    families whose typed methods have not arrived yet.
  - Runnable examples in `examples/embedded`: a cache in front of a slow
    source, and a rate limiter.
- **An `apidiff` check** on every pull request that touches the package.
- **Commands**: `MULTI`, `EXEC` and `DISCARD`, atomic in the log and on
  replicas; the string commands `APPEND`, `STRLEN`, `GETRANGE`, `SUBSTR`,
  `SETRANGE`, `GETDEL`, `GETEX`, `GETSET`, `MSETNX` and `INCRBYFLOAT`, and
  Redis 8's `DIGEST`, `DELEX`, `MSETEX`, `INCREX` and `SET`'s `IFEQ`, `IFNE`,
  `IFDEQ` and `IFDNE`; the key commands `EXPIRETIME`, `PEXPIRETIME`, `TOUCH`,
  `RENAME`, `RENAMENX`, `COPY` and `RANDOMKEY`.
- **RESP3** for connections that ask for it with `HELLO 3`, and the
  connection commands client libraries send on their own.
- **`CONFIG GET`** reports Keel's settings under Redis's names, and
  `CONFIG RESETSTAT` resets the statistics.
- **`INFO`** reports Redis's fields that cost nothing per command, counts
  commands and error replies as Redis does (`errorstats` included), and
  counts keyspace hits and misses as Redis's `lookupKeyRead` does.
- **`-tcp-backlog`**, Redis's `tcp-backlog`, in place of a listen backlog of
  `maxclients`.

### Changed

- `SET` replaces a key of any type, as Redis's does.
- While the server's log fails to write or sync under `everysec` or `no`, it
  refuses writes and `PING` with Redis's `MISCONF` error, serves reads, and
  retries, where it stopped before; under `always` it still stops, as Redis
  exits.
- The server has no key bound by default (`-maxkeys 0`), as Redis has none.
- Generic errors are worded as Redis 8.10.1 words them, in Redis's order;
  `BF.*` and `CF.*` answer exactly as RedisBloom 8.10.1 does, in both
  protocols.
- A failed rewrite of the log leaves the server serving, as Redis's does.
- A short write is cut back off the log where it ends at a whole record, as
  Redis truncates it.
- A write that changes nothing is no longer logged, as Redis's dirty counter
  decides: `DEL` of keys that do not exist, `PERSIST` of a key with no
  expiry.

### Fixed

- A `SPOP` that popped nothing recorded a command that stopped protocol 2
  replicas.
- Migrating a legacy log could publish one that did not yet hold the whole
  keyspace.
