# Local term guards: restart and rolling upgrades

Unreleased development includes `KEEL.PROMOTE` and `KEEL.FENCE`. They are local
stale-generation guards. They cannot stop an isolated former primary, elect a
leader, change a configured replica's role, or prove exclusive write authority.
The external isolation contract in [the failover design](failover-design.md)
remains unimplemented.

The numeric AOF-adjacent `.term` file records the highest observed term. It is
not a durable grant to a process incarnation. Every nonzero-term primary starts
without write authority and needs a fresh externally assigned higher term after
external fencing. Restarting after PROMOTE 1 and FENCE 2 must not claim term 2.
Reusing an already observed term is refused. An unreadable or damaged existing
term file refuses startup for both configured roles. Removing or rolling back
the file is indistinguishable from older state; an external authority still has
to prevent stale storage from regaining authority.

A higher observation stops live writes before attempting persistence. A failed
file write, sync, rename or directory sync returns an error and leaves the
process unable to write. Repeating an observation retries its pending durable
update; it cannot falsely acknowledge an earlier failed update. A promotion
grants local authority only after persistence succeeds. Term changes are
published atomically to the replication transport reader.

At term zero, protocol 2 retains its old four-argument pull request and omits
the zero term from frames and checksums. Old and new processes can replicate in
either direction, including replica checkpoint recovery and primary restart.
Upgrade the entire replication path before assigning nonzero terms. Nonzero
terms use the extended protocol-2 request/frame; protocol 1 refuses them because
it cannot convey the caller's observed term. An older process must not be used
as a rollback target for a deployment with nonzero terms: it does not implement
these guards. Prepare rollback data and external isolation before enabling them.

The regression suite reproduces the original restart defect, transport read
race, failed-persistence retry and legacy request incompatibility. The process
check `scripts/check-term-upgrade.py` exercises both mixed-version directions at
term zero and candidate-to-candidate nonzero-term restart/regrant. Native CI runs
it on Linux ARM64 and Intel macOS using pinned pre-term source
`a7c6600cece5d21a78794e40e2f59be0e55028d4` built with the same toolchain.

Native run 34153273202 passes those mixed-version and nonzero-term checks on
both platforms at candidate `4170b73bd6bed8f1e4d4c172e5f0eed6cabefcce`. The same
run passes streaming snapshots, checkpoint recovery, ordered append replies,
alpha.3 upgrade/rollback preparation, short mixed failure workloads and Linux
ENOSPC, plus ext4/xfs recovery. Full reports are retained in
`bench/results/term-guard-native-2026-09-07.json.gz`. These short native checks
do not replace long deployment soaks.

The frozen eight-hour and 48-hour soaks started before these term guards merged.
They cannot establish the correctness of this new runtime. No automatic failover
or new release is claimed by this correction.
