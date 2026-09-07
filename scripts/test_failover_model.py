"""Finite external-fencing model; no Keel/provider implementation is implied.

Fence, confirmation, durable grant, release and message delivery are separate
steps. Authority crashes preserve durable metadata and established isolation.
"""
from dataclasses import dataclass, replace
import unittest


@dataclass(frozen=True)
class State:
    holder: int = 0
    generation: int = 1
    granted: tuple = (1, 0, 0)
    grant_boot: tuple = (0, 0, 0)
    issued: tuple = (1, 0, 0)
    boot: tuple = (0, 0, 0)
    complete: tuple = (True, True, True)
    active: tuple = (True, False, False)
    fenced: tuple = (False, True, True)
    paused: tuple = (False, False, False)
    authority_up: bool = True
    authority_intact: bool = True
    pending: int = -1
    phase: str = 'idle'

    def writers(self):
        # A paused writer retains authority; resuming it must not create overlap.
        return [i for i in range(3) if self.active[i] and not self.fenced[i]]

    def authority_available(self):
        return self.authority_up and self.authority_intact


def changed(values, index, value):
    return tuple(value if i == index else old for i, old in enumerate(values))


def request(s, candidate):
    if not s.authority_available() or s.phase != 'idle':
        return s
    return replace(s, pending=candidate, phase='fencing')


def establish_fences(s, succeeds=True):
    if not s.authority_available() or s.phase != 'fencing' or not succeeds:
        return s
    # External isolation persists independently of the coordinator/authority.
    # Local active flags deliberately remain unchanged: isolation is the fence.
    return replace(s, fenced=(True, True, True))


def confirm_fences(s):
    if not s.authority_available() or s.phase != 'fencing' or not all(s.fenced):
        return s
    return replace(s, phase='confirmed')


def persist_grant(s):
    if not s.authority_available() or s.phase != 'confirmed' or not all(s.fenced):
        return s
    generation = s.generation + 1
    return replace(s, holder=s.pending, generation=generation, phase='granted',
                   granted=changed(s.granted, s.pending, generation),
                   grant_boot=changed(s.grant_boot, s.pending, s.boot[s.pending]))


def release_and_issue(s):
    if not s.authority_available() or s.phase != 'granted':
        return s
    node = s.pending
    if (s.active[node] or not s.complete[node] or
            s.grant_boot[node] != s.boot[node] or not all(s.fenced)):
        return s
    return replace(s, fenced=changed(s.fenced, node, False),
                   issued=changed(s.issued, node, s.granted[node]),
                   pending=-1, phase='idle')


def deliver_activation(s, node, token, boot):
    # The message can be delayed until after authority loss or another grant.
    # A node need not have learned the new global generation for fencing to hold.
    if token > 0 and token == s.issued[node] and boot == s.grant_boot[node] == s.boot[node]:
        return replace(s, active=changed(s.active, node, True))
    return s


def restart(s, node):
    return replace(s, active=changed(s.active, node, False),
                   fenced=changed(s.fenced, node, True),
                   boot=changed(s.boot, node, s.boot[node]+1),
                   complete=changed(s.complete, node, False))


def abort_transition(s):
    if not s.authority_available():
        return s
    return replace(s, pending=-1, phase='idle')


def promote(s, node):
    s = persist_grant(confirm_fences(establish_fences(request(s, node))))
    s = release_and_issue(s)
    return deliver_activation(s, node, s.issued[node], s.boot[node])


class FailoverModelTests(unittest.TestCase):
    def test_local_terms_reproduce_two_writers(self):
        self.assertEqual(2, sum(memory == disk for memory, disk in [(5, 5), (6, 6)]))

    def test_fence_failure_cannot_advance_to_grant(self):
        s = request(State(), 1)
        s = establish_fences(s, succeeds=False)
        self.assertEqual(s, confirm_fences(s))
        self.assertEqual(s, persist_grant(s))
        self.assertEqual([0], s.writers())

    def test_crash_at_each_authority_boundary(self):
        actions = [lambda s: request(s, 1), establish_fences, confirm_fences,
                   persist_grant, release_and_issue]
        for boundary in range(len(actions)+1):
            s = State()
            for action in actions[:boundary]:
                s = action(s)
            crashed = replace(s, authority_up=False)
            for action in actions[boundary:]:
                self.assertEqual(crashed, action(crashed))
            recovered = replace(crashed, authority_up=True)
            for action in actions[boundary:]:
                recovered = action(recovered)
            recovered = deliver_activation(recovered, 1, recovered.issued[1], recovered.boot[1])
            self.assertEqual([1], recovered.writers(), boundary)

    def test_pause_after_check_and_delayed_activation(self):
        s = replace(State(), paused=(True, False, False))
        s = promote(s, 1)
        # A resumes with its old local activation still present, after B leads.
        s = replace(s, paused=(False, False, False))
        s = deliver_activation(s, 0, 1, 0)
        self.assertTrue(s.active[0])
        self.assertTrue(s.fenced[0])
        self.assertEqual([1], s.writers())

    def test_competing_coordinators_and_stale_messages(self):
        s = request(State(), 1)
        self.assertEqual(s, request(s, 2), 'only one transition may be pending')
        s = release_and_issue(persist_grant(confirm_fences(establish_fences(s))))
        first = s.issued[1]
        s = promote(s, 2)
        s = deliver_activation(s, 1, first, 0)
        self.assertTrue(s.active[1], 'delayed local work must not assume global knowledge')
        self.assertTrue(s.fenced[1], 'the old holder remains externally isolated')
        self.assertEqual([2], s.writers())

    def test_old_boot_and_partial_snapshot_cannot_activate(self):
        s = promote(State(), 1)
        token, boot = s.issued[1], s.boot[1]
        s = restart(s, 1)
        self.assertEqual(s, deliver_activation(s, 1, token, boot))
        s = persist_grant(confirm_fences(establish_fences(request(s, 1))))
        self.assertEqual(s, release_and_issue(s), 'partial state cannot activate')
        s = replace(s, complete=changed(s.complete, 1, True))
        s = release_and_issue(s)
        s = deliver_activation(s, 1, s.issued[1], s.boot[1])
        self.assertEqual([1], s.writers())

    def test_authority_outage_or_storage_rollback_fails_closed(self):
        for up, intact in [(False, True), (True, False), (False, False)]:
            s = replace(State(), authority_up=up, authority_intact=intact)
            self.assertEqual([0], s.writers())
            self.assertEqual(s, request(s, 1))
        # Restarting the authority process does not repair lost durable history.
        s = replace(State(), authority_up=False, authority_intact=False)
        s = replace(s, authority_up=True)
        self.assertEqual(s, promote(s, 1))

    def test_finite_transition_exploration(self):
        seen, frontier = {State()}, {State()}
        for _ in range(8):
            following = set()
            for s in frontier:
                transitions = [replace(s, authority_up=not s.authority_up),
                               establish_fences(s), confirm_fences(s),
                               persist_grant(s), release_and_issue(s), abort_transition(s)]
                for node in range(3):
                    transitions.extend((request(s, node), restart(s, node),
                                        deliver_activation(s, node, s.issued[node], s.boot[node]),
                                        replace(s, complete=changed(s.complete, node, True))))
                for t in transitions:
                    self.assertLessEqual(len(t.writers()), 1, t)
                    if t not in seen:
                        following.add(t)
            seen.update(following)
            frontier = following
        self.assertGreater(len(seen), 1000)


if __name__ == '__main__':
    unittest.main()
