# S2 PR body additions (from coordinator, 2026-10-13)

## Improvements over Redis (add)
- **CF.COMPACT is declared a write, and COMMAND INFO reports it as one.** RedisBloom 8.10.1 registers it read-only (`src/rebloom.c` line 1648: `RegisterCommand(ctx, "cf.compact", CFCompact_RedisCommand, "readonly fast", "read");`) and propagates nothing for it. Keel logs and replicates the compaction (the approved departure), so `command_specs_keel.go` declares it `WRITE FAST` with ACL `WRITE CUCKOO`: cluster-aware clients then send it to the primary, the only node that accepts it, where RedisBloom's readonly flag would route it to replicas that refuse it.

## Rebase checklist for S2
- apply scratchpad/s2-keel-specs.diff (seven hand-written specs) to internal/core/command_specs_keel.go
- run TestCommandSpecsAgreeWithTheTables / TestRedisCommandSpecsMatchRedisSource (KEEL_REDIS_SRC)
- plan paragraph: add that COMMAND INFO reports CF.COMPACT as a write
