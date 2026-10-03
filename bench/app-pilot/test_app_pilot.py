"""Checks for the pilot harness's own logic, which the hosted runs depend on.

A parser that pairs a reply with the wrong command would misreport the command
mix, and a test-count parser that missed a failure would turn a failing suite
into a pass; both are cheaper to catch here than in a twenty-minute run.
"""
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pilot_lib  # noqa: E402
import regression  # noqa: E402
import resp_tap  # noqa: E402
import summarize  # noqa: E402


def resp(*args):
    return b"*%d\r\n" % len(args) + b"".join(b"$%d\r\n%s\r\n" % (len(a), a) for a in args)


class RespParsing(unittest.TestCase):
    def test_command_and_options(self):
        kind, value, end = resp_tap.parse_value(resp(b"SET", b"count", b"ex", b"EX", b"60", b"NX"), 0)
        rec = resp_tap.command_record(kind, value)
        self.assertEqual(rec["cmd"], "SET")
        # The key "count" and the value "ex" are not options; only EX and NX are.
        self.assertEqual(rec["opts"], ["EX", "NX"])

    def test_subcommand(self):
        kind, value, _ = resp_tap.parse_value(resp(b"client", b"setinfo", b"LIB-NAME", b"go-redis"), 0)
        rec = resp_tap.command_record(kind, value)
        self.assertEqual((rec["cmd"], rec["sub"]), ("CLIENT", "SETINFO"))

    def test_incomplete_input_waits(self):
        whole = resp(b"GET", b"key")
        with self.assertRaises(resp_tap.Incomplete):
            resp_tap.parse_value(whole[:-3], 0)

    def test_resp3_map_reply_is_one_value(self):
        reply = b"%2\r\n+server\r\n+redis\r\n+proto\r\n:3\r\n+OK\r\n"
        kind, value, end = resp_tap.parse_value(reply, 0)
        self.assertEqual(kind, "%")
        self.assertEqual(resp_tap.parse_value(reply, end)[0], "+")

    def test_inline_command(self):
        kind, value, _ = resp_tap.parse_value(b"PING\r\n", 0)
        self.assertEqual(resp_tap.command_record(kind, value)["cmd"], "PING")


class TapStream(unittest.TestCase):
    def test_malformed_frame_stops_only_logging(self):
        values, errors = [], []
        stream = resp_tap.Stream(lambda k, v: values.append(k), errors.append)
        stream.feed(resp(b"PING") + b"$abc\r\n")
        stream.feed(resp(b"GET", b"k"))  # forwarded by pipe(), no longer parsed
        self.assertEqual(values, ["*"])
        self.assertEqual(len(errors), 1)
        self.assertTrue(stream.failed)

    def test_split_frames_are_reassembled(self):
        values = []
        stream = resp_tap.Stream(lambda k, v: values.append(resp_tap.command_record(k, v)["cmd"]), self.fail)
        whole = resp(b"SET", b"k", b"v") + resp(b"GET", b"k")
        for i in range(0, len(whole), 3):
            stream.feed(whole[i:i + 3])
        self.assertEqual(values, ["SET", "GET"])


class WireSummary(unittest.TestCase):
    def test_unsupported_separates_client_probes(self):
        records = [{"conn": 1, "event": "open"},
                   {"conn": 1, "cmd": "HELLO", "sub": "3", "reply": "-", "error": "ERR unknown command 'HELLO'"},
                   {"conn": 1, "cmd": "CLIENT", "sub": "SETINFO", "reply": "-",
                    "error": "ERR unknown command 'CLIENT'"},
                   {"conn": 1, "cmd": "GETEX", "reply": "-", "error": "ERR unknown command 'GETEX'"},
                   {"conn": 1, "cmd": "GET", "reply": "$"}]
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / "wire.jsonl"
            log.write_text("".join(json.dumps(r) + "\n" for r in records))
            wire = pilot_lib.summarize_wire([log])
        self.assertEqual(wire["connections"], 1)
        self.assertEqual(wire["unsupported"], ["CLIENT SETINFO", "GETEX", "HELLO"])
        self.assertEqual(wire["unsupported_outside_handshake"], ["GETEX"])
        self.assertEqual(wire["commands"]["GET"]["count"], 1)


class Percentiles(unittest.TestCase):
    def test_nearest_rank(self):
        values = [i / 1000 for i in range(1, 101)]
        self.assertEqual(pilot_lib.percentile(values, 50), 0.05)
        self.assertEqual(pilot_lib.percentile(values, 99), 0.099)
        self.assertIsNone(pilot_lib.percentile([], 50))
        self.assertEqual(pilot_lib.latency_summary([0.002])["p99_ms"], 2.0)


class GoTestJson(unittest.TestCase):
    def write(self, events):
        tmp = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
        tmp.write("".join(json.dumps(e) + "\n" for e in events))
        tmp.close()
        return tmp.name

    def test_counts_and_failures(self):
        path = self.write([
            {"Action": "run", "Package": "p", "Test": "TestA"},
            {"Action": "output", "Package": "p", "Test": "TestA", "Output": "boom: SELECT\n"},
            {"Action": "fail", "Package": "p", "Test": "TestA/Round_7"},
            {"Action": "pass", "Package": "p", "Test": "TestA/Round_1"},
            {"Action": "fail", "Package": "p", "Test": "TestA"},
            {"Action": "pass", "Package": "p", "Test": "TestB"},
            {"Action": "output", "Package": "p", "Test": "TestGinkgo",
             "Output": "\x1b[1mRan 45 of 45 Specs in 0.9 seconds\x1b[0m\n"},
            {"Action": "output", "Package": "p", "Test": "TestGinkgo",
             "Output": "SUCCESS! -- 45 Passed | 0 Failed | 0 Pending | 0 Skipped\n"},
            {"Action": "fail", "Package": "p"},
            # test2json keeps Ginkgo's colour codes but not their ESC byte.
            {"Action": "output", "Package": "q", "Test": "TestGinkgo",
             "Output": "[1m[32mRan 3 of 3 Specs in 0.1 seconds[0m\n"},
            {"Action": "output", "Package": "q", "Test": "TestGinkgo",
             "Output": "[1m[32mSUCCESS![0m -- [32m[1m3 Passed[0m | [91m[1m0 Failed[0m | "
                       "[33m[1m0 Pending[0m | [36m[1m0 Skipped[0m\n"},
        ])
        parsed = regression.parse_go_test_json(path)
        self.assertEqual(parsed["top_level"], {"fail": 1, "pass": 1})
        self.assertEqual(parsed["subtests"], {"fail": 1, "pass": 1})
        self.assertEqual(parsed["ginkgo"], [{"package": "p", "ran": 45, "of": 45, "passed": 45, "failed": 0,
                                             "pending": 0, "skipped": 0},
                                            {"package": "q", "ran": 3, "of": 3, "passed": 3, "failed": 0,
                                             "pending": 0, "skipped": 0}])
        self.assertEqual({f["test"] for f in parsed["failing"]}, {"TestA", "TestA/Round_7"})

    def test_build_failure_is_a_failure(self):
        path = self.write([{"Action": "build-output", "ImportPath": "p", "Output": "cannot find module\n"},
                           {"Action": "fail", "Package": "p", "Output": "[setup failed]"}])
        parsed = regression.parse_go_test_json(path)
        self.assertEqual(parsed["packages"], {"p": "fail"})
        self.assertEqual(parsed["failing"][0]["result"], "fail")


class SuiteVerdict(unittest.TestCase):
    def test_a_suite_that_ran_nothing_fails(self):
        self.assertFalse(regression.suite_passed({"exit_code": 0, "top_level": {}}))
        self.assertTrue(regression.suite_passed({"exit_code": 0, "top_level": {"pass": 3}}))
        self.assertFalse(regression.suite_passed({"exit_code": 0, "top_level": {"pass": 3},
                                                  "harness_error": "x"}))


class Summary(unittest.TestCase):
    def test_arm_is_matched_exactly(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("pilot-app-redis-old", "pilot-app-redis"):
                (root / name).mkdir()
                (root / name / "results.json").write_text("{}")
            self.assertEqual(summarize.find(root, "results.json", "redis").parent.name, "pilot-app-redis")


    def test_missing_arm_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "pilot-regression-keel").mkdir()
            (root / "pilot-regression-keel/regression.json").write_text(json.dumps({
                "arm": "keel", "passed": True, "suites": [{"name": "s", "commit": "c", "gates": True,
                                                           "passed": True, "top_level": {"pass": 2}}]}))
            summary = summarize.build(root)
            short = summarize.compact(summary)
            text = summarize.markdown(summary, short)
        self.assertIn("missing", summary["regression"]["redis"])
        self.assertIn("missing", short["app"]["keel"])
        self.assertIn("| s | yes | pass: 2/2 tests | not run |", text)


if __name__ == "__main__":
    unittest.main()
