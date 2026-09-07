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
correctness and matched workload measurements are reported below. The general
workload harness now includes 4,096-member hash, set and sorted-set reads
(27 scenarios total); all three new cases passed a local smoke check.

The first hosted comparison completed in
[34108762206](https://github.com/brandopakel/keel/actions/runs/34108762206),
b2f1314 against 89857e8, three repetitions of ten seconds per arm. Small reads,
hashes, sets and sorted-set medians stayed near baseline (ratios 1.010, 1.002,
1.005 and 0.997). Pipeline-16, queue and large-list medians were 0.978, 0.986 and
0.964, prompting longer targeted repetitions. Large hash/set measurements
hit generator CPU warnings and cannot establish server capacity. All raw JSON
is retained in `bench/results/collection-matched-2026-09-07.json.gz`.
Hosted Go/race/Docker and native/filesystem checks passed on the runtime commit
821eadb in runs 34108409719 and 34108411991. The subsequent b2f1314 change only
adds the three workload fixtures and their documentation.

The longer targeted comparison in [34110805600](https://github.com/brandopakel/keel/actions/runs/34110805600)
used the same commits, five repetitions of fifteen seconds per arm. Paired
median ratios for pipeline-16, queue and large-list were 0.992, 0.993 and 0.996
(ranges 0.979–1.006, 0.951–1.000 and 0.913–1.044). Median p99 values were
0.695/0.703 ms, 0.367/0.375 ms and 6.271/6.175 ms respectively. No generator
CPU warnings occurred. The first short-run reductions did not persist at the
same magnitude; these results support admission safety with near-baseline
ordinary performance, not a throughput speedup or a strict equivalence bound.
Raw measurements are in `bench/results/collection-targeted-2026-09-07.json.gz`.
