# PR 31 fencing counterexamples

Review of `d7bf73ef7aa3570c3ffeec5ec982e04a5bae7b87` in
[PR 31](https://github.com/brandopakel/keel/pull/31), separately from the merged
external-fencing design in PR 27. Two added regression checks fail against the
implementation. Their test source and output are retained in
`bench/results/failover-31-counterexamples-2026-09-07.txt`.

1. **An isolated primary remains writable after a successor is promoted.**
   Save node A's state after promotion at term 1, independently promote node B
   at term 2, then evaluate A's unchanged state. Both satisfy `Writable()`.
   Checking the highest locally observed term cannot reject a higher term that
   has not arrived. Protocol propagation does not cover the partition interval.

2. **A fenced primary becomes writable on restart.** Promote at term 1, fence
   at term 2, and restart from that same term file. `LoadTerm` sets `held` to the
   observed term and leaves `fenced` false, converting the successor's term into
   local authority. The persisted term does not preserve the fence/holder state.

The original PR should not merge with that split-brain prevention claim. Retaining
local term bookkeeping does not satisfy the accepted safety contract: managed
nodes must boot non-serving, including after missing/rolled-back local state,
and activation requires verified isolation of all former serving incarnations.
The provider, trusted authority and deployment enforcement are still separate
architecture work. A local command acknowledging FENCE alone cannot prove that
a disconnected or restarting incarnation has been isolated.

These tests model independently held node state and a real durable term reload.
They establish concrete violations of the current implementation; they are not
a completed deployment/provider failure test. The other session's branch and
running processes were left unchanged.

The later revision 815a40f narrows the description to a guard against terms a
node has actually observed and explicitly retracts the split-brain prevention
claim. That addresses the first finding's misleading scope. It does not provide
the external fence. The second counterexample was rerun against 815a40f and
still fails: restarting the fenced primary restores writability. That concrete
durability defect remains before the narrower guard could be accepted.
