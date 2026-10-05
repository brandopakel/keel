"""Tests for check-log-compatibility.py's log parsing and normalization."""
import importlib.util
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    'check_log_compatibility', Path(__file__).with_name('check-log-compatibility.py'))
compat = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compat)


def record(*parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n%s\r\n' % (len(p), p) for p in parts)


class NormalizeTest(unittest.TestCase):
    start = 1_790_000_000_000

    def test_records_round_trip_binary_parts(self):
        body = record('SET', 'k', b'a\r\nb') + record('DEL', 'k')
        self.assertEqual(compat.records(body), [[b'SET', b'k', b'a\r\nb'], [b'DEL', b'k']])

    def test_relative_expiry_becomes_hours_from_start(self):
        near = self.start + 2 * compat.HOUR_MS + 1234
        far = 4102444800123
        body = record('PEXPIREAT', 'k', near) + record('PEXPIREAT', 'j', far)
        self.assertEqual(compat.normalize(body, self.start),
                         record('PEXPIREAT', 'k', 'run+2h') + record('PEXPIREAT', 'j', far))

    def test_map_order_is_sorted_and_options_are_left_alone(self):
        body = (record('HSET', 'h', 'b', '2', 'a', '1', 'b', '3') + record('ZADD', 'z', 2, 'y', 1, 'x')
                + record('ZADD', 'z', 'XX', 'CH', 10, 'b'))
        self.assertEqual(compat.normalize(body, self.start),
                         record('HSET', 'h', 'a', '1', 'b', '2', 'b', '3') + record('ZADD', 'z', 1, 'x', 2, 'y')
                         + record('ZADD', 'z', 'XX', 'CH', 10, 'b'))

    def test_a_collection_cut_into_records_is_joined(self):
        body = (record('RPUSH', 'l', 'a', 'b') + record('RPUSH', 'l', 'c') + record('RPUSH', 'm', 'd')
                + record('HSET', 'h', 'b', '2') + record('HSET', 'h', 'a', '1')
                + record('ZADD', 'z', 2, 'y') + record('ZADD', 'z', 'XX', 1, 'x'))
        self.assertEqual(compat.normalize(body, self.start),
                         record('RPUSH', 'l', 'a', 'b', 'c') + record('RPUSH', 'm', 'd')
                         + record('HSET', 'h', 'a', '1', 'b', '2')
                         + record('ZADD', 'z', 2, 'y') + record('ZADD', 'z', 'XX', 1, 'x'))

    def test_first_difference_names_the_record(self):
        a, b = record('SET', 'k', 'v') + record('DEL', 'k'), record('SET', 'k', 'v') + record('UNLINK', 'k')
        self.assertIn('record 1', compat.first_difference(a, b))


if __name__ == '__main__':
    unittest.main()
