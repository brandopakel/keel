# Automatic failover: design and staging

Status: proposed design, September 6, 2026. Nothing here is implemented. This
document exists to make the decision before the code, because the failure mode
of getting it wrong is two nodes accepting writes at once, and that is not a bug
a test suite finds after the fact.

## What exists today

Role is derived from a flag: a process is a replica if `-replicaof` is set, and a
primary otherwise. Promotion is restarting the replica without that flag. There
is no node identity beyond the address an operator typed, no term, no vote, no
membership, and no fencing. `docs/replication-alpha.md` states the contract
plainly: fencing is the operator's job, and there is no guarantee against split
brain once they remove it.

Protocol 2 adds pieces this design can build on: a primary epoch, a monotonic
replication offset, and a durable replica checkpoint that refuses to resume
against a primary whose identity or history does not match. Those already
encode "this replica knows which primary it was following, and will demand a
full resync rather than stitch itself onto a different one."

## The decision this document exists to make

Two ways to get automatic promotion, and they are not close in cost.

**Build consensus into Keel.** Durable terms and votes, quorum membership and
reconfiguration, election timeouts, and the state machine that ties them
together — a Raft-shaped protocol among Keel nodes, for leadership only.

**Provide fencing and let a coordinator elect.** Keel makes promotion *safe* and
delegates *who wins* to something that already solves consensus: an etcd or
Kubernetes lease, a Consul session, or a quorum of observers in the Sentinel
style. Keel's obligation is that two nodes can never both accept writes, even
when the coordinator is wrong, partitioned, or slow.

**The recommendation is the second.** Not because consensus is beyond the
project, but because in this system it buys less than it costs.

Data replication here is asynchronous. A correct election tells you which node
should lead; it does not tell you that node has the data. Promote a replica that
is 200 ms behind and the writes in that window are gone whether the election was
consensus-grade or a coin flip. Building a consensus protocol whose principal
product is safety, and then bolting it onto a data path that cannot honour that
safety, spends the largest engineering budget in the project on the smaller half
of the problem. The half that actually determines whether a failover loses
acknowledged writes is the replication acknowledgement contract, and that is
independent of how the leader is chosen.

The second approach also fails better. If the coordinator is wrong, fencing
still holds and the cluster stalls rather than diverging. If Keel's own election
were wrong, nothing else is watching.

## What Keel must provide: the term

A single mechanism carries the safety property, and everything else is
bookkeeping around it.

A **term** is a monotonically increasing integer naming a period of leadership.
It is durable, it is fsynced before it is acted on, and it is checked on every
write. The coordinator hands the winner a term; the winner may not accept a
single write until that term is on disk.

The rules are short, and each one exists because of a specific way this goes
wrong:

1. A primary accepts a write only while its in-memory term equals its durable
   term and it holds the role. Checked per mutation, so there is no window
   between losing leadership and noticing.
2. A node that learns of any term higher than its own **stops accepting writes
   immediately** and demotes. It does not wait to be told twice, and it does not
   finish what it was doing. This is what makes a returning old primary safe:
   the first frame it exchanges carries a term above its own, and it stands
   down before it can serve a stale read or take a write.
3. A replica records the term it is following. It refuses frames from a lower
   term outright — that is a deposed primary still talking. A higher term forces
   a fresh full synchronisation rather than an attempt to splice histories.
4. Terms never go backwards on a node, including across restart. A node whose
   durable term file is missing or unreadable refuses to start as a primary. It
   may start as a replica, because a replica takes no writes.

Rule 4 is the one that looks paranoid and is not. A node that loses its term and
guesses zero is a node that will accept writes in a term the cluster has already
moved past.

The term is not the epoch. The epoch identifies a primary's history so a replica
can tell whether its offsets still mean anything; it changes on restart. The
term identifies a period of authority and increases only on promotion. A single
value cannot do both jobs, because a primary restarting must invalidate offsets
without claiming new authority.

## The loss policy, stated rather than implied

With asynchronous replication, a promoted replica may lack writes the old
primary acknowledged. No election protocol changes that. The honest options:

- **Accept the loss and bound it.** Publish the window as a function of
  replication lag, expose that lag, and let operators alert on it. This is what
  Redis does, and it is a legitimate choice provided it is stated rather than
  discovered.
- **Let the application opt into durability.** A `WAIT numreplicas timeout`
  form, where a client can require that a write reached *n* replicas before it
  is acknowledged. This converts the loss window into latency, per write, at the
  application's choice — the right shape, because only the application knows
  which writes are worth it.

The recommendation is to implement both: the first is the default and costs
nothing, the second is what makes "we do not lose acknowledged writes" sayable
for the writes that matter. **Neither should be described as zero loss.** A
guarantee of no acknowledged-write loss on primary failure requires synchronous
commit to a quorum on every write, which is a different system with a different
latency profile, and it is not what this one is.

## Divergence on rejoin

An old primary may hold writes in its log that the new primary never saw. Those
writes are not recoverable and must not be replayed. On rejoin it performs a
full synchronisation and discards its own history.

Protocol 2's checkpoint validation is most of this already: it refuses to resume
when identity, epoch or checksum do not match, and falls back to a full sync.
What must be added is that the discarded log is *preserved*, not deleted — an
operator investigating lost writes needs the file, and a system that silently
destroys the only evidence of what it lost is one nobody can trust after an
incident.

## Client-visible behaviour

- A demoted primary answers writes with an error naming the condition, not a
  connection reset. Clients distinguish "not the leader" from "server down".
- A replica that has not completed synchronisation continues to refuse reads, as
  it does now. Promotion does not relax this: an unsynchronised node that is told
  to lead must complete its catch-up before serving anything.
- Reported role changes at the moment authority changes, not when a flag was
  parsed at startup.

## Acceptance

These are the cases that decide whether the design holds, and none of them is a
happy path. Each must be a deterministic test with controllable timing, not a
sleep — the same standard the append and rewrite fault tests already meet.

| Case | What must hold |
| --- | --- |
| Old primary reappears after promotion | It demotes on first contact; it never serves a write or a stale read |
| Asymmetric partition, A sees B but B cannot see A | No second writer appears; the cluster stalls rather than diverging |
| Process paused past its lease, then resumed | The pause is indistinguishable from death, and the resumed node stands down |
| Two coordinators promote different nodes | The lower term loses; exactly one node ever accepts a write |
| Delayed or reordered frames | A frame from an old term is rejected, not applied |
| Storage rollback loses the term file | The node refuses to start as primary |
| Promotion of a lagging replica | Acknowledged-loss window is measured and reported, not assumed to be zero |
| Coordinator unavailable entirely | The existing primary keeps serving; no promotion occurs |

The last one matters and is easy to get backwards: losing the coordinator must
not cost availability of a healthy primary. The coordinator decides promotions,
not whether the current leader may continue.

## Staging

The order is chosen so that each stage is useful alone and safe to stop after.

1. **Terms and fencing, no automation.** Durable term, write-path check,
   demotion on a higher term, replica term guard, `KEEL.PROMOTE` taking a term.
   Manual promotion becomes *safe* rather than merely documented as dangerous —
   worth having on its own, and it is the whole safety property.
2. **Role transitions without restart.** Promote and demote in place. Today's
   restart-to-promote loses the page cache and the replication history, and
   makes every failover slower than it needs to be.
3. **Acknowledgement contract.** Replication acknowledgement offsets from
   replicas, exposed lag, and `WAIT`. This is what turns the loss policy from a
   sentence in a document into something an application can act on.
4. **Coordinator integration.** A reference integration against one lease
   provider, plus the fault matrix above. Not a bundled coordinator.
5. **Optional, and only if demand shows up.** A bundled observer quorum in the
   Sentinel style, for deployments with no lease service.

Stage 1 carries nearly all the safety benefit and is a small fraction of the
work. If this stops after stage 1, the system is meaningfully better than it is
today: split brain becomes impossible rather than merely discouraged.

## Out of scope

Multi-primary or active-active writes, conflict resolution, cluster sharding and
resharding, cross-region topologies, and consensus over the data path. Automatic
failover as described here elects a leader and fences the loser; it does not
make the data path synchronous, and it does not turn asynchronous replication
into a durability guarantee.
