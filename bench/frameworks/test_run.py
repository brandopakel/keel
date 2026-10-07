import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('frameworks_run', Path(__file__).with_name('run.py'))
fr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fr)


class Output(unittest.TestCase):
    def test_first_unknown_command_as_each_client_words_it(self):
        # Verbatim from the local runs of October 7, 2026.
        for output, want in [
            ("ERR unknown command 'brpop', with args beginning with: 'queue:default' '2' (redis://", 'brpop'),
            ("redis.exceptions.ResponseError: unknown command 'EVALSHA', with args beginning with: 'c3f8'", 'evalsha'),
            ("Error: ERR unknown command 'eval', with args beginning with: '--[[   Adds a job", 'eval'),
            ('ok: a job and a delayed job ran', ''),
        ]:
            self.assertEqual(fr.first_unknown(output), want, output)

    def test_commandstats(self):
        text = ('# Commandstats\r\ncmdstat_get:calls=4,usec=8,usec_per_call=2.00,rejected_calls=0,failed_calls=0\r\n'
                'cmdstat_client|setinfo:calls=2,usec=1,usec_per_call=0.50,rejected_calls=0,failed_calls=0\r\n'
                'cmdstat_config|resetstat:calls=1,usec=3,usec_per_call=3.00,rejected_calls=0,failed_calls=0\r\n')
        self.assertEqual(fr.commandstats(text), {'get': 4, 'client|setinfo': 2})


class Summary(unittest.TestCase):
    def test_markdown(self):
        result = lambda passed, unknown='': {'passed': passed, 'first_unknown': unknown}
        text = fr.markdown([
            {'framework': 'Rails cache store', 'redis': result(True), 'keel': result(True),
             'commands_used': {'get': 1, 'set': 1}, 'keel_lacks': []},
            {'framework': 'Sidekiq', 'redis': result(True), 'keel': result(False, 'brpop'),
             'commands_used': {'brpop': 3, 'lpush': 1}, 'keel_lacks': ['brpop']},
        ])
        self.assertIn('| Rails cache store | pass | pass | - | 2 | - |', text)
        self.assertIn('| Sidekiq | pass | fail | `brpop` | 2 | `brpop` |', text)
        self.assertIn('Keel passes 1 of 2 frameworks.', text)


if __name__ == '__main__':
    unittest.main()
