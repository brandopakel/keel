import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('run_telemetry', Path(__file__).with_name('run-telemetry.py'))
rt = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rt)

# Trimmed from redis_exporter v1.93.0 in front of Keel and of Redis 8.10.2.
KEEL = '''# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 9
redis_exporter_last_scrape_error{err=""} 0
redis_target_scrape_request_errors_total 0
redis_up 1
redis_connected_clients 1
redis_db_keys{db="db0"} 20
redis_aof_buffer_length 0
'''
REDIS = '''redis_up 1
redis_exporter_scrapes_total 3
redis_connected_clients 1
redis_db_keys{db="db0"} 20
redis_db_keys{db="db1"} 0
redis_commands_processed_total 120
redis_keyspace_hits_total 40
'''


class Compat(unittest.TestCase):
    def test_metric_names_leave_out_the_exporters_own(self):
        self.assertEqual(rt.metric_names(KEEL),
                         {'redis_up', 'redis_connected_clients', 'redis_db_keys', 'redis_aof_buffer_length'})

    def test_compat_counts_and_lists_the_gap(self):
        result = rt.compat(KEEL, REDIS)
        self.assertEqual((result['keel'], result['redis'], result['shared']), (4, 5, 3))
        self.assertEqual(result['redis_only'], ['redis_commands_processed_total', 'redis_keyspace_hits_total'])
        self.assertEqual(result['keel_only'], ['redis_aof_buffer_length'])
        text = rt.compat_markdown(result)
        self.assertIn('4 metric names for Keel and 5 for Redis; 3 are shared', text)
        self.assertIn('**Missing on Keel (2):** `redis_commands_processed_total`', text)

    def test_no_gap(self):
        text = rt.compat_markdown(rt.compat(REDIS, REDIS))
        self.assertNotIn('Missing', text)


class Exposition(unittest.TestCase):
    def test_one_phase_at_a_time(self):
        state = rt.State()
        self.assertIn('keel_ci_phase{phase="idle"} 1', state.exposition())
        state.phase, state.names = 'eviction', {'keel': 23, 'redis': 191}
        text = state.exposition()
        self.assertIn('keel_ci_phase{phase="eviction"} 1', text)
        self.assertIn('keel_ci_phase{phase="idle"} 0', text)
        self.assertEqual(text.count('keel_ci_phase{'), 1 + len(rt.PHASES))
        self.assertIn('keel_ci_exporter_metric_names{server="redis"} 191', text)
        # Every sample line parses as a metric the way the scraper reads it.
        self.assertEqual(rt.metric_names(text), {'keel_ci_phase', 'keel_ci_exporter_metric_names'})


class Memtier(unittest.TestCase):
    def test_ops_per_second(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'steady-keel.json'
            path.write_text(json.dumps({'ALL STATS': {'Totals': {'Ops/sec': 95508.3}}}))
            self.assertEqual(rt.ops_per_second(path), 95508.3)
            path.write_text('{"ALL STATS": {}}')
            self.assertIsNone(rt.ops_per_second(path))
            self.assertIsNone(rt.ops_per_second(Path(tmp) / 'missing.json'))


if __name__ == '__main__':
    unittest.main()
