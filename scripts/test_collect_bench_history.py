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

    def census(self, artifact_id, created, branch, present):
        census = json.loads(json.dumps(CENSUS))
        census['commands'][0]['keel'] = present
        self.gh.add('command-census', artifact_id, artifact_id * 10, {'census.json': json.dumps(census)},
                    created=created, branch=branch)

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


if __name__ == '__main__':
    unittest.main()
