#!/usr/bin/env python3
"""Copy benchmark results out of CI artifacts into CSV files that keep.

GitHub deletes a workflow artifact after 30 days, and Grafana Cloud's free plan
keeps pushed metrics for 14, so neither can show benchmark history over months.
This script copies the numbers worth keeping from each run's artifact into CSV
files on the repository's bench-history branch. A Grafana dashboard reads them
from raw.githubusercontent.com through the Infinity data source, which fetches
a URL at query time and stores nothing itself.

Five kinds of run are collected:

- command-path: the Command path workflow's paired benchmarks
  (.github/workflows/command-path.yml), from comparison.json and
  provenance.txt. command-path/runs.csv gets one line per run, with the median
  across benchmarks, and command-path/rows.csv one line per benchmark.
- matched: General cache validation's matched keyspace adoption job, from each
  suite's summary.json and lscpu.txt. matched/runs.csv gets one line per run,
  failed ones included, and matched/cases.csv one line per workload of each
  suite.
- matched-everysec: the same job's everysec leg, which runs the standard suite
  with the log on (appendfsync everysec), from its own artifact, into
  matched-everysec/runs.csv and matched-everysec/cases.csv, with the same
  columns.
- census: the Command census workflow's census.json (scripts/command-census.py).
  census/runs.csv gets one line per run, with how many of Redis's commands and
  subcommands Keel has, and census/areas.csv one line per area (a command group,
  or a module) of each run. census/latest.csv holds every command and
  subcommand of the newest census of develop, with whether Keel has it; it is
  rewritten, not appended to, when a newer one arrives.
- telemetry: the Live telemetry workflow's load/compat.json and
  load/timeline.json, with provenance.txt for the host. telemetry/runs.csv gets
  one line per run, with how many metric names redis_exporter reported for Keel
  and for Redis, and telemetry/phases.csv one line per load phase, with each
  server's ops/s (not a comparison of speed: both share one unpinned runner).

A run is recorded once, keyed by its artifact's ID, so running this again adds
only what is new. A run still in progress, or from a fork, is skipped. An
artifact that expired before it was collected can still be read from a saved
copy: --saved names a directory holding one extracted artifact per run, in a
subdirectory named by run ID.

Times are the run's creation time, in UTC. The host is the runner's CPU model,
shortened (AMD EPYC 7763 64-Core Processor becomes EPYC 7763), because results
on hosted runners differ by host.

This script reads artifacts as data only. It runs from the default branch with
a token that can push, so nothing in an artifact is executed, every member it
reads is size-limited, and each value is written as a quoted CSV field.

GitHub is reached through the gh CLI, which takes its token from GH_TOKEN in CI.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import csv
import io
import json
import math
from pathlib import Path
import re
import statistics
import subprocess
import sys
import zipfile

# Members read from an artifact, at most this large each.
MAX_MEMBER_BYTES = 8 << 20

COMMAND_PATH_RUNS = ['artifact_id', 'run_id', 'time', 'event', 'branch', 'pr', 'conclusion', 'baseline',
                     'candidate', 'identical', 'host', 'cpu_model', 'cpus', 'go', 'rounds', 'benchmarks',
                     'median_ratio', 'interval_low', 'interval_high', 'row_min', 'row_max', 'order_effect',
                     'failures', 'url']
COMMAND_PATH_ROWS = ['artifact_id', 'run_id', 'time', 'branch', 'pr', 'host', 'benchmark', 'median_ratio',
                     'interval_low', 'interval_high', 'pair_min', 'pair_max', 'baseline_ns', 'candidate_ns',
                     'baseline_allocs', 'candidate_allocs', 'baseline_bytes', 'candidate_bytes']
MATCHED_RUNS = ['artifact_id', 'run_id', 'time', 'event', 'branch', 'pr', 'conclusion', 'baseline', 'candidate',
                'host', 'cpu_model', 'suites', 'status', 'url']
MATCHED_CASES = ['artifact_id', 'run_id', 'time', 'branch', 'pr', 'host', 'suite', 'workload', 'repetitions',
                 'ratio_median', 'ratio_min', 'ratio_max', 'baseline_ops', 'candidate_ops', 'baseline_p99_ms',
                 'candidate_p99_ms', 'baseline_p999_ms', 'candidate_p999_ms', 'baseline_rss_mib',
                 'candidate_rss_mib', 'client_cpu_warnings']

CENSUS_RUNS = ['artifact_id', 'run_id', 'time', 'event', 'branch', 'pr', 'conclusion', 'redis_version', 'commands',
               'commands_present', 'subcommands', 'subcommands_present', 'url']
CENSUS_AREAS = ['artifact_id', 'run_id', 'time', 'branch', 'area', 'commands', 'commands_present', 'subcommands',
                'subcommands_present']
CENSUS_LATEST = ['run_id', 'time', 'branch', 'name', 'kind', 'area', 'since', 'deprecated', 'keel']
TELEMETRY_RUNS = ['artifact_id', 'run_id', 'time', 'event', 'branch', 'pr', 'conclusion', 'host',
                  'keel_metric_names', 'redis_metric_names', 'shared_metric_names', 'url']
TELEMETRY_PHASES = ['artifact_id', 'run_id', 'time', 'branch', 'host', 'phase', 'keel_ops', 'redis_ops']

# Which artifact each kind comes from, the members it needs, and its files. A
# kind with a latest file also keeps one snapshot: the newest run of develop.
KINDS = {
    'command-path': {'artifact': 'command-path-benchmarks',
                     # The raw outputs only for go test's cpu line (see command_path_records).
                     'members': re.compile(r'^(comparison\.json|provenance\.txt|baseline\.txt|candidate\.txt)$'),
                     'files': {'runs': COMMAND_PATH_RUNS, 'rows': COMMAND_PATH_ROWS}},
    'matched': {'artifact': 'matched-keyspace-adoption',
                'members': re.compile(r'^(lscpu\.txt|(baseline|candidate)-source\.txt|[\w.-]+/summary\.json)$'),
                'files': {'runs': MATCHED_RUNS, 'cases': MATCHED_CASES}},
    # The everysec leg's artifact: the same files, kept apart so that a
    # workload's history with the log on is never mixed with the log off.
    'matched-everysec': {'artifact': 'matched-keyspace-adoption-everysec',
                         'members': re.compile(r'^(lscpu\.txt|(baseline|candidate)-source\.txt|[\w.-]+/summary\.json)$'),
                         'files': {'runs': MATCHED_RUNS, 'cases': MATCHED_CASES}},
    'census': {'artifact': 'command-census', 'members': re.compile(r'^census\.json$'),
               'files': {'runs': CENSUS_RUNS, 'areas': CENSUS_AREAS}, 'latest': CENSUS_LATEST},
    'telemetry': {'artifact': 'telemetry',
                  'members': re.compile(r'^(provenance\.txt|load/compat\.json|load/timeline\.json)$'),
                  'files': {'runs': TELEMETRY_RUNS, 'phases': TELEMETRY_PHASES}},
}


def host_of(model):
    """A short name for a CPU model: the family and the model number."""
    if not model:
        return ''
    # Some hosts report the model in capitals (INTEL(R) XEON(R) PLATINUM 8573C).
    m = re.search(r'\b(EPYC|Xeon)(?:\(R\))?\s+(?:(?:Platinum|Gold|Silver|Bronze|CPU)\s+)*([\w-]+(?: v\d+)?)',
                  model, re.I)
    if not m:
        return ' '.join(model.split())
    return f"{'EPYC' if m[1].upper() == 'EPYC' else 'Xeon'} {m[2]}"


def num(value, places):
    if value is None:
        return ''
    return round(value) if places == 0 else round(value, places)


def run_fields(run, pr):
    """The columns every runs file shares, from the GitHub run."""
    return {'run_id': run['id'], 'time': run['created_at'], 'event': run['event'], 'branch': run['head_branch'],
            'pr': pr or '', 'conclusion': run.get('conclusion') or '', 'url': run['html_url']}


def parse_provenance(text):
    found = {'baseline': '', 'candidate': '', 'cpus': '', 'cpu_model': '', 'go': '', 'identical': 'no'}
    for line in text.splitlines():
        if m := re.match(r'^(baseline|candidate) ([0-9a-f]{7,40})$', line):
            found[m[1]] = m[2]
        elif m := re.match(r'^runner .* cpus (\d+)(?: (.+))?$', line):
            found['cpus'], found['cpu_model'] = int(m[1]), (m[2] or '').strip()
        elif m := re.match(r'^go version (go\S+)', line):
            found['go'] = m[1]
        elif line.startswith('binaries identical'):
            found['identical'] = 'yes'
    return found


def gmedian(values):
    """The median on a log scale, as compare-benchmarks.py takes it."""
    return math.exp(statistics.median(math.log(v) for v in values)) if values else None


def go_cpu(text):
    """The cpu line go test prints before its results."""
    m = re.search(r'^cpu: (.+)$', text, re.M)
    return m[1].strip() if m else ''


def command_path_records(artifact_id, run, pr, files):
    """runs.csv and rows.csv lines for one Command path run.

    Runs from before the pairing of October 3, 2026 (65ebdbc) wrote an older
    comparison.json: each row has its median ratio and allocations, with no
    intervals, pair range or B/op, and there is no overall section. Their
    provenance has no CPU model, which go test's cpu line gives instead. For
    those the median across benchmarks is taken here, as the script now does,
    and their intervals are left empty."""
    prov = parse_provenance(files.get('provenance.txt', ''))
    comparison = json.loads(files['comparison.json']) if 'comparison.json' in files else {}
    rows_in = comparison.get('rows', [])
    model = prov['cpu_model'] or go_cpu(files.get('candidate.txt', '')) or go_cpu(files.get('baseline.txt', ''))
    head = {'artifact_id': artifact_id, **run_fields(run, pr)}
    overall = comparison.get('overall') or {}
    low, high = overall.get('interval_95') or (None, None)
    medians = [r['median_ratio'] for r in rows_in]
    row_min, row_max = overall.get('row_range') or ((min(medians), max(medians)) if medians else (None, None))
    if not comparison:
        failures = 1  # A run with no comparison.json compared nothing.
    else:
        failures = len(comparison.get('failures', comparison.get('allocation_regressions', [])))
    host = host_of(model)
    runs = [{**head, 'baseline': prov['baseline'], 'candidate': prov['candidate'], 'identical': prov['identical'],
             'host': host, 'cpu_model': model, 'cpus': prov['cpus'], 'go': prov['go'],
             'rounds': comparison.get('rounds', {}).get('count', ''), 'benchmarks': len(rows_in),
             'median_ratio': num(overall.get('median_ratio', gmedian(medians)), 4), 'interval_low': num(low, 4),
             'interval_high': num(high, 4), 'row_min': num(row_min, 4), 'row_max': num(row_max, 4),
             'order_effect': num(overall.get('order_effect'), 4), 'failures': failures}]
    rows = []
    for r in rows_in:
        low, high = r.get('interval_95') or (None, None)
        pair_min, pair_max = r.get('pair_range') or (None, None)
        b = r.get('base_metrics') or {'allocs/op': r.get('base_allocs')}
        c = r.get('cand_metrics') or {'allocs/op': r.get('cand_allocs')}
        rows.append({'artifact_id': artifact_id, 'run_id': run['id'], 'time': run['created_at'],
                     'branch': run['head_branch'], 'pr': pr or '', 'host': host,
                     'benchmark': r['name'].removeprefix('Benchmark'),
                     'median_ratio': num(r['median_ratio'], 4), 'interval_low': num(low, 4),
                     'interval_high': num(high, 4), 'pair_min': num(pair_min, 4),
                     'pair_max': num(pair_max, 4), 'baseline_ns': num(r['base_ns'], 2),
                     'candidate_ns': num(r['cand_ns'], 2), 'baseline_allocs': num(b.get('allocs/op'), 2),
                     'candidate_allocs': num(c.get('allocs/op'), 2), 'baseline_bytes': num(b.get('B/op'), 1),
                     'candidate_bytes': num(c.get('B/op'), 1)})
    return {'runs': runs, 'rows': rows}


def lscpu_model(text):
    m = re.search(r'^Model name:\s*(.+)$', text, re.M)
    return m[1].strip() if m else ''


def stat(side, key, places):
    return num((side.get(key) or {}).get('median'), places)


def matched_records(artifact_id, run, pr, files):
    """runs.csv and cases.csv lines for one matched adoption run."""
    model = lscpu_model(files.get('lscpu.txt', ''))
    host = host_of(model)
    suites = {}
    for name, text in files.items():
        if name.endswith('/summary.json'):
            suites[name.split('/')[0]] = json.loads(text)
    statuses = {s.get('status', 'unknown') for s in suites.values()}
    status = ('no summary' if not suites else 'passed' if statuses == {'passed'}
              else ' '.join(sorted(statuses - {'passed'})))
    runs = [{'artifact_id': artifact_id, **run_fields(run, pr),
             'baseline': files.get('baseline-source.txt', '').strip(),
             'candidate': files.get('candidate-source.txt', '').strip(), 'host': host, 'cpu_model': model,
             'suites': ' '.join(sorted(suites)), 'status': status}]
    cases = []
    for suite in sorted(suites):
        summary = suites[suite]
        for workload, case in summary.get('cases', {}).items():
            b, c = case.get('baseline', {}), case.get('candidate', {})
            ratio = case.get('candidate_baseline_throughput_ratio') or {}
            cases.append({'artifact_id': artifact_id, 'run_id': run['id'], 'time': run['created_at'],
                          'branch': run['head_branch'], 'pr': pr or '', 'host': host, 'suite': suite,
                          'workload': workload, 'repetitions': summary.get('repetitions', ''),
                          'ratio_median': num(ratio.get('median'), 4), 'ratio_min': num(ratio.get('min'), 4),
                          'ratio_max': num(ratio.get('max'), 4), 'baseline_ops': stat(b, 'ops_per_second', 0),
                          'candidate_ops': stat(c, 'ops_per_second', 0), 'baseline_p99_ms': stat(b, 'p99_ms', 3),
                          'candidate_p99_ms': stat(c, 'p99_ms', 3), 'baseline_p999_ms': stat(b, 'p999_ms', 3),
                          'candidate_p999_ms': stat(c, 'p999_ms', 3), 'baseline_rss_mib': stat(b, 'rss_mib', 2),
                          'candidate_rss_mib': stat(c, 'rss_mib', 2),
                          'client_cpu_warnings': b.get('client_cpu_warnings', 0) + c.get('client_cpu_warnings', 0)})
    return {'runs': runs, 'cases': cases}


def census_records(artifact_id, run, pr, files):
    """runs.csv, areas.csv and latest.csv lines for one census."""
    census = json.loads(files['census.json'])
    total = census['summary']['total']
    runs = [{'artifact_id': artifact_id, **run_fields(run, pr), 'redis_version': census.get('redis_version', ''),
             'commands': total['commands'], 'commands_present': total['commands_present'],
             'subcommands': total['subcommands'], 'subcommands_present': total['subcommands_present']}]
    areas = [{'artifact_id': artifact_id, 'run_id': run['id'], 'time': run['created_at'],
              'branch': run['head_branch'], 'area': area, **counts}
             for area, counts in census['summary']['areas'].items()]
    latest = [{'run_id': run['id'], 'time': run['created_at'], 'branch': run['head_branch'], 'name': c['name'],
               'kind': c['kind'], 'area': f"module:{c['module']}" if c.get('module') else c['group'],
               'since': c.get('since', ''), 'deprecated': 'yes' if c.get('deprecated') else 'no', 'keel': c['keel']}
              for c in census['commands']]
    return {'runs': runs, 'areas': areas, 'latest': latest}


def telemetry_records(artifact_id, run, pr, files):
    """runs.csv and phases.csv lines for one Live telemetry run."""
    host = host_of(parse_provenance(files.get('provenance.txt', ''))['cpu_model'])
    compat = json.loads(files['load/compat.json']) if 'load/compat.json' in files else {}
    timeline = json.loads(files['load/timeline.json']) if 'load/timeline.json' in files else []
    runs = [{'artifact_id': artifact_id, **run_fields(run, pr), 'host': host,
             'keel_metric_names': compat.get('keel', ''), 'redis_metric_names': compat.get('redis', ''),
             'shared_metric_names': compat.get('shared', '')}]
    phases = [{'artifact_id': artifact_id, 'run_id': run['id'], 'time': run['created_at'],
               'branch': run['head_branch'], 'host': host, 'phase': t['phase'],
               'keel_ops': num(t['ops_per_second'].get('keel'), 0), 'redis_ops': num(t['ops_per_second'].get('redis'), 0)}
              for t in timeline]
    return {'runs': runs, 'phases': phases}


RECORDS = {'command-path': command_path_records, 'matched': matched_records,
           'matched-everysec': matched_records, 'census': census_records, 'telemetry': telemetry_records}


def read_members(names_and_readers, pattern):
    """The wanted members of an artifact as text, from (name, size, read) triples."""
    files = {}
    for name, size, read in names_and_readers:
        name = name.removeprefix('./')
        if pattern.match(name):
            if size > MAX_MEMBER_BYTES:
                raise ValueError(f'{name} is {size} bytes, over the {MAX_MEMBER_BYTES}-byte limit')
            files[name] = read().decode('utf-8', 'replace')
    return files


def zip_members(data, pattern):
    with zipfile.ZipFile(io.BytesIO(data)) as z:
        return read_members(((i.filename, i.file_size, lambda i=i: z.read(i)) for i in z.infolist()), pattern)


def dir_members(root, pattern):
    paths = [p for p in Path(root).rglob('*') if p.is_file()]
    return read_members(((p.relative_to(root).as_posix(), p.stat().st_size, p.read_bytes) for p in paths), pattern)


class Store:
    """The CSV files of one kind under the data directory."""

    def __init__(self, data, kind):
        self.dir = Path(data) / kind
        self.files = KINDS[kind]['files']
        self.lines = {name: self.load(name, columns) for name, columns in self.files.items()}
        # The snapshot of the newest run of develop, for a kind that keeps one:
        # its time as the file has it, and the lines that will replace it.
        self.latest_columns = KINDS[kind].get('latest')
        held = self.load('latest', self.latest_columns) if self.latest_columns else []
        self.latest_time = held[0]['time'] if held else ''
        self.latest = None

    def path(self, name):
        return self.dir / f'{name}.csv'

    def load(self, name, columns):
        path = self.path(name)
        if not path.exists():
            return []
        with open(path, newline='') as f:
            reader = csv.DictReader(f)
            if reader.fieldnames != columns:
                raise SystemExit(f'{path} has columns {reader.fieldnames}, expected {columns}; '
                                 'migrate the file before collecting into it')
            return list(reader)

    def recorded(self):
        return {line['artifact_id'] for line in self.lines['runs']}

    def add(self, records):
        for name, lines in records.items():
            if name == 'latest':
                self.offer_latest(lines)
            else:
                self.lines[name] += [{k: str(v) for k, v in line.items()} for line in lines]

    def offer_latest(self, lines):
        """Keep these lines as the snapshot if they come from develop and are
        newer than the one held. A run collected late cannot replace a newer one."""
        if not lines or lines[0]['branch'] != 'develop' or lines[0]['time'] <= self.latest_time:
            return
        self.latest_time = lines[0]['time']
        self.latest = [{k: str(v) for k, v in line.items()} for line in lines]

    def save(self):
        self.dir.mkdir(parents=True, exist_ok=True)
        for name, columns in self.files.items():
            # Sorted by time, so the files read in order and diff cleanly.
            lines = sorted(self.lines[name], key=lambda line: (line['time'], int(line['artifact_id'])))
            with open(self.path(name), 'w', newline='') as f:
                writer = csv.DictWriter(f, fieldnames=columns, lineterminator='\n', quoting=csv.QUOTE_MINIMAL)
                writer.writeheader()
                writer.writerows(lines)
        if self.latest is not None:
            with open(self.path('latest'), 'w', newline='') as f:
                writer = csv.DictWriter(f, fieldnames=self.latest_columns, lineterminator='\n')
                writer.writeheader()
                writer.writerows(self.latest)


class GitHub:
    """The GitHub API through the gh CLI."""

    def __init__(self, repo):
        self.repo = repo
        self.prs = {}

    def api(self, path, *args):
        out = subprocess.run(['gh', 'api', f'repos/{self.repo}/{path}', *args], check=True, capture_output=True)
        return out.stdout

    def artifacts(self, name):
        out = self.api(f'actions/artifacts?name={name}&per_page=100', '--paginate', '--jq', '.artifacts[]')
        return [json.loads(line) for line in out.splitlines() if line.strip()]

    def run(self, run_id):
        return json.loads(self.api(f'actions/runs/{run_id}'))

    def download(self, artifact_id):
        return self.api(f'actions/artifacts/{artifact_id}/zip')

    def pr(self, sha, branch):
        """The pull request a commit heads, preferring one from the run's branch."""
        if sha not in self.prs:
            try:
                pulls = json.loads(self.api(f'commits/{sha}/pulls'))
            except subprocess.CalledProcessError:
                pulls = []  # A commit force-pushed away may no longer be found.
            mine = [p for p in pulls if p['head']['ref'] == branch] or pulls
            self.prs[sha] = mine[0]['number'] if mine else ''
        return self.prs[sha]


def describe(error):
    if isinstance(error, subprocess.CalledProcessError):
        return f"{' '.join(error.cmd[:3])}: {error.stderr.decode('utf-8', 'replace').strip()}"
    return f'{type(error).__name__}: {error}'


def collect(gh, data, kinds, saved=None, max_new=None, workers=8):
    """Add every new run of each kind to its files. Returns a count per
    outcome and the errors, one per artifact that could not be read."""
    counts, errors = {}, []
    for kind in kinds:
        store = Store(data, kind)
        seen = store.recorded()
        spec = KINDS[kind]
        # Oldest first: those expire first.
        todo = sorted((a for a in gh.artifacts(spec['artifact']) if str(a['id']) not in seen),
                      key=lambda a: (a['created_at'], a['id']))[:max_new]

        def fetch(artifact):
            run_id = artifact['workflow_run']['id']
            local = Path(saved) / str(run_id) if saved else None
            local = local if local and local.is_dir() else None
            if artifact.get('expired') and not local:
                return 'expired before collection', None
            try:
                run = gh.run(run_id)
                if run['status'] != 'completed':
                    return 'still running', None
                if (run.get('head_repository') or {}).get('full_name') != gh.repo:
                    return 'from a fork', None
                run['head_branch'] = run.get('head_branch') or ''
                files = (dir_members(local, spec['members']) if local
                         else zip_members(gh.download(artifact['id']), spec['members']))
                return 'collected', RECORDS[kind](artifact['id'], run, gh.pr(run['head_sha'], run['head_branch']),
                                                  files)
            except Exception as e:  # One unreadable artifact must not hold back the rest.
                errors.append(f"{kind} artifact {artifact['id']} (run {run_id}): {describe(e)}")
                return 'failed', None

        with ThreadPoolExecutor(workers) as pool:
            for outcome, records in pool.map(fetch, todo):
                if records is not None:
                    store.add(records)
                counts[f'{kind} {outcome}'] = counts.get(f'{kind} {outcome}', 0) + 1
        store.save()
    return counts, errors


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--repo', required=True, help='owner/name')
    ap.add_argument('--data', required=True, help='checkout of the bench-history branch')
    ap.add_argument('--kind', choices=sorted(KINDS), action='append', help='collect only this kind (repeatable)')
    ap.add_argument('--saved', help='extracted artifacts saved before they expired, one directory per run ID')
    ap.add_argument('--max-new', type=int, help='look at no more than this many new artifacts of each kind')
    args = ap.parse_args(argv)
    counts, errors = collect(GitHub(args.repo), args.data, args.kind or sorted(KINDS), args.saved, args.max_new)
    for outcome, n in sorted(counts.items()):
        print(f'{outcome}: {n}')
    for error in errors:
        print(error, file=sys.stderr)
    # What was read is saved either way; the status says whether anything was not.
    return 1 if errors else 0


if __name__ == '__main__':
    sys.exit(main())
