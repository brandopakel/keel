"""Tests for check-replication-compatibility.py's frame normalization."""
import importlib.util
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
spec = importlib.util.spec_from_file_location(
    'check_replication_compatibility', Path(__file__).with_name('check-replication-compatibility.py'))
replication = importlib.util.module_from_spec(spec)
spec.loader.exec_module(replication)


def record(*parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n%s\r\n' % (len(p), p) for p in parts)


def parts(*args):
    return [a if isinstance(a, bytes) else str(a).encode() for a in args]


class KeyGroupsTest(unittest.TestCase):
    def test_a_full_frame_keeps_flushdb_first_and_orders_keys(self):
        records = [parts('FLUSHDB'), parts('SET', 'b', '1'), parts('PEXPIREAT', 'b', 9),
                   parts('RPUSH', 'a', 'x'), parts('SADD', 'c', 'm')]
        self.assertEqual(replication.key_groups(records),
                         [parts('FLUSHDB'), parts('RPUSH', 'a', 'x'), parts('SET', 'b', '1'),
                          parts('PEXPIREAT', 'b', 9), parts('SADD', 'c', 'm')])

    def test_a_delta_keeps_each_keys_records_together(self):
        records = [parts('DEL', 'z'), parts('SET', 'z', 'v'), parts('DEL', 'a'),
                   parts('DEL', 'm'), parts('HSET', 'm', 'f', 'v')]
        self.assertEqual(replication.key_groups(records),
                         [parts('DEL', 'a'), parts('DEL', 'm'), parts('HSET', 'm', 'f', 'v'),
                          parts('DEL', 'z'), parts('SET', 'z', 'v')])


class OpaqueRunsTest(unittest.TestCase):
    def test_images_of_one_command_are_ordered_and_operations_are_not(self):
        records = [parts('SET', 'z', 'v'), parts('SET', 'a', 'v'),
                   parts('DEL', 'hll3'), parts('KEEL.RESTORE', 'hll3', 'i3'),
                   parts('DEL', 'hll'), parts('KEEL.RESTORE', 'hll', 'i1'), parts('PEXPIREAT', 'hll', 7),
                   parts('INCR', 'c'), parts('DEL', 'b'), parts('DEL', 'a')]
        self.assertEqual(replication.opaque_runs(records),
                         [parts('SET', 'z', 'v'), parts('SET', 'a', 'v'),
                          parts('DEL', 'hll'), parts('KEEL.RESTORE', 'hll', 'i1'), parts('PEXPIREAT', 'hll', 7),
                          parts('DEL', 'hll3'), parts('KEEL.RESTORE', 'hll3', 'i3'),
                          parts('INCR', 'c'), parts('DEL', 'b'), parts('DEL', 'a')])

    def test_a_restore_without_its_del_is_an_operation(self):
        records = [parts('KEEL.RESTORE', 'b', 'i'), parts('KEEL.RESTORE', 'a', 'i')]
        self.assertEqual(replication.opaque_runs(records), records)


class NormalizeBodyTest(unittest.TestCase):
    start = 1_790_000_000_000

    def test_protocol2_body_keeps_its_order_but_not_map_order(self):
        body = (record('HSET', 'h', 'b', '2', 'a', '1') + record('SET', 'z', 'v') + record('SET', 'a', 'v')
                + record('PEXPIREAT', 'a', self.start + 3 * replication.compat.HOUR_MS + 5))
        self.assertEqual(replication.normalize_body(body, self.start, 'protocol2'),
                         record('HSET', 'h', 'a', '1', 'b', '2') + record('SET', 'z', 'v') + record('SET', 'a', 'v')
                         + record('PEXPIREAT', 'a', 'run+3h'))

    def test_protocol1_body_is_put_in_key_order(self):
        body = record('DEL', 'b') + record('SET', 'b', 'v') + record('DEL', 'a')
        self.assertEqual(replication.normalize_body(body, self.start, 'protocol1'),
                         record('DEL', 'a') + record('DEL', 'b') + record('SET', 'b', 'v'))

    def test_a_snapshot_is_a_log(self):
        body = record('RPUSH', 'l', 'a') + record('RPUSH', 'l', 'b') + record('SET', 'k', 'v')
        self.assertEqual(replication.normalize_body(body, self.start, 'snapshot'),
                         record('RPUSH', 'l', 'a', 'b') + record('SET', 'k', 'v'))


class NamesTest(unittest.TestCase):
    def test_identities_are_named_by_first_appearance(self):
        names = replication.Names()
        self.assertEqual([names('epoch', 'x'), names('snapshot', 's'), names('epoch', 'y'), names('epoch', 'x')],
                         ['epoch-1', 'snapshot-1', 'epoch-2', 'epoch-1'])
        self.assertEqual(names('epoch', ''), '')


if __name__ == '__main__':
    unittest.main()
