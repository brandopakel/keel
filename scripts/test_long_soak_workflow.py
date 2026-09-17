import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    'generate_long_soak_workflow', Path(__file__).with_name('generate-long-soak-workflow.py'))
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


class LongSoakWorkflowTests(unittest.TestCase):
    def test_checked_in_workflow_is_the_generated_one(self):
        self.assertEqual(generator.WORKFLOW.read_text(), generator.render(),
                         'run scripts/generate-long-soak-workflow.py and commit the result')

    def test_segments_make_forty_eight_hours_and_chain_within_each_arm(self):
        self.assertEqual(generator.SEGMENTS * generator.SEGMENT_SECONDS, 48 * 3600)
        text = generator.render()
        for arm, _, _ in generator.ARMS:
            self.assertIn(f'  {arm}-s1:\n    name: ', text)
            self.assertIn(f'needs: build\n    uses:', text)
            for segment in range(2, generator.SEGMENTS + 1):
                self.assertIn(f'  {arm}-s{segment}:\n    name: ', text)
                self.assertIn(f'    needs: {arm}-s{segment - 1}\n', text)
                self.assertIn(f"      previous: 'long-soak-{arm}-s{segment - 1}'\n", text)
        self.assertIn("      previous: ''\n", text)
        self.assertIn(f"github.event_name == 'pull_request' && '{generator.REHEARSAL_SECONDS}'", text)
        self.assertIn('  pull_request:\n    paths:\n      - .github/workflows/long-soak.yml\n', text)

    def test_segment_workflow_exists_and_uploads_what_the_next_segment_needs(self):
        segment = (generator.WORKFLOW.parent / 'soak-segment.yml').read_text()
        for needed in ('primary/store.aof', 'replica/store.aof', 'replica/store.aof.replica-checkpoint',
                       '**/*.json', 'if: failure()', 'long-soak-binary'):
            self.assertIn(needed, segment)


if __name__ == '__main__':
    unittest.main()
