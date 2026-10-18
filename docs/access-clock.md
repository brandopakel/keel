# The access word: LRU idle time and LFU decay

Status: design, approved by the manager on 2026-10-18 with decision (a) below; the owner may
overrule. No code changes yet. Code waits for #202 (`touchSlow`), and lands after the LRM
branch, which uses the same word. `OBJECT`, `DUMP` and `RESTORE` are generic-family gaps no
branch implements yet; they build on the API below. Phase 4's keyscmds (RENAME, COPY, TOUCH,
RANDOMKEY) reviewed it on 2026-10-18.

## What Redis 8.10.1 does

Each value has a 24-bit `lru` field (`object.h`, `LRU_BITS`). The maxmemory policy decides what
it holds.

- **LRU and LRM policies** hold the LRU clock at the last access (LRU) or the last modification
  (LRM). The clock is Unix time in seconds (`LRU_CLOCK_RESOLUTION` is 1000 ms), masked to 24 bits,
  so it wraps about every 194 days. `serverCron` refreshes it once a tick (`server.lruclock`,
  read by `LRU_CLOCK()`). `estimateObjectIdleTime` is the clock minus the field, in
  milliseconds, allowing for one wrap.
- **LFU policies** hold 16 bits of Unix time in minutes (the last access) above an 8-bit
  logarithmic counter (`evict.c`).
  - On each access, `updateLFU` takes the decayed counter, raises it with probability
    `1/((counter-5)*lfu-log-factor+1)`, never below a base of 0 and saturating at 255
    (`LFULogIncr`), and stamps the current minute.
  - Decay is lazy (`LFUDecrAndReturn`). The counter drops by one for each `lfu-decay-time`
    minutes since the last access, never below 0. It is computed where the counter is read:
    on access, by the eviction pool (whose score is `255 - counter`) and by `OBJECT FREQ`. The
    field is not written back.
  - `lfu-decay-time 0` never decays. The default is 1. The elapsed minutes come from the 16-bit
    field, taking the time to have wrapped at most once (about 45.5 days).
- **A new value** starts with the LRU clock, or at minute and counter `LFU_INIT_VAL` (5)
  (`initObjectLRUOrLFU`). An overwrite keeps the old value's field (`db.c`, `val->lru =
  old->lru`, or the in-place swap). The lookup the write made has already counted an access.
- **Reads**: `lookupKey` updates the field unless the lookup is `NOTOUCH` (`CLIENT NO-TOUCH`,
  `OBJECT`, `TYPE`, ...), and under LRM it leaves the field alone. `keyModified` stamps it under
  LRM. `TOUCH` is a read that counts.
- **`OBJECT IDLETIME key`** gives the idle time in seconds. Under an LFU policy it is an error:
  `An LFU maxmemory policy is selected, idle time not tracked. Please note that when switching
  between policies at runtime LRU and LFU data will take some time to adjust.`
- **`OBJECT FREQ key`** gives the decayed counter. Under any other policy it is an error: `An LFU
  maxmemory policy is not selected, access frequency not tracked. Please note ...` (the same
  sentence).
- **`RESTORE ... IDLETIME s` and `FREQ n`** set the field (`objectSetLRUOrLFU`): FREQ only
  under LFU, IDLETIME only otherwise. Each is ignored under the other kind of policy.
- **Switching policy** at runtime does not convert the fields. A field written under one kind is
  read as the other, hence the "will take some time to adjust" in both errors.

Redis's own tests read these: `unit/maxmemory` (a new key's `OBJECT FREQ` is 5, a new key's
`IDLETIME` is at most 2, and the LRM idle-time tests), `unit/introspection-2` (`TOUCH` and
no-touch against `IDLETIME`) and `unit/dump` (`RESTORE IDLETIME` and `FREQ`).

## What Keel does today

`data_structure.Space` keeps one `uint64` access word per key and a space-wide `clock`.

- **The clock is logical.** It goes up by one on every access anywhere in the space (`Touch`).
  An LRU word is the clock at the key's last access, which orders accesses exactly, ties
  included.
- **The LFU word** is `clock<<8 | counter`, with Redis's logarithmic increment and initial 5.
  Decay is by **accesses, not minutes**: one point per `LFUDecayPeriod` accesses anywhere in the
  space (default 10,000, `Off` never). This was a deliberate, measured choice (`space.go`'s
  table). It keeps eviction reproducible and ties forgetting to how busy the cache is.
- **`lfu-decay-time` is not answered.** `CONFIG GET` returns nothing for it, and `CONFIG SET`
  refuses it as an unknown option (`config_test.go`). The CLI has `-lfu-decay-period`, in
  accesses.
- **A new key's `Put` touches** the word that `NewAccess` gave it. Under LFU, a counter at 5
  always rises on its first touch, so a new key's counter is 6 where Redis's is 5.
- **An overwrite (`Put`) replaces the word.** Under LFU this drops the key's history, where Redis
  keeps the old field.
- **No `OBJECT IDLETIME` or `OBJECT FREQ` yet** (generic family, not yet assigned). The word
  holds no time, so IDLETIME could not be answered from it.

## The gaps

Each of these is visible on the wire.

1. `CONFIG GET/SET lfu-decay-time` is refused. Redis accepts 0 to INT_MAX, default 1.
2. LFU counters decay with traffic, not with idle minutes. `OBJECT FREQ` (once it exists) and
   which keys LFU evicts both drift from Redis's over time.
3. A new key reports 6, not 5. Redis's `unit/maxmemory` asserts 5.
4. An overwrite under LFU forgets the key's frequency.
5. There is no time in the word, so `OBJECT IDLETIME` cannot report seconds, nor can the LRM
   tests that read it.

## Proposal

### A time-anchored clock: idle time with today's order

Keep `clock` as the LRU and LRM word, still raised by one on every access, so that `Touch` stays
the same instructions (`clock++`, store) and keeps its inline cost of 73 (budget 80). Once a turn,
raise it to the current time:

    clock = max(clock, nowMs << 16)

The turn is where `SampleCommandRate` already runs: the server's loop, and `Open`'s driver in
`maintain`.

- **Order** stays exact. The clock never goes back, and accesses within a turn still count up
  from the turn's time.
- **Idle time** is `(nowMs - word>>16) / 1000` seconds. The low 16 bits order up to 65,536
  accesses within a millisecond. A busier turn carries the clock a few milliseconds ahead of
  real time, which the next raise absorbs. The error is far below `OBJECT IDLETIME`'s one-second
  unit.
- **Range**: `nowMs<<16` needs about 57 bits today, so there is no wrap in practice, where
  Redis's 24-bit clock wraps about every 194 days.
- **Tests** get an `AccessClock` hook, as `ExpiryClock` is for expiry, so that idle time is
  deterministic.

### LFU in minutes, decayed as Redis decays it

- **The word** becomes `minutes<<8 | counter`, as Redis's is. `minutes` is the space's cached
  turn time in minutes. It is not shifted from the LRU clock, which would overflow 64 bits.
- **Decay** is computed lazily wherever the counter is read, as `LFUDecrAndReturn` computes it:
  one point per `lfu-decay-time` minutes since the last access, and none for 0. That covers the
  touch (in `touchSlowly`, already out of line under #202), `Score` and `OBJECT FREQ`.
- **No 16-bit wrap.** The minutes keep 56 bits, so a key idle for more than about 45 days decays
  by the full time. Redis's wrapped arithmetic under-decays it. That is an improvement no client
  can rely on, listed as one.
- **A new key** keeps `NewAccess`'s word, with counter 5 and no touch on its first store, so
  `OBJECT FREQ` reports 5.
- **An overwrite** keeps the old word. The command's own lookup has counted the access, as
  Redis's does.

### Settings

- **`lfu-decay-time`** becomes live: Keel answers for it in `CONFIG GET` and `CONFIG SET`, with
  Redis's range and default. Options gain `LFUDecayTime` (minutes; zero is the default 1, `Off`
  never decays, as Redis's 0 does).
- **Decided (a): Keel's access-counted decay becomes an opt-in.** It measured well and keeps
  eviction reproducible. The two ways were:
  - **(a) Chosen.** Redis's minutes become the default. Accesses stay as an opt-in mode:
    `-lfu-decay-period` / `Options.LFUDecayPeriod`, off by default, documented as a Keel
    improvement. Clients and tools that set `lfu-decay-time` get Redis's behaviour, and an
    operator who wants reproducible LFU can choose it. It is listed under "Improvements over
    Redis" with `space.go`'s measurements, as is the minutes field that does not wrap.
  - **(b)** Keep accesses as the default and accept `lfu-decay-time` without effect. This is
    cheaper, but `OBJECT FREQ` and LFU eviction then differ from Redis by default, which the
    owner's "follow Redis when in doubt" rule argues against.

### What `OBJECT`, `RESTORE` and `RENAME` need from the space

The space would offer:

- `AccessOf(key) (word uint64, ok bool)`, the raw word without a touch. `Keyspace` gains it
  beside `ScoreOf`, and `SetAccess(key, word)` for RESTORE.
- `IdleMillis(word) uint64` and `Frequency(word) uint8`. Frequency is decayed and does not write
  the word back.
- `IdleWord(seconds)` and `FrequencyWord(n)`, to build the word RESTORE's IDLETIME and FREQ set.
  Each applies only under its kind of policy, as Redis's `objectSetLRUOrLFU` does.
- **`OBJECT` looks the key up with `Has`, then reads `AccessOf`.** Redis's lookup for OBJECT
  (`kvobjCommandLookup`, object.c) is `NOTOUCH|NONOTIFY`: it leaves the field alone, but still
  removes an expired key, counting it in `expired_keys`, and still counts the keyspace hit or
  miss. `Has` reaps and counts the expired key without touching. `Peek` would answer for an
  expired key and count nothing, which `scripts/stats-parity.py` would catch. The engine counts the
  hit or miss as for any read. `OBJECT` refuses under the other kind of policy, in Redis's words
  above.
- **`RENAME` keeps the source's word.** Redis's `renameGenericCommand` (db.c) touches the source
  in its write lookup, then moves that same object to the new name, field and all. Keel's Rename
  touches the source with Get, takes its word with `AccessOf`, deletes it, puts the value at the
  new name, then `SetAccess`es the word. Otherwise `OBJECT FREQ` after a RENAME reads 5, where
  Redis reads the source's counter. Phase 4 wires this into keyscmds once the API is on develop,
  or this change does if keyscmds lands first. A test that RENAME keeps `OBJECT FREQ` and
  `IDLETIME` belongs with `OBJECT`.
- **Already as Redis, no change:**
  - `COPY` makes a new object, which starts with a new key's field, as Redis's duplicate does.
  - `TOUCH` is a read that counts, through Get. Under LRM it leaves the word alone, as any read
    does.
  - `RANDOMKEY` samples without touching, as `dbRandomKey` does.

### Hot path

- `Touch` and `TouchModified` are unchanged in shape. The new work is one compare and store a
  turn, plus arithmetic that already sits in `touchSlowly` (LFU) or runs only when a command
  reads the word.
- Check `go build -gcflags=-m ./internal/data_structure`. `Touch` must still say "can inline".
- Run the paired command-path job, and the nm layout check, before review.

### Order

1. #202 merges (`touchSlow`).
2. LRM, which uses the LRU word as it stands.
3. This change: the clock, LFU in minutes, `lfu-decay-time`, new-key and overwrite parity, and
   the API above. keyscmds and this change both add methods to `Keyspace`, so whichever lands
   second resolves a small interface conflict.
4. `OBJECT`, `DUMP` and `RESTORE` (generic family) on the API, and keyscmds' RENAME carrying the
   word with `SetAccess`.
