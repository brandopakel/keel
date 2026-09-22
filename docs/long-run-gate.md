# The long-run gate

The release checklist asks for 48 hours of continuous operation under crash
recovery. Three attempts on a laptop never completed, and the nightly soak that
PR #68 introduced in their place failed every night from September 9 to
September 17, 2026 - nine runs, 21 of 27 arm-jobs - on the growth bound it added.
This records what those failures were, what changed, and how the 48 hours are
now run without a machine that can be kept for two days.

## What the nightly failures were

Every failure was the same assertion, in two shapes. Neither was a leak.

**The replica's log under protocol 2** (18 of 18 arm-jobs, deterministic, five
minutes in). The bound was three times the largest value from cycles 1-3, which
put it at 18-45 MiB. The server does not compact a log below
`-auto-aof-rewrite-min-size`, then 64 MiB by default, and after a restart it
waits until the log has also doubled against a live-data estimate
(`internal/core/aof.go`). Under protocol 2 the replica keeps its log across
restarts, so it climbed through the bound while doing exactly what it was
configured to do. Because the run died first, the soak had never observed the
replica compact at all - the property the check was written to protect.

**The primary's log under protocol 1** (3 of 9 nights, at cycle 16, always
3.02-3.06x). The primary is rewritten at every 30-second checkpoint, so its log
swings between about 1.9 MiB and 5.7 MiB. The early cycles sampled the trough;
the checkpoint and recovery clocks then drift apart until a sample lands just
before a rewrite. The natural ratio between the two is the tolerance.

PR #68's validation, a 180-second run, could not have caught either: with the
default 60-second cycles that is at most three cycles, and the bound skips the
first three. The first nightly run was the first time it was ever evaluated.

## What changed in the harness

- Both servers run with `-auto-aof-rewrite-min-size 8mb`, so a multi-hour run
  compacts each log hundreds of times rather than never. The replica's
  compactions are counted in the report (`replica_compactions`), across its
  restarts, so "compaction happened" is a number rather than an inference.
- A log's baseline is floored at the server's compaction ceiling,
  `max(min size, 2 x 3 x used_memory)`, measured from the live data every
  cycle and only ever raised. Below that, growth is the contract; three times
  it is still crossed within a few cycles by a log that stops compacting. For
  this workload the ceiling is about 11.8 MiB, so the bound sits near 35 MiB
  against a healthy peak near 12 MiB.
- Temporary files have a floor of two. A quantity that settles at zero was
  never judged by the first version, so the descriptor half of the check had
  been a no-op. Torn-tail backups, which the server keeps on purpose after a
  crash tore the last record, are counted separately and not bounded.
- A run whose growth bound never judged a cycle fails rather than passing. The
  report records `growth.judged_cycles`.
- Every recovery cycle's measurements go to `recoveries.jsonl`, not only a
  breach, and each checkpoint carries the replica's persistence section.

Locally (M-series laptop, 2 MiB floor, replica restarted every cycle) the
replica did not compact until the log reached six times live memory - the
restart estimate, not the floor, decides - which is why the floor is the
ceiling and not the flag's value.

## The stall that motivated the gate, and what closes it

The one 48-hour attempt that ran long enough to stop was read at the time as a
server fault: a lost readiness registration leaving the event loop parked with
a reply owed. The preserved dump and the harness source that ran say otherwise
- the loop was being woken at 10 Hz, and the harness's three-second socket
timeout means it was never waiting on the server. It was asleep in a call of
its own with no deadline, most likely `ps`, which failed the same way on the
same laptop three days later. The [investigation](soak-progress-observability.md)
carries the evidence.

That withdraws a claimed defect; it does not prove there is none, and the
loop had a real gap in how it would have shown one. Its sweep closed
connections holding incomplete input or an undelivered reply for thirty
seconds, silently, and did not look at a connection whose request it had
parsed and never answered - the one state that means the loop forgot someone.
A recurrence would have been a hang with nothing in the log. Three things now
make the property enforceable rather than argued:

- `sweepStalledClients` closes every stalled state within thirty seconds,
  including a parsed run never executed, a held or deferred run in the
  ordered-append queue, and a queued pipeline continuation. An unanswered
  request is **logged with the connection's full state** - parsed commands,
  reply bytes, held/deferred/queued flags, registered interest, age - while
  that state still exists. A slow reader is closed and counted, not logged.
- `INFO clients` reports `clients_closed_slow` and `clients_closed_unanswered`.
  The second is the server saying it stopped serving someone. The soak asserts
  it is zero on both servers at every checkpoint and every recovery cycle, so
  a recurrence fails the run within a minute and the server log names the
  connection.
- `TestConcurrentAppendAnswersEveryRequest` drives the ordered-append path
  from six connections at once with every request shape that moves its state
  machine - admitted and unmodelled runs, rewrites under traffic, keys expiring
  on their own, unknown commands, hundred-command pipelines, a reply larger
  than the arena - each under a five-second deadline, and checks the counter.

The gate matters for the reason it always did - slow growth and elapsed-time
effects - and a connection the loop forgets can no longer hide behind it.

### It happened, twice, and the first version of the check let it get away

On September 20, 2026, two hosted Linux runs of protocol 2 with concurrent
appends stopped serving their existing connections while still answering new
ones. In the nightly's recovery arm (run 35499515760), 22 seconds after a
primary crash-restart, the harness's `RPUSH` went unanswered for three
seconds and the replica's link to the same primary timed out in the same
second; the SIGQUIT dump shows the loop idle in `epoll_wait`, no append
worker, an empty buffer, every AOF offset equal - and a fresh connection
(the harness's diagnostic probe) answered normally. Two minutes into segment
4 of the chain's continuous-primary arm (run 35432168246) the same thing.
The Oracle machine ran the continuous-primary shape for 48 hours with no
occurrence; the hosted runners have four vCPUs to its two. Roughly one
arm-run in six on hosted Linux.

What the record did not contain was the connection's state, because the
harness SIGQUITed the server four seconds after its own timeout, before the
server's thirty-second sweep would have closed the connection and said
whether it was parsed-and-unexecuted, held, deferred, queued - and now,
whether the kernel still had its registration. Three changes:

- The harness waits out the sweep before killing anything, polling `INFO
  clients` over a fresh connection, and records what the server closed
  (`failure_diagnostics.*.sweep_closed_unanswered`).
- The sweep asks the multiplexer to forget the descriptor before closing it
  and logs `registered=true|false`: the difference between a registration
  the loop lost and a client its own queues forgot.
- `liveness-hunt.yml` runs many fifteen-minute samples of both protocol 2
  arms in parallel on hosted Linux, so a hit arrives in an hour rather than
  a night.

The two runs' evidence (reports, both server logs with dumps, checkpoints)
is retained locally at `/tmp/keel-check/nightly-0920` and in the run
artifacts. Until a hit with the named state is in hand, this is an open
server-side liveness defect on the protocol 2 concurrent-append path under
hosted Linux timing, and the strongest reason not to make an availability
claim for that path.

## How 48 hours run on free runners

`long-soak.yml` runs each of the three soak shapes as a chain of nine segments
of 5h20m. A segment inherits both servers' logs and the replica's checkpoint
from the previous one, along with the harness's record of every value it was
ever acknowledged (`handoff.json`), verifies all of it on the new machine
before writing anything, runs the workload, and hands on. The final segment
promotes the replica, as the single-run soak does. Cumulative counters
(`cumulative` in each report) roll up across segments. The chain builds the
binary once and every segment checks its hash, and the harness refuses a
handoff from a different binary.

What this establishes: 48 hours of one dataset and one pair of logs under
continuous writes, crash recovery, compaction and eight planned restarts on
nine machines, with every acknowledged value verified at every boundary and at
every cycle. Growth is bounded by the check above at every cycle throughout.

What it does not establish: 48 hours of process uptime. A defect that appears
only after five hours of one process, and that neither the growth bounds nor
the crash cycles expose, is not covered. The nightly soak's "continuous
primary" arm covers 4.5 hours of it; the chain's boundaries restart both
servers, and the replica takes a full snapshot at each one because the
primary's replication history does not survive its restart. Say this when
citing the result.

The chain runs on dispatch. About 146 runner hours per run, on standard
runners in a public repository, which cost nothing.

## Schedule after the gate was met

The 48-hour uptime gate passed twice: run 35352473161 (September 18 to 20,
2026) and run 35502854458 (September 20 to 22), each one primary process for
48.00 hours with no growth breach and no unanswered closure. Repeating it
weekly re-proves the same thing, so on September 22 both 48-hour forms - the
Saturday segment chain and the Sunday uptime run - stopped being scheduled.
They remain one dispatch away, and a passing earlier build does not validate
a later one: before a release, the uptime run is dispatched on the release
candidate's commit and its result is cited with the release.

The nightly three-arm soak stays scheduled. It runs on GitHub-hosted runners,
not on any maintainer's machine, and it is the only place the open protocol 2
liveness stall has reproduced.

## True uptime, when a machine exists

`scheduled-soak.yml` has an `uptime` job: 48 hours of the continuous-primary
shape on a self-hosted runner, the only shape for which process uptime means
anything (the recovery shapes restart their primary every third cycle by
design). It runs when dispatched with `uptime=true` and a `runner` label. Any
arm can also be dispatched onto that runner with the `runner`, `seconds` and
`timeout_minutes` inputs; self-hosted jobs may run for five days.

The machine that costs nothing is an **Oracle Cloud Always Free** instance.
Two shapes qualify: the Ampere **VM.Standard.A1.Flex** (up to 4 OCPUs and
24 GB, arm64) and the AMD **VM.Standard.E2.1.Micro** (1 OCPU, 1 GB, x86; two
allowed). The A1 is the better machine and the scarcer one: on September 17,
2026 it was "Out of capacity" in the only availability domain of the San Jose
home region, so the runner went on a Micro, which is enough for two small
servers and the harness once the bootstrap adds swap for the Go build, and
which is the hosted runners' architecture besides. The A1 can be tried again
at any time; both can be held at once.

Nobody can create the account but its owner - it needs identity, a card for
verification only, and a phone - so these are the steps, in order, with the
console's quirks as met:

1. Sign up at cloud.oracle.com. The home region is where Always Free resources
   live and cannot be changed later.
2. **Create the network first**, with Networking → Overview → *Create a VCN
   with internet connectivity* (the wizard). It makes a public subnet, an
   internet gateway, the route and a security list that admits SSH. The
   instance form's own "create new VCN" option leaves the public-IP toggle
   disabled, and its VCN list does not refresh while the form is open.
3. Compute → Instances → Create instance. Change the **image** before the
   shape: the full Ubuntu 24.04 image is x86 only, so an A1 needs *Canonical
   Ubuntu 24.04 Minimal aarch64* (search "aarch64"); a Micro takes the full
   image. Then *Change shape*: A1 is under Ampere and lets the row expand to
   set OCPUs and memory; the Micro is under *Specialty and previous
   generation*. Both carry the "Always Free-eligible" badge; nothing without
   it. If the dialog shows no shapes at all, close and reopen it.
4. Networking: *Select existing* VCN and its public subnet; the public IPv4
   toggle turns on by itself. SSH keys: *Paste public key* - generate a pair
   locally (`ssh-keygen -t ed25519 -f ~/.ssh/keel-soak`) rather than
   downloading Oracle's private key. Storage: default. Create. "Out of
   capacity" means retry later or take the other shape.
5. On the instance, as root, with a registration token that
   `gh api -X POST repos/brandopakel/keel/actions/runners/registration-token -q .token`
   prints (valid one hour; nothing long-lived stays on the machine):

       git clone https://github.com/brandopakel/keel.git
       sudo ./keel/scripts/provision-soak-runner.sh --repo brandopakel/keel --token <token>

   It adds swap on a small machine, installs the runner from GitHub's release
   verified against the published checksum, and registers it as a service
   under an unprivileged user with the label `soak`.
6. A 48-hour run is
   `gh workflow run scheduled-soak.yml -f uptime=true -f runner=soak`,
   and a short check that the runner works is any ordinary dispatch with
   `-f runner=soak -f seconds=600`, which runs the three arms one after another
   there.

One Ubuntu rule learned the hard way: `unattended-upgrades` runs daily and
`needrestart` restarts any service that maps an upgraded library, the runner
included, and a restarted runner cancels the job it is running. The first
48-hour attempt (run 35278409128) ended that way at 9.09 hours on September
18 with every metric healthy - 1.82 million writes, 80 replica compactions,
105 judged cycles, no breaches, no unanswered requests, primary RSS flat at
50-53 MB. The bootstrap now exempts the runner from `needrestart` and turns
off automatic reboots; security updates stay on. A cancelled job's `if:
always()` steps do not run, so its evidence is not uploaded either; what the
harness wrote stays on the machine under the runner's work directory.

Two Oracle rules to know. Always Free compute is reclaimed after seven days
in which CPU, network and memory all stayed under 20% at the 95th percentile.
With the weekly 48-hour run no longer scheduled the machine sits idle between
releases and may be reclaimed; a reclaimed instance is recreated from step 2
before the next release candidate's run. And a shape other
than A1.Flex within the free limits, or a boot volume past the allowance,
bills; the console marks free-eligible choices.

Self-hosted runners on a public repository run workflows from forks unless
approval is required, so the repository's Actions setting now requires
approval for every outside collaborator's workflow, and the default workflow
token is read-only. The runner is persistent rather than ephemeral because a
schedule has to find it; those two settings are what make that acceptable.
Google Cloud's Always Free `e2-micro` (1 GB, x86) and any always-on machine
you already own work with the same script.

## Reading the evidence

Each segment's artifact holds `report.json`, `checkpoints.jsonl`,
`recoveries.jsonl`, both `server.log`s, and - for a passing segment - the two
logs and checkpoint the next segment took over. `report.json` carries
`growth` (floors, settled values, judged cycles, breaches),
`replica_compactions`, `handoff_recovery` (how long the takeover verification
took and what the replica reported about it), and `cumulative`. A chain's
result is the final segment's report of each arm; anything else is a partial
run, and a partial run is not a pass.
