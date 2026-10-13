import csv
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location('collect_bench_history',
                                              Path(__file__).with_name('collect-bench-history.py'))
cbh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cbh)

REPO = 'brandopakel/keel'

# Verbatim from run 37573741036, an A/B pair on a hosted runner.
PROVENANCE = '''baseline c45298ba65b97a2146d48343dee87c6e81fb4cf9
candidate 5e1cc40a95d66922451890e4887ebf0b50a8d970
runner Linux 6.17.0-1022-azure x86_64 cpus 4 AMD EPYC 7763 64-Core Processor
go version go1.26.8 linux/amd64
e51ea5c4c17cda7eab400b6863ba9b9b1903248664a5973a20a22c0778cde99a  base.test
b4db3dc10f6be99c3d3db2221761da7e80e80b0c923d6904f85ebc0d93c23fbb  cand.test
'''

# The fields compare-benchmarks.py writes, trimmed to one row.
COMPARISON = {
    'rounds': {'count': 12, 'baseline_first': [1, 3], 'candidate_first': [2, 4]},
    'procs': 4,
    'rows': [{'name': 'BenchmarkCommandPathWithLog/SET', 'pairs': 12, 'median_ratio': 0.9915,
              'pair_range': [0.9684, 0.9977], 'order_effect': 0.9966, 'base_ns': 433.95, 'cand_ns': 429.65,
              'base_metrics': {'B/op': 48.0, 'allocs/op': 1.0}, 'cand_metrics': {'B/op': 48.0, 'allocs/op': 1.0},
              'pair_ratios': [], 'interval_95': [0.987, 0.9966]}],
    'overall': {'median_ratio': 0.999, 'interval_95': [0.9935, 1.0029], 'row_range': [0.9613, 1.0212],
                'order_effect': 0.999, 'resamples': 2000},
    'one_side_only': {'baseline': [], 'candidate': []},
    'failures': [],
}

# command-census.py's census.json, trimmed from run 37658260465.
CENSUS = {
    'redis_version': '8.10.1',
    'summary': {'areas': {'string': {'commands': 26, 'commands_present': 12, 'subcommands': 0, 'subcommands_present': 0},
                          'module:bf': {'commands': 10, 'commands_present': 6, 'subcommands': 0,
                                        'subcommands_present': 0}},
                'total': {'commands': 401, 'commands_present': 107, 'subcommands': 148, 'subcommands_present': 9}},
    'control_failures': [], 'keel_no_reply': [],
    'commands': [{'name': 'append', 'kind': 'command', 'group': 'string', 'module': '', 'since': '2.0.0',
                  'deprecated': False, 'keel': 'missing', 'redis': 'present', 'keel_reply': 'ERR unknown command'},
                 {'name': 'bf.add', 'kind': 'command', 'group': 'module', 'module': 'bf', 'since': '1.0.0',
                  'deprecated': False, 'keel': 'present', 'redis': 'present', 'keel_reply': 'ERR wrong number'}],
}

# The argument census's arguments.json, trimmed.
ARGUMENTS = {
    'redis_version': '8.10.1',
    'summary': {'areas': {'string': {'commands': 12, 'forms': 30, 'accepted': 26, 'refused': 4, 'wrong_reply': 0,
                                     'no_reply': 0, 'control': 0, 'redis8_forms': 4, 'redis8_accepted': 0}},
                'total': {'commands': 100, 'forms': 217, 'accepted': 207, 'refused': 10, 'wrong_reply': 0,
                          'no_reply': 0, 'control': 0, 'redis8_forms': 4, 'redis8_accepted': 0}},
    'forms': [{'command': 'set', 'area': 'string', 'label': '', 'since': '1.0.0', 'argv': ['SET', 'str', 'v'],
               'class': 'accepted', 'detail': '', 'keel': '+OK', 'redis': '+OK'},
              {'command': 'set', 'area': 'string', 'label': 'IFEQ', 'since': '8.4.0',
               'argv': ['SET', 'str', 'v', 'IFEQ', 's'], 'class': 'refused', 'detail': 'not parsed',
               'keel': '-ERR syntax error', 'redis': '$-1'},
              {'command': 'shutdown', 'area': 'server', 'label': '', 'since': '1.0.0', 'argv': [],
               'class': 'not probed', 'detail': 'stops the server', 'keel': '', 'redis': ''}],
}

# The reply census's replies.json, trimmed.
REPLIES = {
    'redis_version': '8.10.1',
    'summary': {'areas': {'string': {'commands': 12, 'cases': 116, 'same': 108, 'differs': 0, 'refused': 8,
                                     'no_reply': 0, 'resp3_cases': 58, 'resp3_same': 54,
                                     'keel_schema_violations': 0, 'redis_schema_violations': 0}},
                'total': {'commands': 100, 'cases': 906, 'same': 886, 'differs': 0, 'refused': 20, 'no_reply': 0,
                          'resp3_cases': 453, 'resp3_same': 443, 'keel_schema_violations': 2,
                          'redis_schema_violations': 1}},
    'commands': {'get': {'area': 'string', 'function': 'getCommand', 'at': 't_string.c:270', 'sites': [],
                         'schema': True}},
    'cases': [{'command': 'get', 'area': 'string', 'case': 'form', 'label': '', 'since': '1.0.0',
               'argv': ['GET', 'str'], 'protocol': 3, 'result': 'same', 'detail': '', 'keel': '$abc',
               'redis': '$abc', 'keel_shape': '$', 'redis_shape': '$', 'keel_schema': 'ok', 'redis_schema': 'ok'}],
}

# The configuration census's config.json, trimmed.
CONFIG = {
    'redis_version': '8.10.1',
    'summary': {'total': {'configs': 218, 'has': 20, 'mapped': 5, 'lacks': 193, 'lacks_used': 4, 'lacks_tested': 90,
                          'documented': 200, 'same_value': 13},
                'families': {'tls': {'configs': 22, 'has': 0, 'mapped': 0, 'lacks': 22}}},
    'configs': [{'name': 'maxmemory', 'alias': '', 'kind': 'sizet', 'flags': [], 'condition': '',
                 'documented': 'example', 'class': 'has', 'keel_value': '0', 'redis_value': '0', 'note': '',
                 'used_by': ['redis_exporter (config_maxmemory)'], 'test_files': 19, 'family': 'memory and eviction'},
                {'name': 'repl-compression', 'alias': '', 'kind': 'int', 'flags': ['IMMUTABLE_CONFIG'],
                 'condition': '#ifdef USE_COMPRESSION', 'documented': 'no', 'class': 'lacks', 'keel_value': None,
                 'redis_value': None, 'note': '', 'used_by': [], 'test_files': 0, 'family': 'replication'}],
}

# The Redis tests workflow's redis-tests.json, trimmed.
REDIS_TESTS = {
    'skipped_units': {'unit/shutdown': 'stops and restarts the server'},
    'complete': True, 'incomplete_groups': [],
    'totals': {'keel': {'units': 2, 'passed': 100, 'failed': 44, 'ignored': 3, 'not_run': 0, 'units_crashed': 0,
                        'units_timed_out': 1, 'units_exception': 0},
               'redis': {'units': 2, 'passed': 150, 'failed': 1, 'ignored': 3, 'not_run': 0, 'units_crashed': 0,
                         'units_timed_out': 0, 'units_exception': 0}},
    'units': [{'unit': 'unit/scan', 'keel': None,
               'redis': {'outcome': 'ok', 'passed': 6, 'failed': 1, 'ignored': 0, 'not_run': 0, 'seconds': 2.0,
                         'failing': ['x'], 'details': {}, 'exception': ''}},
              {'unit': 'unit/type/string', 'keel': {'outcome': 'timeout', 'passed': 100, 'failed': 44, 'ignored': 3,
                                                     'not_run': 0, 'seconds': 900.0, 'failing': ['GETEX EX option'],
                                                     'details': {'GETEX EX option': "ERR unknown command 'getex'"},
                                                     'exception': ''},
               'redis': {'outcome': 'ok', 'passed': 144, 'failed': 0, 'ignored': 3, 'not_run': 0, 'seconds': 5.0,
                         'failing': [], 'details': {}, 'exception': ''}}],
}

# The Live telemetry artifact's files, from run 37587041339.
TELEMETRY = {
    'provenance.txt': 'keel 2aeb1c8\nrunner Linux 6.17.0-1022-azure x86_64 cpus 4 AMD EPYC 9V74 80-Core Processor\n',
    'load/compat.json': json.dumps({'keel': 21, 'redis': 188, 'shared': 17, 'redis_only': [], 'keel_only': []}),
    'load/timeline.json': json.dumps([{'phase': 'steady', 'description': '', 'start': 0, 'end': 1,
                                       'ops_per_second': {'keel': 95508.3, 'redis': 104308.9}},
                                      {'phase': 'expiry', 'description': '', 'start': 1, 'end': 2,
                                       'ops_per_second': {'keel': None, 'redis': 90740.1}}]),
}

GO_HEADER = 'goos: linux\ngoarch: amd64\ncpu: AMD EPYC 9V74 80-Core Processor                \n'

# Before 65ebdbc (October 3, 2026): no CPU model in the provenance, and rows
# without intervals or B/op under no overall section.
OLD_PROVENANCE = '''baseline 8380d9fc121a57788e7cd88efe805786d4f70764
candidate 8380d9fc121a57788e7cd88efe805786d4f70764
runner Linux 6.17.0-1022-azure x86_64 cpus 4
go version go1.26.8 linux/amd64
binaries identical: an A/A run
'''
OLD_COMPARISON = {
    'rows': [{'name': 'BenchmarkCommandPath/BF.ADD', 'pairs': 6, 'median_ratio': 1.0956, 'base_ns': 380.6,
              'cand_ns': 413.9, 'base_allocs': 2.0, 'cand_allocs': 2.0},
             {'name': 'BenchmarkCommandPath/GET', 'pairs': 6, 'median_ratio': 0.98, 'base_ns': 209.85,
              'cand_ns': 205.65, 'base_allocs': 2.0, 'cand_allocs': 2.0}],
    'one_side_only': [], 'allocation_regressions': ['BenchmarkCommandPath/SET: 2 -> 3 allocs/op'],
}

SUMMARY = {
    'status': 'passed', 'repetitions': 7,
    'cases': {'pipeline-16': {
        'baseline': {'rss_mib': {'median': 14.8671875}, 'ops_per_second': {'median': 841386.89},
                     'p99_ms': {'median': 0.727}, 'p999_ms': {'median': 2.895}, 'client_cpu_warnings': 0},
        'candidate': {'rss_mib': {'median': 14.63}, 'ops_per_second': {'median': 818875.2},
                      'p99_ms': {'median': 0.743}, 'p999_ms': {'median': 2.9}, 'client_cpu_warnings': 1},
        'candidate_baseline_throughput_ratio': {'median': 0.979203815357667, 'min': 0.9482420472721862,
                                                'max': 1.007820682961345, 'pairs': []}}},
}

# The memory suite reports RSS only.
MEMORY = {'status': 'passed', 'repetitions': 3, 'cases': {'keys-2500-value-64-clients-1': {
    'baseline': {'rss_mib': {'median': 13.5}, 'client_cpu_warnings': 0},
    'candidate': {'rss_mib': {'median': 13.25}, 'client_cpu_warnings': 0}}}}

LSCPU = 'Architecture:                            x86_64\nModel name:                              AMD EPYC 9V74 80-Core Processor\n'


def zipped(files):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, 'w') as z:
        for name, text in files.items():
            z.writestr(name, text)
    return buf.getvalue()


def command_path_files(failures=()):
    return {'comparison.json': json.dumps({**COMPARISON, 'failures': list(failures)}),
            'provenance.txt': PROVENANCE, 'baseline.txt': GO_HEADER, 'candidate.txt': GO_HEADER,
            'other.txt': 'not read'}


def matched_files():
    return {'lscpu.txt': LSCPU, 'baseline-source.txt': 'aaaa\n', 'candidate-source.txt': 'bbbb\n',
            'standard/summary.json': json.dumps(SUMMARY), 'memory/summary.json': json.dumps(MEMORY),
            'standard/r1-pipeline-16-baseline/report.json': '{}'}


class FakeGitHub:
    """Artifacts, runs and pull requests as the gh calls return them."""

    def __init__(self):
        self.repo = REPO
        self.artifacts_by_name = {}
        self.runs = {}
        self.zips = {}
        self.downloads = []

    def add(self, name, artifact_id, run_id, files, created='2026-10-07T04:54:54Z', expired=False,
            status='completed', repo=REPO, branch='refactor/remove-default-engine-3'):
        self.artifacts_by_name.setdefault(name, []).append(
            {'id': artifact_id, 'created_at': created, 'expired': expired, 'workflow_run': {'id': run_id}})
        self.runs[run_id] = {'id': run_id, 'created_at': created, 'event': 'pull_request', 'head_branch': branch,
                             'head_sha': f'sha{run_id}', 'status': status, 'conclusion': 'success',
                             'html_url': f'https://github.com/{REPO}/actions/runs/{run_id}',
                             'head_repository': {'full_name': repo}}
        self.zips[artifact_id] = files if files is None or isinstance(files, bytes) else zipped(files)

    def artifacts(self, name):
        return self.artifacts_by_name.get(name, [])

    def run(self, run_id):
        return dict(self.runs[run_id])

    def download(self, artifact_id):
        self.downloads.append(artifact_id)
        if self.zips[artifact_id] is None:
            raise subprocess.CalledProcessError(1, ['gh', 'api', 'zip'], stderr=b'HTTP 410: Gone')
        return self.zips[artifact_id]

    def pr(self, sha, branch):
        return 130


def read(path):
    with open(path, newline='') as f:
        return list(csv.DictReader(f))


class Hosts(unittest.TestCase):
    def test_short_names(self):
        for model, host in [('AMD EPYC 7763 64-Core Processor', 'EPYC 7763'),
                            ('AMD EPYC 9V74 80-Core Processor', 'EPYC 9V74'),
                            ('Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz', 'Xeon 8370C'),
                            ('Intel(R) Xeon(R) 6973P-C', 'Xeon 6973P-C'),
                            ('INTEL(R) XEON(R) PLATINUM 8573C', 'Xeon 8573C'),
                            ('Intel(R) Xeon(R) CPU E5-2673 v4 @ 2.30GHz', 'Xeon E5-2673 v4'),
                            ('Apple  M3 Pro', 'Apple M3 Pro'), ('', '')]:
            self.assertEqual(cbh.host_of(model), host, model)


class Records(unittest.TestCase):
    def setUp(self):
        self.run = {'id': 37573741036, 'created_at': '2026-10-07T04:54:54Z', 'event': 'pull_request',
                    'head_branch': 'b', 'conclusion': 'success', 'html_url': 'u'}

    def test_command_path(self):
        files = cbh.read_members(((n, len(t), lambda t=t: t.encode()) for n, t in command_path_files().items()),
                                 cbh.KINDS['command-path']['members'])
        self.assertEqual(sorted(files), ['baseline.txt', 'candidate.txt', 'comparison.json', 'provenance.txt'])
        records = cbh.command_path_records(11461977740, self.run, 130, files)
        run, = records['runs']
        self.assertEqual((run['host'], run['cpus'], run['go'], run['identical']), ('EPYC 7763', 4, 'go1.26.8', 'no'))
        self.assertEqual((run['baseline'][:7], run['candidate'][:7]), ('c45298b', '5e1cc40'))
        self.assertEqual((run['median_ratio'], run['interval_low'], run['interval_high']), (0.999, 0.9935, 1.0029))
        self.assertEqual((run['rounds'], run['benchmarks'], run['failures'], run['pr']), (12, 1, 0, 130))
        row, = records['rows']
        self.assertEqual(row['benchmark'], 'CommandPathWithLog/SET')
        self.assertEqual((row['median_ratio'], row['baseline_allocs'], row['candidate_bytes']), (0.9915, 1.0, 48.0))

    def test_identical_binaries_and_failures(self):
        files = {'provenance.txt': PROVENANCE + 'binaries identical: an A/A run\n',
                 'comparison.json': json.dumps({**COMPARISON, 'failures': ['x: 1 -> 2 allocs/op']})}
        run, = cbh.command_path_records(1, self.run, '', files)['runs']
        self.assertEqual((run['identical'], run['failures']), ('yes', 1))

    def test_command_path_before_pairing(self):
        records = cbh.command_path_records(1, self.run, '', {
            'provenance.txt': OLD_PROVENANCE, 'comparison.json': json.dumps(OLD_COMPARISON),
            'candidate.txt': GO_HEADER})
        run, = records['runs']
        # The geometric midpoint of 0.98 and 1.0956, as compare-benchmarks.py takes it.
        self.assertEqual((run['host'], run['cpus'], run['median_ratio'], run['interval_low']),
                         ('EPYC 9V74', 4, 1.0362, ''))
        self.assertEqual((run['row_min'], run['row_max'], run['failures'], run['identical']),
                         (0.98, 1.0956, 1, 'yes'))
        first = records['rows'][0]
        self.assertEqual((first['median_ratio'], first['pair_min'], first['baseline_allocs'], first['baseline_bytes']),
                         (1.0956, '', 2.0, ''))

    def test_command_path_without_comparison(self):
        records = cbh.command_path_records(1, self.run, '', {'provenance.txt': PROVENANCE})
        run, = records['runs']
        self.assertEqual((run['failures'], run['median_ratio'], records['rows']), (1, '', []))

    def test_matched(self):
        files = {n: t for n, t in matched_files().items() if cbh.KINDS['matched']['members'].match(n)}
        self.assertNotIn('standard/r1-pipeline-16-baseline/report.json', files)
        records = cbh.matched_records(5, self.run, '', files)
        run, = records['runs']
        self.assertEqual((run['host'], run['suites'], run['status'], run['baseline']),
                         ('EPYC 9V74', 'memory standard', 'passed', 'aaaa'))
        memory, standard = records['cases']
        self.assertEqual((memory['suite'], memory['candidate_rss_mib'], memory['ratio_median'], memory['baseline_ops']),
                         ('memory', 13.25, '', ''))
        self.assertEqual((standard['workload'], standard['ratio_median'], standard['baseline_ops'],
                          standard['candidate_p99_ms'], standard['client_cpu_warnings']),
                         ('pipeline-16', 0.9792, 841387, 0.743, 1))

    def test_matched_failures_are_runs_too(self):
        run, = cbh.matched_records(5, self.run, '', {'lscpu.txt': LSCPU})['runs']
        self.assertEqual(run['status'], 'no summary')
        failed = {'standard/summary.json': json.dumps({**SUMMARY, 'status': 'failed'})}
        run, = cbh.matched_records(5, self.run, '', failed)['runs']
        self.assertEqual(run['status'], 'failed')

    def test_oversized_member_is_refused(self):
        with self.assertRaisesRegex(ValueError, 'over the'):
            cbh.read_members([('provenance.txt', cbh.MAX_MEMBER_BYTES + 1, bytes)],
                             cbh.KINDS['command-path']['members'])


class Collect(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / 'data'
        self.gh = FakeGitHub()

    def tearDown(self):
        self.tmp.cleanup()

    def test_collects_once_in_time_order(self):
        self.gh.add('command-path-benchmarks', 20, 200, command_path_files(), created='2026-10-07T05:00:00Z')
        self.gh.add('command-path-benchmarks', 10, 100, command_path_files(), created='2026-10-06T05:00:00Z')
        counts, errors = cbh.collect(self.gh, self.data, ['command-path'])
        self.assertEqual((counts, errors), ({'command-path collected': 2}, []))
        runs = read(self.data / 'command-path/runs.csv')
        self.assertEqual([r['artifact_id'] for r in runs], ['10', '20'])
        self.assertEqual(len(read(self.data / 'command-path/rows.csv')), 2)
        before = (self.data / 'command-path/runs.csv').read_text()
        counts, errors = cbh.collect(self.gh, self.data, ['command-path'])
        self.assertEqual((counts, errors, sorted(self.gh.downloads)), ({}, [], [10, 20]))
        self.assertEqual((self.data / 'command-path/runs.csv').read_text(), before)

    def test_skips_what_it_cannot_or_should_not_read(self):
        self.gh.add('command-path-benchmarks', 1, 100, command_path_files(), expired=True)
        self.gh.add('command-path-benchmarks', 2, 200, command_path_files(), status='in_progress')
        self.gh.add('command-path-benchmarks', 3, 300, command_path_files(), repo='someone/keel')
        self.gh.add('command-path-benchmarks', 4, 400, None)
        self.gh.add('command-path-benchmarks', 5, 500, b'not a zip')
        self.gh.add('command-path-benchmarks', 6, 600, command_path_files())
        counts, errors = cbh.collect(self.gh, self.data, ['command-path'])
        self.assertEqual(counts, {'command-path expired before collection': 1, 'command-path still running': 1,
                                  'command-path from a fork': 1, 'command-path failed': 2,
                                  'command-path collected': 1})
        self.assertEqual(len(errors), 2)
        self.assertTrue(any('HTTP 410' in e for e in errors), errors)
        self.assertEqual([r['artifact_id'] for r in read(self.data / 'command-path/runs.csv')], ['6'])
        # A run still in progress or unreadable now is looked at again next time.
        self.gh.runs[200]['status'] = 'completed'
        self.gh.zips[4] = zipped(command_path_files())
        counts, _ = cbh.collect(self.gh, self.data, ['command-path'])
        self.assertEqual(counts['command-path collected'], 2)

    def test_expired_artifact_from_a_saved_copy(self):
        self.gh.add('matched-keyspace-adoption', 7, 700, None, expired=True)
        saved = Path(self.tmp.name) / 'saved'
        for name, text in matched_files().items():
            (saved / '700' / name).parent.mkdir(parents=True, exist_ok=True)
            (saved / '700' / name).write_text(text)
        counts, errors = cbh.collect(self.gh, self.data, ['matched'], saved=saved)
        self.assertEqual((counts, errors, self.gh.downloads), ({'matched collected': 1}, [], []))
        self.assertEqual(len(read(self.data / 'matched/cases.csv')), 2)

    def test_the_everysec_leg_is_collected_apart(self):
        self.gh.add('matched-keyspace-adoption', 7, 700, matched_files())
        self.gh.add('matched-keyspace-adoption-everysec', 8, 700, matched_files())
        counts, errors = cbh.collect(self.gh, self.data, ['matched', 'matched-everysec'])
        self.assertEqual((counts, errors), ({'matched collected': 1, 'matched-everysec collected': 1}, []))
        self.assertEqual([r['artifact_id'] for r in read(self.data / 'matched/runs.csv')], ['7'])
        self.assertEqual([r['artifact_id'] for r in read(self.data / 'matched-everysec/runs.csv')], ['8'])
        self.assertEqual(len(read(self.data / 'matched-everysec/cases.csv')), 2)

    def test_max_new_bounds_the_work(self):
        for i in range(1, 6):
            self.gh.add('command-path-benchmarks', i, i * 100, command_path_files(), created=f'2026-10-0{i}T00:00:00Z')
        cbh.collect(self.gh, self.data, ['command-path'], max_new=2)
        self.assertEqual(sorted(self.gh.downloads), [1, 2])
        cbh.collect(self.gh, self.data, ['command-path'], max_new=2)
        self.assertEqual(len(read(self.data / 'command-path/runs.csv')), 4)

    def test_refuses_a_file_with_other_columns(self):
        (self.data / 'command-path').mkdir(parents=True)
        (self.data / 'command-path/runs.csv').write_text('artifact_id,time\n1,2026-10-01T00:00:00Z\n')
        with self.assertRaisesRegex(SystemExit, 'migrate'):
            cbh.collect(self.gh, self.data, ['command-path'])


class NewKinds(unittest.TestCase):
    def setUp(self):
        self.run = {'id': 37658260465, 'created_at': '2026-10-07T17:20:12Z', 'event': 'push',
                    'head_branch': 'develop', 'conclusion': 'success', 'html_url': 'u'}

    def test_census(self):
        records = cbh.census_records(1, self.run, '', {'census.json': json.dumps(CENSUS)})
        run, = records['runs']
        self.assertEqual((run['redis_version'], run['commands'], run['commands_present'], run['subcommands_present']),
                         ('8.10.1', 401, 107, 9))
        self.assertEqual({a['area']: a['commands_present'] for a in records['areas']}, {'string': 12, 'module:bf': 6})
        self.assertEqual([(c['name'], c['area'], c['keel']) for c in records['latest']],
                         [('append', 'string', 'missing'), ('bf.add', 'module:bf', 'present')])
        self.assertNotIn('argument-runs', records, 'a census from before the argument census')

    def test_argument_census(self):
        records = cbh.census_records(1, self.run, '', {'census.json': json.dumps(CENSUS),
                                                      'arguments.json': json.dumps(ARGUMENTS)})
        run, = records['argument-runs']
        self.assertEqual((run['redis_version'], run['commands'], run['forms'], run['accepted'], run['refused'],
                          run['wrong_reply'], run['redis8_forms'], run['redis8_accepted']),
                         ('8.10.1', 100, 217, 207, 10, 0, 4, 0))
        area, = records['argument-areas']
        self.assertEqual((area['area'], area['forms'], area['accepted'], area['refused']), ('string', 30, 26, 4))
        self.assertEqual([(f['command'], f['form'], f['sent'], f['class'], f['keel']) for f in records['arguments']],
                         [('set', 'base', 'SET str v', 'accepted', '+OK'),
                          ('set', 'IFEQ', 'SET str v IFEQ s', 'refused', '-ERR syntax error')],
                         'not probed commands are not forms')
        self.assertTrue(cbh.KINDS['census']['members'].match('arguments.json'))
        self.assertNotIn('reply-runs', records, 'a census from before the reply census')

    def test_reply_census(self):
        records = cbh.census_records(1, self.run, '', {'census.json': json.dumps(CENSUS),
                                                      'replies.json': json.dumps(REPLIES)})
        run, = records['reply-runs']
        self.assertEqual((run['redis_version'], run['commands'], run['cases'], run['same'], run['refused'],
                          run['resp3_same'], run['keel_schema_violations'], run['redis_schema_violations']),
                         ('8.10.1', 100, 906, 886, 20, 443, 2, 1))
        area, = records['reply-areas']
        self.assertEqual((area['area'], area['cases'], area['same'], area['refused']), ('string', 116, 108, 8))
        case, = records['replies']
        self.assertEqual((case['command'], case['form'], case['protocol'], case['sent'], case['result'],
                          case['keel_shape'], case['redis_at']), ('get', 'base', 3, 'GET str', 'same', '$', 't_string.c:270'))
        self.assertTrue(cbh.KINDS['census']['members'].match('replies.json'))

    def test_config_census(self):
        records = cbh.census_records(1, self.run, '', {'census.json': json.dumps(CENSUS),
                                                      'config.json': json.dumps(CONFIG)})
        run, = records['config-runs']
        self.assertEqual((run['configs'], run['has'], run['mapped'], run['lacks'], run['lacks_used']),
                         (218, 20, 5, 193, 4))
        directive, = records['config']
        self.assertEqual((directive['name'], directive['class'], directive['keel_value'], directive['used_by'],
                          directive['test_files']), ('maxmemory', 'has', '0', 'redis_exporter (config_maxmemory)', 19))
        self.assertTrue(cbh.KINDS['census']['members'].match('config.json'))

    def test_redis_tests(self):
        records = cbh.redis_test_records(3, self.run, '', {'redis-tests.json': json.dumps(REDIS_TESTS)})
        run, = records['redis-tests']
        self.assertEqual((run['units'], run['redis_passed'], run['keel_passed'], run['keel_failed'],
                          run['keel_units_timed_out'], run['parity']), (2, 150, 100, 44, 1, 0.6667))
        self.assertEqual([(u['unit'], u['keel_passed'], u['keel_outcome'], u['redis_passed']) for u in
                          records['redis-tests-units']], [('unit/scan', '', '', 6), ('unit/type/string', 100, 'timeout', 144)])
        self.assertEqual(records['redis-tests-failing'],
                         [{'run_id': 37658260465, 'time': '2026-10-07T17:20:12Z', 'branch': 'develop',
                           'unit': 'unit/type/string', 'test': 'GETEX EX option', 'detail': "ERR unknown command 'getex'"}])

    def test_an_incomplete_redis_tests_report_is_quarantined(self):
        report = dict(REDIS_TESTS, complete=False, incomplete_groups=['keel group 4'])
        records = cbh.redis_test_records(3, self.run, '', {'redis-tests.json': json.dumps(report)})
        self.assertEqual(sorted(records), ['redis-tests'], 'no unit lines and no snapshot')
        run, = records['redis-tests']
        self.assertEqual((run['complete'], run['incomplete_groups'], run['keel_passed']), ('no', 'keel group 4', 100))
        # A report from before the field existed counts as incomplete.
        old = {k: v for k, v in REDIS_TESTS.items() if k not in ('complete', 'incomplete_groups')}
        run, = cbh.redis_test_records(3, self.run, '', {'redis-tests.json': json.dumps(old)})['redis-tests']
        self.assertEqual((run['complete'], run['incomplete_groups']), ('no', 'not recorded'))

    def test_the_failing_snapshot_survives_an_incomplete_run_and_empties_on_a_clean_one(self):
        gh = FakeGitHub()
        gh.add('redis-tests', 7, 70, {'redis-tests.json': json.dumps(REDIS_TESTS)}, created='2026-10-13T01:00:00Z',
               branch='develop')
        partial = dict(REDIS_TESTS, complete=False, incomplete_groups=['redis group 2'])
        partial['units'] = [dict(REDIS_TESTS['units'][1], keel=dict(REDIS_TESTS['units'][1]['keel'], failing=[]))]
        gh.add('redis-tests', 8, 80, {'redis-tests.json': json.dumps(partial)}, created='2026-10-13T02:00:00Z',
               branch='develop')
        with tempfile.TemporaryDirectory() as tmp:
            cbh.collect(gh, tmp, ['redis-tests'])
            failing = read(Path(tmp) / 'census/redis-tests-failing.csv')
            self.assertEqual([(f['run_id'], f['test']) for f in failing], [('70', 'GETEX EX option')],
                             'the later, incomplete run does not replace the snapshot')
            self.assertEqual([r['complete'] for r in read(Path(tmp) / 'census/redis-tests.csv')], ['yes', 'no'])
            self.assertEqual(len(read(Path(tmp) / 'census/redis-tests-units.csv')), 2, 'units of the complete run only')
            clean = dict(REDIS_TESTS)
            clean['units'] = [dict(u, keel=dict(u['keel'], failing=[], details={})) if u['keel'] else u
                              for u in REDIS_TESTS['units']]
            gh.add('redis-tests', 9, 90, {'redis-tests.json': json.dumps(clean)}, created='2026-10-13T03:00:00Z',
                   branch='develop')
            cbh.collect(gh, tmp, ['redis-tests'])
            self.assertEqual(read(Path(tmp) / 'census/redis-tests-failing.csv'), [],
                             'a complete run with no failures empties the snapshot')

    def test_redis_tests_files_live_under_census(self):
        gh = FakeGitHub()
        gh.add('redis-tests', 7, 70, {'redis-tests.json': json.dumps(REDIS_TESTS)}, created='2026-10-13T01:00:00Z',
               branch='develop')
        with tempfile.TemporaryDirectory() as tmp:
            counts, errors = cbh.collect(gh, tmp, ['redis-tests'])
            self.assertEqual((counts, errors), ({'redis-tests collected': 1}, []))
            self.assertEqual(sorted(p.name for p in (Path(tmp) / 'census').iterdir()),
                             ['redis-tests-failing.csv', 'redis-tests-units.csv', 'redis-tests.csv'])
            self.assertEqual(read(Path(tmp) / 'census/redis-tests.csv')[0]['keel_passed'], '100')
            counts, _ = cbh.collect(gh, tmp, ['redis-tests'])
            self.assertEqual(counts, {}, 'recorded once')

    def test_telemetry(self):
        records = cbh.telemetry_records(2, self.run, 134, TELEMETRY)
        run, = records['runs']
        self.assertEqual((run['host'], run['keel_metric_names'], run['redis_metric_names'], run['shared_metric_names']),
                         ('EPYC 9V74', 21, 188, 17))
        self.assertEqual([(p['phase'], p['keel_ops'], p['redis_ops']) for p in records['phases']],
                         [('steady', 95508, 104309), ('expiry', '', 90740)])

    def test_members(self):
        names = ['provenance.txt', 'load/compat.json', 'load/timeline.json', 'load/steady-keel.json', 'alloy.log']
        self.assertEqual([n for n in names if cbh.KINDS['telemetry']['members'].match(n)], names[:3])


class Latest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / 'data'
        self.gh = FakeGitHub()

    def tearDown(self):
        self.tmp.cleanup()

    def census(self, artifact_id, created, branch, present, arguments=False):
        census = json.loads(json.dumps(CENSUS))
        census['commands'][0]['keel'] = present
        files = {'census.json': json.dumps(census)}
        if arguments:
            files['arguments.json'] = json.dumps(ARGUMENTS)
            files['replies.json'] = json.dumps(REPLIES)
            files['config.json'] = json.dumps(CONFIG)
        self.gh.add('command-census', artifact_id, artifact_id * 10, files, created=created, branch=branch)

    def latest(self):
        return [(r['run_id'], r['name'], r['keel']) for r in read(self.data / 'census/latest.csv')]

    def test_newest_census_of_develop_is_the_snapshot(self):
        self.census(1, '2026-10-07T10:00:00Z', 'develop', 'missing')
        self.census(2, '2026-10-07T11:00:00Z', 'develop', 'present')
        self.census(3, '2026-10-07T12:00:00Z', 'feat/x', 'missing')
        counts, errors = cbh.collect(self.gh, self.data, ['census'])
        self.assertEqual((counts, errors), ({'census collected': 3}, []))
        self.assertEqual(self.latest(), [('20', 'append', 'present'), ('20', 'bf.add', 'present')])
        self.assertEqual(len(read(self.data / 'census/runs.csv')), 3, 'every run is in the history')
        self.assertEqual(len(read(self.data / 'census/areas.csv')), 6)

    def test_an_older_census_collected_late_does_not_replace_it(self):
        self.census(2, '2026-10-07T11:00:00Z', 'develop', 'present')
        cbh.collect(self.gh, self.data, ['census'])
        self.census(1, '2026-10-07T10:00:00Z', 'develop', 'missing')
        cbh.collect(self.gh, self.data, ['census'])
        self.assertEqual(self.latest()[0], ('20', 'append', 'present'))
        self.census(4, '2026-10-07T13:00:00Z', 'develop', 'missing')
        cbh.collect(self.gh, self.data, ['census'])
        self.assertEqual(self.latest()[0], ('40', 'append', 'missing'))

    def test_no_snapshot_without_a_census_of_develop(self):
        self.census(3, '2026-10-07T12:00:00Z', 'feat/x', 'missing')
        cbh.collect(self.gh, self.data, ['census'])
        self.assertFalse((self.data / 'census/latest.csv').exists())

    def test_argument_forms_are_a_snapshot_of_their_own(self):
        self.census(1, '2026-10-07T10:00:00Z', 'develop', 'missing')
        self.census(2, '2026-10-07T11:00:00Z', 'develop', 'present', arguments=True)
        counts, errors = cbh.collect(self.gh, self.data, ['census'])
        self.assertEqual((counts, errors), ({'census collected': 2}, []))
        forms = read(self.data / 'census/arguments.csv')
        self.assertEqual([(f['run_id'], f['command'], f['form'], f['class']) for f in forms],
                         [('20', 'set', 'base', 'accepted'), ('20', 'set', 'IFEQ', 'refused')])
        self.assertEqual(len(read(self.data / 'census/argument-runs.csv')), 1, 'the older run had no argument census')
        self.assertEqual(len(read(self.data / 'census/reply-runs.csv')), 1)
        self.assertEqual(read(self.data / 'census/replies.csv')[0]['run_id'], '20')
        self.assertEqual(len(read(self.data / 'census/config-runs.csv')), 1)
        self.assertEqual(read(self.data / 'census/config.csv')[0]['name'], 'maxmemory')
        self.assertEqual(len(read(self.data / 'census/runs.csv')), 2)
        # A newer census of develop without arguments.json leaves the snapshot.
        self.census(3, '2026-10-07T12:00:00Z', 'develop', 'present')
        cbh.collect(self.gh, self.data, ['census'])
        self.assertEqual(read(self.data / 'census/arguments.csv')[0]['run_id'], '20')
        self.assertEqual(read(self.data / 'census/latest.csv')[0]['run_id'], '30')


if __name__ == '__main__':
    unittest.main()
