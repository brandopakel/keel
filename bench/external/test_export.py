import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('exporter',Path(__file__).with_name('bencher-export.py'))
exporter=importlib.util.module_from_spec(spec);spec.loader.exec_module(exporter)

class ExportTests(unittest.TestCase):
    def test_units_and_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'raw.csv'
            path.write_text('policy,rep,role,scheduled_ms,error\neverysec,0,probe,1.25,timeout\neverysec,1,probe,2.5,\n')
            result=exporter.export(path)
            self.assertEqual(result['tail/everysec/rep-0/probe/p99']['latency']['value'],1250000)
            self.assertEqual(result['tail/everysec/rep-0/probe/errors']['count']['value'],1)
            self.assertEqual(result['tail/everysec/rep-1/probe/errors']['count']['value'],0)
            path.write_text('policy,rep,role,scheduled_ms,error\neverysec,0,probe,nan,\n')
            with self.assertRaises(ValueError):exporter.export(path)
if __name__=='__main__':unittest.main()
