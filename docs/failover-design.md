# Automatic failover: external fencing before activation

Status: proposed contract, September 7, 2026. Automatic failover and
`KEEL.PROMOTE` are not implemented. The executable model in
`scripts/test_failover_model.py` tests this contract's ordering assumptions; it
is not an implementation or a test of a real fencing provider.

## Safety property and counterexample

At most one node incarnation may expose an authoritative read or accept an
acknowledged write for a deployment. A node incarnation is one process boot,
identified independently of a restorable data directory.

Matching a node's in-memory and durable terms does not establish this property.
If primary A at term 5 is partitioned but still reachable by clients, promoting B
at term 6 does not teach A about term 6. Both can accept writes. Learning a higher
term and then demoting only protects nodes that receive the message.

Terms remain useful for rejecting obsolete replication histories and stale
activation requests. They are not a fence. The previous proposal's claims that
local terms alone made manual promotion safe were incorrect.

## Chosen authority mechanism

The initial design uses a trusted external **fencing authority**. The coordinator
selects a candidate; the authority enforces exclusivity. The authority has a
durable, linearizable record of grants and node incarnations. An arbitrary term
supplied by a coordinator is never an activation credential.

Before issuing a new activation grant the authority must:

1. Serialize the transition against every other promotion request.
2. Revoke every previously issued grant that could still serve this deployment.
3. Establish and verify an external fence against those incarnations. An
   implementation can stop a VM and prevent its restart, or isolate every data
   endpoint including established connections while keeping that isolation in
   force. A failed health check, lost lease, SIGTERM request, changed service
   route or coordinator assertion is not confirmation of a fence.
4. Persist the new generation, recipient node identity and boot identity, then
   issue an authenticated, deployment-specific activation grant.

The order is fence confirmation, durable grant, activation. If any fence cannot
be confirmed, no successor activates. Safety may cost availability. A provider
whose isolation does not cover all client paths cannot implement this contract.
Deployment credentials must prevent clients/operators outside this authority
from independently restarting or un-fencing old writable instances.

Keel in managed-failover mode starts non-serving on every boot. It verifies an
activation grant for its current boot with the authority; it never restores
write authority from an AOF, a term file or a cached token. A duplicate request
for the same grant is idempotent. Replayed grants for an old boot, old generation,
other deployment or other node are rejected. Authority storage rollback is a
fail-closed disaster-recovery event, not permission to reissue old grants.

The already active primary may continue serving if the coordinator or authority
is unavailable: its authority is not a locally expiring lease. A successor can
activate only after the old incarnation is externally fenced. An old process
paused during promotion therefore resumes behind the fence, including if it was
paused after its last local authority check. Removing the fence requires a new
non-serving incarnation and a fresh grant; it cannot revive the old process.

This design trusts the fencing authority, its durable state and the isolation
mechanism. It does not promise safety if that authority lies, loses its durable
history and continues, or if a privileged actor bypasses its fences. A faulty
coordinator can request unnecessary failovers and damage availability, but it
cannot fabricate authority or cause two grants to become simultaneously usable.

## Why a provider name is insufficient

An etcd/Consul/Kubernetes election can select a coordinator or serialize requests.
It does not by itself disable an isolated Keel process. Kubernetes client-go's
leader-election contract explicitly does not guarantee fencing:
https://pkg.go.dev/k8s.io/client-go/tools/leaderelection.

A future lease-based alternative needs a separate proof covering authority
expiry, clock-rate assumptions, arbitrary process pauses and the check-to-use
interval at both mutation and reply publication. It cannot replace the external
fence in this design merely by adding a deadline check. No lease implementation
is selected by this document.

## Data state and acknowledged loss

Protocol 2 provides history identity, offsets and validated completed-state
checkpoints. Its epoch is a history identifier, not an authority credential.
Replication remains asynchronous. A valid activation grant does not certify that
a candidate has every acknowledged write.

The candidate must have a complete validated local state before it can activate.
A partially installed snapshot is ineligible. The authority must not require
catching up to an unreachable failed primary as an impossible universal
precondition; the promotion policy instead specifies an eligible replicated
prefix and an explicit allowed-loss policy. Record the selected prefix, observed
lag and missing acknowledged fixture writes in failure tests. Preserve divergent
old logs for investigation before replacing old state on rejoin.

The proposed `WAIT` feature is a separate connection-scoped acknowledgement
operation. Define received, applied, appended and fsynced replica offsets
separately. A Redis-compatible WAIT returns how many replicas acknowledged earlier
writes from that connection, including on timeout; it does not retroactively
hold or revoke the original SET reply. A different gated-write API must be named
and specified separately. WAIT alone does not guarantee failover preserves a
write: candidate selection, durable storage and fencing still matter.
https://redis.io/docs/latest/commands/wait/.

Do not claim zero acknowledged loss from asynchronous replication or from adding
WAIT. A stronger commit guarantee requires a separately specified acknowledgement,
recovery and promotion protocol that preserves the committed prefix.

## Implementation sequence and acceptance

1. Specify one real fencing-provider adapter and its trusted deployment boundary.
   Prove its fence covers live connections, paused processes and restart paths.
   Until then, existing manual promotion still requires operator fencing.
2. Add managed-failover mode with non-serving startup, boot identity, authenticated
   grant validation and durable monotonic generations. Ordinary startup flags
   cannot bypass that mode's grant requirement.
3. Add in-place role changes and stale-generation replication guards. An old
   primary's divergent log is preserved before full replacement on rejoin.
4. Add optional connection-scoped replication acknowledgement and observable lag,
   with timeout and promotion-eligibility behavior documented independently.
5. Integrate one coordinator/provider and execute the full fault matrix below.

| Failure | Required behavior |
| --- | --- |
| A isolated from coordinator but reachable by clients | B cannot activate until A's data paths/process are externally fenced |
| A paused immediately after an authority check | Fence still excludes A when it resumes |
| Two coordinators propose different successors | Authority serializes grants and fences any former holder before the next activates |
| Fence request fails or confirmation is lost | No successor activates; retry reconciles durable authority state |
| Authority crashes before/after fence or grant persistence | Recovery cannot create overlapping authority; incomplete transitions fail closed |
| Old grant delayed until after a newer grant | Old incarnation remains externally fenced and cannot serve, even if delayed local activation runs |
| Term/data directory restored from backup | New boot starts non-serving and cannot reuse a previous activation grant |
| Authority durable state rolls back | Authority refuses grants until an externally fenced recovery establishes fresh state |
| Coordinator or authority unavailable | Existing unfenced primary can serve; no unverified promotion |
| Candidate has incomplete snapshot | Activation refused |
| Candidate is complete but lagging | Apply explicit loss policy and report observed loss; no inferred zero-RPO claim |
| Old primary rejoins | Preserve divergent log; replace state as a replica; do not replay its writes into the winner |

The model explores finite transitions including partitions, pauses, failed
fences, delayed/replayed activation, authority outage/recovery, incarnation
restart and competing successors. Provider integration still needs independent
fault injection around each real external action and persistence boundary.

No cloud resources are needed to review or test the model. Provisioning a fencing
provider, native HA deployment, multi-primary operation, cluster sharding and
consensus over the data path are not implemented by this design change.


The finite model deliberately permits a node with an old, authentic grant to
set its local active flag after it has lost authority. It does not give nodes an
instantaneous oracle for the authority's current holder: a stale message or a
process paused after validation can still reach that local instruction. The
asserted safety property is that externally unfenced writers never overlap.
Local stale-generation checks remain useful defense in depth once a node learns
new state, but they do not replace the external fence.
