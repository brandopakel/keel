import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('rewrite_validation', Path(__file__).resolve().parents[1]/'bench/run-rewrite-validation.py')
validation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validation)


class RewriteCompatibilityTests(unittest.TestCase):
    def test_missing_append_flag_fails_before_measurement(self):
        flags = ('host', 'port', 'appendonly', 'appendfsync', 'appendfilename',
                 'aof-async-append', 'aof-concurrent-append', 'auto-aof-rewrite-percentage')
        help_text = '\n'.join('  -'+flag+' value\n      help' for flag in flags)
        validation.require_flags(help_text)
        with self.assertRaisesRegex(ValueError, 'aof-concurrent-append'):
            validation.require_flags(help_text.replace('  -aof-concurrent-append', '  -unrelated'))

    def test_info_fields_are_required_and_numeric(self):
        valid = dict(aof_rewrites='0', aof_rewrite_in_progress='0')
        validation.require_persistence_fields(valid)
        for field in valid:
            with self.assertRaisesRegex(ValueError, field):
                validation.require_persistence_fields({key: value for key, value in valid.items() if key != field})
            with self.assertRaisesRegex(ValueError, field):
                validation.require_persistence_fields(dict(valid, **{field: 'unknown'}))
