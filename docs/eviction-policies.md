# Eviction policies

`-evict` chooses what Keel does when the keyspace reaches `-maxmemory` or
`-maxkeys`. It takes Redis's `maxmemory-policy` names, and `INFO`'s
`maxmemory_policy` and `CONFIG GET maxmemory-policy` report them:

| Policy | What it evicts |
| --- | --- |
| `allkeys-lru` (the default; also `lru`) | Of a sample of every key, the one least recently used |
| `allkeys-lfu` (also `lfu`) | Of a sample of every key, the one least frequently used |
| `allkeys-random` (also `random`) | Any key |
| `volatile-lru` | Of a sample of the keys that carry a TTL, the one least recently used |
| `volatile-lfu` | Of a sample of the keys that carry a TTL, the one least frequently used |
| `volatile-random` | Any key that carries a TTL |
| `volatile-ttl` | Of a sample of the keys that carry a TTL, the one due soonest |
| `noeviction` | Nothing |

The default stays `allkeys-lru`, the owner's choice for embedded use
(`TestOptionsDefaults`), where Redis's is `noeviction`. Redis 8's
`volatile-lrm` and `allkeys-lrm` (least recently modified) are not here yet:
they need a key's last write kept apart from its last read, and every read
and write of every type touches a key the same way today.

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
