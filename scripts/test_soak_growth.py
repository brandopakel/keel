"""The growth bound, its compaction floor, and the state a segment hands on."""
from collections import deque
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
import unittest.mock
from unittest.mock import MagicMock

import soak


class GrowthBoundTests(unittest.TestCase):
    def test_fill_up_then_flat_passes_and_a_ratchet_fails(self):
        growth = soak.GrowthBounds(3.0)
        for cycle, size in enumerate([1000, 2000, 3000], start=1):
            self.assertEqual(growth.observe(cycle, {'log': size}), [])
        self.assertEqual(growth.settled, {'log': 3000})
        self.assertEqual(growth.observe(4, {'log': 8000}), [])
        self.assertEqual(growth.judged_cycles, 1)
        breaches = growth.observe(5, {'log': 9001})
        self.assertEqual([(b['quantity'], b['now'], b['baseline']) for b in breaches], [('log', 9001, 3000)])
        self.assertEqual(growth.breaches, breaches)

    def test_shrinking_after_compaction_is_not_a_breach(self):
        growth = soak.GrowthBounds(3.0)
        for cycle in range(1, 4):
            growth.observe(cycle, {'log': 5000})
        self.assertEqual(growth.observe(4, {'log': 100}), [])

    def test_floor_covers_a_log_the_server_has_not_promised_to_compact_yet(self):
        # Protocol 2 kept the replica's log across restarts; the nine nightly
        # failures had it at 6-15 MiB after warm-up and three times that within
        # ten cycles, all below the 64 MiB the server compacts at.
        floor = 8 << 20
        growth = soak.GrowthBounds(3.0, {'replica_aof': floor})
        for cycle, size in enumerate([5_000_000, 10_000_000, 15_000_000], start=1):
            growth.observe(cycle, {'replica_aof': size})
        self.assertEqual(growth.baseline('replica_aof'), 15_000_000)
        # Warm-up below the floor: the floor is the baseline.
        small = soak.GrowthBounds(3.0, {'replica_aof': floor})
        for cycle in range(1, 4):
            small.observe(cycle, {'replica_aof': 2_000_000})
        self.assertEqual(small.baseline('replica_aof'), floor)
        self.assertEqual(small.observe(4, {'replica_aof': 3 * floor}), [])
        self.assertEqual(len(small.observe(5, {'replica_aof': 3 * floor + 1})), 1)

    def test_primary_swing_between_rewrites_sits_under_the_floor(self):
        # Measured on the nightly runs: 1,886,711 after a checkpoint rewrite,
        # 5,736,391 just before the next one. Without the floor that ratio is
        # the tolerance, and which of the two the warm-up sampled was chance.
        growth = soak.GrowthBounds(3.0, {'primary_aof': 8 << 20})
        for cycle in range(1, 4):
            growth.observe(cycle, {'primary_aof': 1_886_711})
        self.assertEqual(growth.observe(16, {'primary_aof': 5_736_391}), [])
        unfloored = soak.GrowthBounds(3.0)
        for cycle in range(1, 4):
            unfloored.observe(cycle, {'primary_aof': 1_886_711})
        self.assertEqual(len(unfloored.observe(16, {'primary_aof': 5_736_391})), 1)

    def test_carried_baseline_is_judged_from_the_first_cycle(self):
        growth = soak.GrowthBounds(3.0, {'log': 10}, settled={'log': 100})
        self.assertEqual(len(growth.observe(641, {'log': 301})), 1)
        self.assertEqual(growth.judged_cycles, 1)

    def test_floors_only_rise_and_zero_settled_quantities_are_still_judged(self):
        growth = soak.GrowthBounds(3.0, {'tmpfiles': soak.TMPFILE_FLOOR})
        for cycle in range(1, 4):
            growth.observe(cycle, {'tmpfiles': 0})
        # The first version never judged this: settled 0 gave baseline 0.
        self.assertEqual(growth.observe(4, {'tmpfiles': 6}), [])
        self.assertEqual(len(growth.observe(5, {'tmpfiles': 7})), 1)
        growth.raise_floor('log', 100)
        growth.raise_floor('log', 50)
        self.assertEqual(growth.floors['log'], 100)

    def test_compaction_ceiling_is_the_larger_of_min_size_and_six_times_live(self):
        client = MagicMock()
        client.call.return_value = b'# Memory\r\nused_memory:1961240\r\nused_memory_human:1.87M\r\n'
        self.assertEqual(soak.compaction_ceiling(client, 8 << 20), 6 * 1961240)
        self.assertEqual(soak.compaction_ceiling(client, 64 << 20), 64 << 20)
        client.call.assert_called_with('INFO', 'memory')

    def test_growth_values_count_torn_tails_apart_from_temporary_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in soak.SERVER_DIRECTORIES:
                (root / name).mkdir()
            (root / 'primary' / 'store.aof').write_bytes(b'x' * 10)
            (root / 'primary' / 'store.aof.rewrite').write_bytes(b'')
            (root / 'primary' / '.keel-torn-tail-abc').write_bytes(b'')
            (root / 'primary' / '.keel-torn-tail-def').write_bytes(b'')
            (root / 'primary' / 'server.log').write_bytes(b'')
            (root / 'replica' / 'store.aof').write_bytes(b'x' * 20)
            (root / 'replica' / '.keel-replica-checkpoint-tmp').write_bytes(b'')
            (root / 'replica' / 'store.aof.replica-checkpoint').write_bytes(b'')
            values = soak.growth_values(SimpleNamespace(directory=root / 'primary'),
                                        SimpleNamespace(directory=root / 'replica'))
            self.assertEqual(values, {'primary_aof': 10, 'primary_tmpfiles': 1, 'primary_torn_tails': 2,
                                      'replica_aof': 20, 'replica_tmpfiles': 1, 'replica_torn_tails': 0})

    def test_informational_quantities_are_recorded_and_never_judged(self):
        growth = soak.GrowthBounds(3.0, informational=soak.TORN_TAIL_COUNTS)
        for cycle in range(1, 4):
            growth.observe(cycle, {'primary_torn_tails': 1, 'log': 100})
        self.assertEqual(growth.settled['primary_torn_tails'], 1)
        self.assertEqual(growth.observe(4, {'primary_torn_tails': 40, 'log': 100}), [])
        self.assertEqual(growth.describe()['informational'], sorted(soak.TORN_TAIL_COUNTS))

    def test_describe_is_json(self):
        growth = soak.GrowthBounds(3.0, {'log': 10})
        growth.observe(1, {'log': 5})
        json.dumps(growth.describe())

    def test_parse_size_matches_the_server(self):
        self.assertEqual(soak.parse_size('8mb'), 8 << 20)
        self.assertEqual(soak.parse_size(' 64MB '), 64 << 20)
        self.assertEqual(soak.parse_size('1gb'), 1 << 30)
        self.assertEqual(soak.parse_size('512kb'), 512 << 10)
        self.assertEqual(soak.parse_size('4096'), 4096)
        for bad in ('', 'mb', '8 mb x', '-1mb', '1.5mb'):
            with self.assertRaises(ValueError):
                soak.parse_size(bad)


class HandoffTests(unittest.TestCase):
    def test_state_round_trips_exactly(self):
        expected = {f'cache:{n}': f'{n}:'.encode() + b'v' * 256 for n in range(5)}
        events = deque([b'0', b'20'], maxlen=128)
        hashes = {b'f:1': b'1:' + b'v' * 256}
        members = {b'm:1', b'm:2'}
        scores = {b'm:1': 21, b'm:2': 42}
        large = deque([b'L' * 4096, b'500' + b'L' * 4096], maxlen=128)
        encoded = json.loads(json.dumps(soak.encode_state(expected, events, hashes, members, scores, large)))
        decoded = soak.decode_state(encoded)
        self.assertEqual(decoded[0], expected)
        self.assertEqual(list(decoded[1]), list(events))
        self.assertEqual(decoded[1].maxlen, 128)
        self.assertEqual(decoded[2], hashes)
        self.assertEqual(decoded[3], members)
        self.assertEqual(decoded[4], scores)
        self.assertEqual(list(decoded[5]), list(large))
        self.assertEqual(decoded[5].maxlen, 128)

    def handoff(self, directory, **overrides):
        binary = Path(directory) / 'keel'
        binary.write_bytes(b'#!/bin/sh\n')
        payload = {'passed': True, 'segment': 1, 'segments': 9, 'replication_protocol': 2,
                   'concurrent': True, 'primary_crash_every': 3, 'auto_rewrite_min_size': '8mb',
                   'binary_sha256': soak.sha256(str(binary)),
                   'i': 10, 'cycles': 640, 'ports': {'primary': 1, 'replica': 2},
                   'growth_settled': {}, 'growth_floors': {}, 'cumulative': {}, 'state': {}}
        payload.update(overrides)
        (Path(directory) / soak.HANDOFF).write_text(json.dumps(payload))
        return SimpleNamespace(resume=directory, bin=str(binary), segment=2, segments=9, replication_protocol=2,
                               concurrent=True, primary_crash_every=3, auto_rewrite_min_size='8mb')

    def test_handoff_must_be_the_passed_predecessor_with_the_same_shape(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.handoff(directory)
            previous, handoff = soak.load_handoff(args)
            self.assertEqual(previous, Path(directory).resolve())
            self.assertEqual(handoff['cycles'], 640)
            for broken in ({'passed': False}, {'segment': 2}, {'segments': 8},
                           {'replication_protocol': 1}, {'concurrent': False},
                           {'primary_crash_every': 0}, {'auto_rewrite_min_size': '64mb'},
                           {'binary_sha256': 'not the same build'}):
                with self.assertRaises(AssertionError, msg=broken):
                    soak.load_handoff(self.handoff(directory, **broken))

    def test_inherited_directories_leave_the_previous_logs_behind(self):
        with tempfile.TemporaryDirectory() as directory:
            previous, root = Path(directory) / 'previous', Path(directory) / 'root'
            for name in soak.SERVER_DIRECTORIES:
                (previous / name).mkdir(parents=True)
                (previous / name / 'store.aof').write_bytes(b'*1\r\n$4\r\nPING\r\n')
                (previous / name / 'server.log').write_text('gc trace\n')
                (previous / name / 'nested').mkdir()
            (previous / 'replica' / 'store.aof.replica-checkpoint').write_text('{}')
            root.mkdir()
            soak.inherit_directories(previous, root)
            self.assertEqual(sorted(p.name for p in (root / 'primary').iterdir()), ['store.aof'])
            self.assertEqual(sorted(p.name for p in (root / 'replica').iterdir()),
                             ['store.aof', 'store.aof.replica-checkpoint'])
            self.assertEqual((root / 'primary' / 'store.aof').read_bytes(), b'*1\r\n$4\r\nPING\r\n')


if __name__ == '__main__':
    unittest.main()


class StallSweepWaitTests(unittest.TestCase):
    def info_sequence(self, *rows):
        calls = iter(rows)
        def call(*parts):
            row = next(calls)
            return ''.join(f'{k}:{v}\r\n' for k, v in row.items()).encode()
        return call

    def test_waits_until_the_server_reports_a_closure(self):
        probe = MagicMock()
        probe.call.side_effect = self.info_sequence(
            {'clients_closed_slow': 0, 'clients_closed_unanswered': 0},
            {'clients_closed_slow': 0, 'clients_closed_unanswered': 0},
            {'clients_closed_slow': 0, 'clients_closed_unanswered': 1})
        evidence = {}
        with unittest.mock.patch.object(soak.time, 'sleep'):
            soak.wait_for_stall_sweep(probe, evidence)
        self.assertEqual(evidence['sweep_closed_unanswered'], 1)
        self.assertEqual(evidence['sweep_closed_slow'], 0)
        self.assertEqual(evidence['clients_after_sweep']['clients_closed_unanswered'], '1')

    def test_waits_for_a_request_the_loop_never_read(self):
        probe = MagicMock()
        quiet = {'clients_closed_slow': 0, 'clients_closed_unanswered': 0, 'clients_closed_unread': 0}
        probe.call.side_effect = self.info_sequence(quiet, quiet, dict(quiet, clients_closed_unread=1))
        evidence = {}
        with unittest.mock.patch.object(soak.time, 'sleep'):
            soak.wait_for_stall_sweep(probe, evidence)
        self.assertEqual(evidence['sweep_closed_unread'], 1)
        self.assertEqual(evidence['sweep_closed_unanswered'], 0)

    def test_records_nothing_closed_when_the_sweep_stays_quiet(self):
        probe = MagicMock()
        probe.call.side_effect = lambda *parts: b'clients_closed_slow:0\r\nclients_closed_unanswered:0\r\n'
        evidence = {}
        clock = iter(range(0, 200))
        with unittest.mock.patch.object(soak.time, 'sleep'), \
             unittest.mock.patch.object(soak.time, 'monotonic', side_effect=lambda: next(clock)):
            soak.wait_for_stall_sweep(probe, evidence)
        self.assertEqual(evidence['sweep_closed_unanswered'], 0)
        self.assertIn('clients_after_sweep', evidence)
