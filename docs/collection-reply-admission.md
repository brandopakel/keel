# Collection reply and removal-record admission

Short commands could produce responses larger than the server's 64 MiB output
limit before admission. The reproduced HGETALL reply was 136,316,836 bytes;
LRANGE/LPOP, SMEMBERS and score-range replies exceeded 68 MiB. A counted pop
could already have removed the members before the server refused its output.

Hash, list and sorted-set visitors now traverse without materializing arrays.
HGETALL/HKEYS/HVALS, LRANGE and rank/score ranges count exact RESP framing before
allocating one output buffer. SMEMBERS and distinct SRANDMEMBER use indexed
members. LPOP/RPOP, SPOP and ZPOPMIN/ZPOPMAX admit the reply before removal.
Random distinct draws can reorder set positions, so they invalidate active
rewrite cursors even when admission fails; logical membership remains intact.

SPOP and sorted-set pops also size the complete canonical SREM/ZREM record,
including the key name, before deletion. The record and temporary member-index
array each have a 64 MiB limit. These resource limits apply in every append mode,
including AOF-off, so oversized commands have consistent behavior. Ordinary
reply types, member ordering, score formatting and empty/null behavior stay
compatible. No new persistence format is introduced.

Fourteen reproduced collection cases now reject with less than 256 KiB allocated
and preserve their members. Separate cases cover a reply that fits while an
8 MiB key makes its removal record too large. Process tests pipeline five
oversized destructive commands followed by PING and verify all members, list
head and sorted score before shutdown and through two AOF restarts in both
barrier and concurrent modes; AOF-off is also checked. Full local Go tests and
vet pass. Raw before/after output is retained in
`bench/results/collection-reply-admission-2026-09-07.txt`.

These are per-command payload, record and metadata limits. The output buffer,
canonical record and parsed input may coexist. Aggregate transient admission,
per-client execution scheduling and a latency bound for an individual large
command remain separate work. Size traversal itself is synchronous. Hosted
correctness and matched workload measurements remain pending.
