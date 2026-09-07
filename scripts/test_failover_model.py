"""Finite model of grant ordering; external fencing is an explicit assumption.

Run: python3 -m unittest discover -s scripts -p test_failover_model.py -v
This does not test Keel or a provider. It catches protocol ordering mistakes.
"""
from dataclasses import dataclass, replace
import unittest


@dataclass(frozen=True)
class State:
    holder: int = 0
    generation: int = 1
    granted: tuple = (1, 0, 0)
    active: tuple = (True, False, False)
    fenced: tuple = (False, True, True)
    paused: tuple = (False, False, False)
    authority_up: bool = True

    def writers(self):
        return [i for i in range(3) if self.active[i] and not self.fenced[i]]


def changed(values, index, value):
    return tuple(value if i == index else old for i, old in enumerate(values))


def grant(s, candidate, fence_confirmed=True):
    if not s.authority_up or not fence_confirmed:
        return s
    # The authority serializes this transition. Real adapters must implement
    # the durable fence-before-grant ordering, including crash recovery.
    generation = s.generation + 1
    return replace(s, holder=candidate, generation=generation,
                   granted=changed(s.granted, candidate, generation),
                   fenced=tuple(i != candidate for i in range(3)),
                   active=changed(s.active, candidate, False))


def activate(s, candidate, token):
    if (s.authority_up and candidate == s.holder and
            token == s.generation == s.granted[candidate] and
            not s.fenced[candidate]):
        return replace(s, active=changed(s.active, candidate, True))
    return s


def restart(s, candidate):
    # A new incarnation starts non-serving; an old grant cannot authorize it.
    return replace(s, active=changed(s.active, candidate, False),
                   fenced=changed(s.fenced, candidate, True),
                   granted=changed(s.granted, candidate, 0))


class FailoverModelTests(unittest.TestCase):
    def test_local_terms_reproduce_two_writers(self):
        memory_terms = [5, 6]
        durable_terms = [5, 6]
        roles = ['primary', 'primary']
        self.assertEqual(2, sum(roles[i] == 'primary' and
                                memory_terms[i] == durable_terms[i]
                                for i in range(2)))

    def test_fence_failure_preserves_old_holder(self):
        self.assertEqual(State(), grant(State(), 1, fence_confirmed=False))

    def test_pause_after_check_does_not_escape_fence(self):
        s = replace(State(), paused=(True, False, False))
        s = grant(s, 1)
        s = activate(s, 1, s.generation)
        s = replace(s, paused=(False, False, False))
        self.assertEqual([1], s.writers())

    def test_stale_grant_and_restarted_incarnation_cannot_activate(self):
        s = grant(State(), 1)
        old = s.generation
        s = grant(s, 2)
        self.assertEqual(s, activate(s, 1, old))
        s = restart(s, 2)
        self.assertEqual(s, activate(s, 2, s.generation))

    def test_authority_outage_allows_current_primary_only(self):
        s = replace(State(), authority_up=False)
        self.assertEqual([0], s.writers())
        self.assertEqual(s, grant(s, 1))
        self.assertEqual(s, activate(s, 1, 2))

    def test_finite_transition_exploration(self):
        seen, frontier = {State()}, {State()}
        for _ in range(6):
            following = set()
            for s in frontier:
                self.assertLessEqual(len(s.writers()), 1, s)
                transitions = [replace(s, authority_up=not s.authority_up)]
                for i in range(3):
                    transitions.extend((grant(s, i), grant(s, i, False),
                                        activate(s, i, s.granted[i]),
                                        activate(s, i, max(0, s.generation-1)),
                                        restart(s, i),
                                        replace(s, paused=changed(s.paused, i, not s.paused[i]))))
                for t in transitions:
                    self.assertLessEqual(len(t.writers()), 1, t)
                    if t not in seen:
                        following.add(t)
            seen.update(following)
            frontier = following
        self.assertGreater(len(seen), 1000)


if __name__ == '__main__':
    unittest.main()
