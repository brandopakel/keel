# Eviction policies

`-evict` chooses what Keel does when the keyspace reaches `-maxmemory` or
`-maxkeys`. It takes Redis's `maxmemory-policy` names, and `INFO`'s
`maxmemory_policy` and `CONFIG GET maxmemory-policy` report them:

| Policy | What it evicts |
| --- | --- |
| `allkeys-lru` (the default; also `lru`) | Of a sample of every key, the one least recently used |
| `allkeys-lfu` (also `lfu`) | Of a sample of every key, the one least frequently used |
| `allkeys-random` (also `random`) | Any key |
| `allkeys-lrm` | Of a sample of every key, the one least recently modified |
| `volatile-lru` | Of a sample of the keys that carry a TTL, the one least recently used |
| `volatile-lfu` | Of a sample of the keys that carry a TTL, the one least frequently used |
| `volatile-random` | Any key that carries a TTL |
| `volatile-ttl` | Of a sample of the keys that carry a TTL, the one due soonest |
| `volatile-lrm` | Of a sample of the keys that carry a TTL, the one least recently modified |
| `noeviction` | Nothing |

The default stays `allkeys-lru`, the owner's choice for embedded use
(`TestOptionsDefaults`), where Redis's is `noeviction`.

## Least recently modified

Redis 8's `allkeys-lrm` and `volatile-lrm` rank keys as LRU does, by a
stamp, but only a modification moves the stamp: a read leaves it where it
was, as Redis's `lookupKey` does under an LRM policy, and a write moves it,
as Redis's `keyModified` does (`db.c`). A key that is only read ages out
before one written since, however often it is read.

What counts as a modification is what Redis stamps:

- storing a value, and changing one in place: each type's writes, which
  store or resize the key or note the change;
- giving a key a TTL, changing it, or removing it (`EXPIRE` and its kind,
  `PERSIST`, `GETEX` with an expiry or `PERSIST`), as `expire.c` calls
  `keyModified` with the value;
- a RedisBloom write that succeeds (`CF.ADD`, `CF.ADDNX`, `CF.INSERT`,
  `CF.INSERTNX`, `CF.DEL`, `CF.COMPACT`, `CMS.INCRBY`, `CMS.MERGE`, and
  Keel's `MORRIS.INCRBY`), as a module key opened for
  writing is stamped when it closes (`module.c`). RedisBloom's failed writes
  are stamped too, there; Keel stamps only those that succeed.

`TestEveryFamilyCountsItsWritesUnderLRM` runs every case of the command
corpus under `allkeys-lrm`. It fails on a write that changed a key's value
or TTL without moving its stamp, and on a read that moved one, so a command
added later is held to it too.

**Not yet:** Redis's LRM tests (`unit/maxmemory`) read the stamp through
`OBJECT IDLETIME`, which Keel does not have yet: its stamp counts accesses,
not seconds. Their eviction test, that keys only read go first, holds
(`TestLRMEvictsWhatWasOnlyReadFirst`).

## Sampling

A volatile policy samples each keyspace's expiry table rather than its keys,
in proportion to how many keys with a TTL each holds, as Redis samples a
database's `expires` rather than its `keys` (`evict.c`,
`performEvictions`). `volatile-ttl` ranks its sample by when each key falls
due, the soonest first, as Redis's `evictionPoolPopulate` does
(`ULLONG_MAX - expire`). A key that has lost its TTL, or been given a later
one, since it was sampled is passed over. Changing the policy clears the
candidates pooled under the old one.

## When nothing can be evicted

Under `noeviction`, and under a volatile policy once no key carries a TTL,
the keyspace stays over its bound. Then, as Redis's `processCommand` does
when `performEvictions` fails (`server.c`), a command whose spec carries
`DENYOOM` is refused:

```
-OOM command not allowed when used memory > 'maxmemory'.
```

- The write that crosses the bound runs, since Redis checks memory before a
  command, not after it.
- Reads, deletes and expiry run, and once they bring the keyspace back
  inside its bound, writes run again.
- A `DENYOOM` command refused as it is queued spoils its transaction, and
  `EXEC` answers `EXECABORT ... previous errors`. An `EXEC` whose queue holds
  one, when memory has gone over since it was queued, is refused whole:
  `-EXECABORT Transaction discarded because of: OOM command not allowed when
  used memory > 'maxmemory'.`
- A replica refuses nothing for memory, as a Redis replica with the default
  `replica-ignore-maxmemory yes` does not, and neither does a replay of the
  log.
- `-maxkeys`, which Redis does not have, bounds the keyspace the same way and
  is refused in the same words.

INFO's `current_eviction_exceeded_time` and `total_eviction_exceeded_time`
report how long the keyspace has been over its bound with nothing to evict,
as Redis's do (`docs/info-compatibility.md`, Stats).

`scripts/error-parity.py` holds these replies to Redis's byte for byte, on a
Keel and a Redis started with `noeviction` and a `maxmemory` of one byte.

**Differs from Redis:** Redis counts its whole process against `maxmemory`,
Keel only its keyspace (`used_memory`, rule 4 of
`docs/info-compatibility.md`), so at the same bound Redis is over it sooner.
Under an `allkeys` policy Keel never refuses: every write evicts until the
keyspace is back inside its bounds, without Redis's spurious refusals while
evictable keys remain (`docs/redis-parity-plan.md`, "no spurious OOM").
