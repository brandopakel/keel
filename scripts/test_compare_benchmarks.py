import contextlib
import importlib.util
import io
import json
from pathlib import Path
import random
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('compare_benchmarks', Path(__file__).with_name('compare-benchmarks.py'))
cb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cb)

HEADER = ('goos: linux\ngoarch: amd64\npkg: github.com/brandopakel/keel/internal/core\n'
          'cpu: AMD EPYC 9V74 80-Core Processor                \n')

# Verbatim lines from a hosted run (37091722334), tabs and all: a custom metric
# sits between ns/op and B/op on the eviction rows.
REAL = '''BenchmarkCommandPath/SET-4            \t  727260\t       311.1 ns/op\t      80 B/op\t       2 allocs/op
BenchmarkCommandPath/MGET-10-4        \t   98815\t      2338 ns/op\t     128 B/op\t       1 allocs/op
BenchmarkCommandPath/ZRANGE-100-WITHSCORES-4         \t   26668\t      8783 ns/op\t    3320 B/op\t     176 allocs/op
BenchmarkCommandPathUnderEviction/random-4           \t  283646\t       942.8 ns/op\t         0.9820 evictions/op\t      95 B/op\t       1 allocs/op
BenchmarkCommandPathUnderEviction/lru-4              \t   74677\t      3477 ns/op\t         0.9996 evictions/op\t    1022 B/op\t      12 allocs/op
BenchmarkCommandPathUnderEviction/lfu-4              \t   89253\t      2680 ns/op\t         1.000 evictions/op\t     940 B/op\t      10 allocs/op
'''


def result(name, ns, allocs=2, extra=None, procs=4, benchmem=True):
    """One result line in Go's layout."""
    line = f"{name}{'' if procs == 1 else f'-{procs}'}    \t  500000\t   {ns:.1f} ns/op"
    for unit, value in (extra or {}).items():
        line += f'\t   {value} {unit}'
    if benchmem:
        line += f'\t   80 B/op\t   {allocs} allocs/op'
    return line + '\n'


def block(round_, first, lines):
    return f'keel-round: {round_}\nkeel-first: {first}\n' + HEADER + ''.join(lines) + 'PASS\n'


def abba(n):
    """The order the workflow uses: odd rounds baseline first."""
    return [(r, 'baseline' if r % 2 else 'candidate') for r in range(1, n + 1)]


def files(rounds=4, base=None, cand=None, order=None):
    """Baseline and candidate texts. base and cand map (round, first) to the
    lines of that side's run."""
    base = base or (lambda r, f: [result('BenchmarkCommandPath/SET', 300)])
    cand = cand or base
    order = order or abba(rounds)
    return (''.join(block(r, f, base(r, f)) for r, f in order),
            ''.join(block(r, f, cand(r, f)) for r, f in order))


def run(base_text, cand_text, resamples=200):
    return cb.compare(cb.parse(base_text, 'baseline'), cb.parse(cand_text, 'candidate'), resamples=resamples)


def failures_matching(res, text):
    return [f for f in res['failures'] if text in f]


class ParsingTests(unittest.TestCase):
    def test_every_metric_on_a_real_line_is_parsed(self):
        res = run(*files(base=lambda r, f: [REAL]))
        self.assertEqual(res['failures'], [])
        rows = {row['name']: row for row in res['rows']}
        self.assertEqual(sorted(rows), sorted([
            'BenchmarkCommandPath/SET', 'BenchmarkCommandPath/MGET-10', 'BenchmarkCommandPath/ZRANGE-100-WITHSCORES',
            'BenchmarkCommandPathUnderEviction/random', 'BenchmarkCommandPathUnderEviction/lru',
            'BenchmarkCommandPathUnderEviction/lfu']))
        lru = rows['BenchmarkCommandPathUnderEviction/lru']
        self.assertEqual(lru['base_metrics'], {'evictions/op': 0.9996, 'B/op': 1022, 'allocs/op': 12})
        self.assertEqual(lru['cand_metrics'], lru['base_metrics'])
        self.assertEqual(lru['base_ns'], 3477)
        self.assertEqual(res['procs'], 4)

    def test_names_keep_their_numbers_without_a_processor_suffix(self):
        lines = [result('BenchmarkCommandPath/SET', 300, procs=1), result('BenchmarkCommandPath/MGET-10', 2300, procs=1)]
        res = run(*files(base=lambda r, f: lines))
        self.assertEqual(res['failures'], [])
        self.assertEqual([row['name'] for row in res['rows']], ['BenchmarkCommandPath/SET', 'BenchmarkCommandPath/MGET-10'])
        self.assertEqual(res['procs'], 1)

    def test_different_processor_counts_fail(self):
        res = run(*files(base=lambda r, f: [result('BenchmarkX/a', 300, procs=4)],
                         cand=lambda r, f: [result('BenchmarkX/a', 300, procs=2)]))
        self.assertTrue(failures_matching(res, 'GOMAXPROCS 4, candidate with 2'))

    def test_rows_that_do_not_parse_fail(self):
        for bad in ['BenchmarkCommandPath/SET-4    \t  727260\t   311.1 ns/op\t  80\n',
                    'BenchmarkCommandPath/SET-4    \t  727260\t   fast ns/op\n',
                    'BenchmarkCommandPath/SET-4    \t  many\t   311.1 ns/op\n',
                    'BenchmarkCommandPath/SET-4    \t  727260\t   311.1 ns/op\t 1 ns/op\n',
                    'BenchmarkCommandPath/SET-4    \t  727260\t   80 B/op\t 1 allocs/op\n',
                    'BenchmarkCommandPath/SET-4    \t\n']:
            with self.subTest(bad=bad):
                base, cand = files()
                res = run(base, cand.replace('PASS\n', bad + 'PASS\n', 1))
                self.assertEqual(len(failures_matching(res, 'candidate line ')), 1, res['failures'])

    def test_failed_benchmark_output_fails(self):
        for bad in ['--- FAIL: BenchmarkCommandPathUnderEviction/lru-4\n',
                    '    keyspace_path_bench_test.go:179: 10 evictions over 100 writes\n',
                    'FAIL\n']:
            with self.subTest(bad=bad):
                base, cand = files()
                res = run(base.replace('PASS\n', bad, 1), cand)
                self.assertTrue(failures_matching(res, 'baseline line '), res['failures'])
                self.assertTrue(failures_matching(res, 'unexpected line'))

    def test_result_before_any_round_fails(self):
        base, cand = files()
        res = run(result('BenchmarkCommandPath/SET', 300) + base, cand)
        self.assertTrue(failures_matching(res, 'result before any keel-round line'))

    def test_benchmark_twice_in_a_round_fails(self):
        base, cand = files(base=lambda r, f: [result('BenchmarkX/a', 300)] * (2 if r == 2 else 1))
        res = run(base, cand)
        self.assertTrue(failures_matching(res, 'appears twice in round 2'))


class PairingTests(unittest.TestCase):
    def test_each_candidate_run_pairs_with_the_baseline_run_of_its_round(self):
        # The machine drifts a lot between rounds; within a round the two
        # sides are equal, so every correctly paired ratio is exactly 1.
        speed = {1: 300, 2: 450, 3: 360, 4: 600, 5: 330, 6: 510}
        lines = lambda r, f: [result('BenchmarkX/a', speed[r])]
        base, cand = files(rounds=6, base=lines)
        # Pairing is by round, not by position in the file.
        blocks = ['keel-round: ' + b for b in cand.split('keel-round: ')[1:]]
        random.Random(1).shuffle(blocks)
        res = run(base, ''.join(blocks))
        self.assertEqual(res['failures'], [])
        row = res['rows'][0]
        self.assertEqual(row['median_ratio'], 1)
        self.assertEqual(row['pair_range'], [1, 1])
        self.assertEqual(row['interval_95'], [1, 1])
        self.assertEqual([p['round'] for p in row['pair_ratios']], [1, 2, 3, 4, 5, 6])

    def test_alternating_order_cancels_a_cost_of_running_second(self):
        # Whichever side runs second is 10% slower; the code is the same.
        slow = 1.1
        base, cand = files(rounds=8, base=lambda r, f: [result('BenchmarkX/a', 300 * (slow if f == 'candidate' else 1))],
                           cand=lambda r, f: [result('BenchmarkX/a', 300 * (slow if f == 'baseline' else 1))])
        res = run(base, cand)
        self.assertEqual(res['failures'], [])
        row = res['rows'][0]
        self.assertAlmostEqual(row['median_ratio'], 1, places=4)
        self.assertAlmostEqual(row['order_effect'], slow, places=3)
        self.assertAlmostEqual(res['overall']['order_effect'], slow, places=3)
        self.assertEqual(row['pair_range'], [round(1 / slow, 4), round(slow, 4)])

    def test_a_real_difference_survives_the_alternation(self):
        base, cand = files(rounds=6, base=lambda r, f: [result('BenchmarkX/a', 300)],
                           cand=lambda r, f: [result('BenchmarkX/a', 330)])
        res = run(base, cand)
        self.assertAlmostEqual(res['rows'][0]['median_ratio'], 1.1, places=3)
        self.assertIn('(over the 0.98 budget)', cb.markdown(res))
        self.assertEqual(res['failures'], [], 'time is reported, not gated')

    def test_unbalanced_order_fails(self):
        res = run(*files(order=[(r, 'baseline') for r in range(1, 7)]))
        self.assertTrue(failures_matching(res, 'orders are unbalanced: 6 rounds ran the baseline first and 0'))
        self.assertIsNone(res['rows'][0]['order_effect'])
        self.assertIn('needs rounds in both orders', cb.markdown(res))

    def test_sides_disagreeing_on_the_order_fail(self):
        base, cand = files()
        cand = cand.replace('keel-round: 2\nkeel-first: candidate', 'keel-round: 2\nkeel-first: baseline')
        res = run(base, cand)
        self.assertTrue(failures_matching(res, 'round 2: baseline says candidate ran first, candidate says baseline'))

    def test_a_round_on_one_side_only_fails(self):
        base, _ = files(rounds=6)
        _, cand = files(rounds=4)
        res = run(base, cand)
        self.assertTrue(failures_matching(res, 'round 5 has no candidate run'))
        self.assertTrue(failures_matching(res, 'round 6 has no candidate run'))

    def test_rounds_must_be_numbered_from_one(self):
        res = run(*files(order=[(2, 'baseline'), (3, 'candidate'), (5, 'baseline'), (6, 'candidate')]))
        self.assertTrue(failures_matching(res, 'rounds are not numbered 1..4'))

    def test_repeated_or_missing_round_markers_fail(self):
        base, cand = files()
        res = run(base.replace('keel-round: 3', 'keel-round: 1'), cand)
        self.assertTrue(failures_matching(res, "round '1' is not a new round number"))
        res = run(base.replace('keel-first: candidate\n', '', 1), cand)
        self.assertTrue(failures_matching(res, 'round 2 has no keel-first line'))
        res = run(base.replace('keel-first: candidate', 'keel-first: neither', 1), cand)
        self.assertTrue(failures_matching(res, "keel-first 'neither'"))

    def test_bootstrap_interval_is_deterministic_and_holds_the_median(self):
        rng = random.Random(7)
        noise = {(r, side): rng.uniform(.95, 1.05) for r in range(1, 13) for side in 'bc'}
        base, cand = files(rounds=12, base=lambda r, f: [result('BenchmarkX/a', 300 * noise[r, 'b']),
                                                         result('BenchmarkX/b', 900 * noise[r, 'b'])],
                           cand=lambda r, f: [result('BenchmarkX/a', 300 * noise[r, 'c']),
                                              result('BenchmarkX/b', 900 * noise[r, 'c'])])
        first, second = run(base, cand), run(base, cand)
        self.assertEqual(first, second)
        for row in first['rows'] + [first['overall']]:
            low, high = row['interval_95']
            self.assertLess(low, high)
            self.assertLessEqual(low, row['median_ratio'])
            self.assertLessEqual(row['median_ratio'], high)


class CoverageTests(unittest.TestCase):
    def test_benchmark_missing_from_one_side_fails(self):
        both = lambda r, f: [result('BenchmarkX/a', 300), result('BenchmarkX/b', 300)]
        one = lambda r, f: [result('BenchmarkX/a', 300)]
        res = run(*files(base=both, cand=one))
        self.assertTrue(failures_matching(res, 'BenchmarkX/b: in the baseline only'))
        res = run(*files(base=one, cand=both))
        self.assertTrue(failures_matching(res, 'BenchmarkX/b: in the candidate only'))
        self.assertEqual(res['one_side_only'], {'baseline': [], 'candidate': ['BenchmarkX/b']})

    def test_benchmark_missing_from_one_round_fails_on_the_counts(self):
        base, cand = files(rounds=4, base=lambda r, f: [result('BenchmarkX/a', 300), result('BenchmarkX/b', 300)],
                           cand=lambda r, f: [result('BenchmarkX/a', 300)] + ([] if r == 3 else [result('BenchmarkX/b', 300)]))
        res = run(base, cand)
        self.assertTrue(failures_matching(res, 'BenchmarkX/b: 4 baseline samples but 3 candidate samples; '
                                               'no candidate sample in round 3'))
        self.assertEqual(res['rows'][1]['pairs'], 3)

    def test_no_benchmark_in_common_fails(self):
        res = run('', '')
        self.assertIn('no benchmark is present on both sides', res['failures'])
        self.assertIsNone(res['overall'])
        cb.markdown(res)


class GateTests(unittest.TestCase):
    def test_more_allocations_fail(self):
        res = run(*files(base=lambda r, f: [result('BenchmarkX/a', 300, allocs=2)],
                         cand=lambda r, f: [result('BenchmarkX/a', 300, allocs=3)]))
        self.assertEqual(res['failures'], ['BenchmarkX/a: 2 -> 3 allocs/op'])

    def test_fewer_allocations_pass(self):
        res = run(*files(base=lambda r, f: [result('BenchmarkX/a', 300, allocs=3)],
                         cand=lambda r, f: [result('BenchmarkX/a', 300, allocs=2)]))
        self.assertEqual(res['failures'], [])

    def test_allocations_on_rows_with_an_extra_metric_are_gated(self):
        cand = REAL.replace('1022 B/op\t      12 allocs/op', '1022 B/op\t      13 allocs/op')
        res = run(*files(base=lambda r, f: [REAL], cand=lambda r, f: [cand]))
        self.assertEqual(res['failures'], ['BenchmarkCommandPathUnderEviction/lru: 12 -> 13 allocs/op'])

    def test_missing_allocations_fail(self):
        res = run(*files(base=lambda r, f: [result('BenchmarkX/a', 300, benchmem=False)]))
        self.assertTrue(failures_matching(res, 'BenchmarkX/a: no allocs/op on both sides to gate'))

    def test_evictions_are_shown_for_both_sides(self):
        res = run(*files(base=lambda r, f: [REAL]))
        text = cb.markdown(res)
        self.assertIn('evictions/op 0.9996 -> 0.9996', text)
        self.assertIn('12 -> 12', text)

    def test_evictions_outside_their_sane_range_fail(self):
        normal = lambda r, f: [REAL]
        for value, side in ((0.4, 'candidate'), (1.6, 'baseline')):
            with self.subTest(value=value):
                odd = REAL.replace('0.9996 evictions/op', f'{value} evictions/op')
                once = lambda r, f: [odd] if r == 2 else [REAL]
                res = run(*files(base=once if side == 'baseline' else normal,
                                 cand=once if side == 'candidate' else normal))
                # One sample out of four leaves the medians equal: only the range fails.
                self.assertEqual(res['failures'], [f'BenchmarkCommandPathUnderEviction/lru: {side} '
                                                   f'evictions/op {value:g} outside 0.5 to 1.5'])

    def test_evictions_that_differ_between_sides_fail(self):
        fewer = REAL.replace('0.9996 evictions/op', '0.9000 evictions/op')
        res = run(*files(base=lambda r, f: [REAL], cand=lambda r, f: [fewer]))
        self.assertEqual(res['failures'], ['BenchmarkCommandPathUnderEviction/lru: evictions/op 0.9996 -> 0.9: '
                                           'the two sides did different work, so their times are not comparable'])

    def test_a_metric_on_one_side_only_fails(self):
        silent = REAL.replace('         0.9996 evictions/op\t', '')
        res = run(*files(base=lambda r, f: [REAL], cand=lambda r, f: [silent]))
        self.assertTrue(failures_matching(res, 'BenchmarkCommandPathUnderEviction/lru: the two sides report '
                                               'different metrics: evictions/op absent from candidate round 1'))


class MainTests(unittest.TestCase):
    def run_main(self, base, cand):
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory) / name for name in ('baseline.txt', 'candidate.txt', 'comparison.json')]
            paths[0].write_text(base)
            paths[1].write_text(cand)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                status = cb.main([str(paths[0]), str(paths[1]), '--json', str(paths[2])])
            return status, out.getvalue(), json.loads(paths[2].read_text())

    def test_clean_comparison_exits_zero_and_writes_everything(self):
        status, text, data = self.run_main(*files(rounds=4, base=lambda r, f: [REAL]))
        self.assertEqual(status, 0)
        self.assertEqual(data['failures'], [])
        self.assertEqual(len(data['rows']), 6)
        self.assertEqual(data['rounds'], {'count': 4, 'baseline_first': [1, 3], 'candidate_first': [2, 4]})
        self.assertIn('95% interval', text)
        self.assertIn('2 ran the baseline first and 2 the candidate first', text)
        self.assertNotIn('FAILED', text)

    def test_any_failure_exits_one_and_is_listed(self):
        status, text, data = self.run_main(*files(order=[(1, 'baseline'), (2, 'baseline')]))
        self.assertEqual(status, 1)
        self.assertIn('**FAILED**', text)
        self.assertIn('- orders are unbalanced', text)
        self.assertEqual(len(data['failures']), 1)


if __name__ == '__main__':
    unittest.main()
