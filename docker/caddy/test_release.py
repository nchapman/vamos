from pathlib import Path
import subprocess
import sys
import unittest

from release import plan


class ReleaseTest(unittest.TestCase):
    def test_invalid_registry_response_does_not_emit_a_plan(self):
        script = Path(__file__).with_name('release.py')
        for args, data in [([], '{}'), (['caddy-20261120.0'], '{}'), (['caddy-20261120.0'], 'not json')]:
            result = subprocess.run([sys.executable, script, *args], input=data, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, '')
            self.assertNotIn('Traceback', result.stderr)

    def test_plans(self):
        tags = ['2.11.9', '20261121', '20261121.3']
        ref = 'caddy-20261120.'
        self.assertEqual(plan(ref + '0', tags), dict(line='20261120', version='20261120.0', build='yes'))
        tags.append('20261120.0')  # retry after immutable push, before promotion
        self.assertEqual(plan(ref + '0', tags)['build'], 'no')
        self.assertEqual(plan(ref + '1', tags)['build'], 'yes')
        tags.extend(['20261120', '20261120.1'])
        for bad in [ref + '0', ref + '3', ref + '01', ref + '-1', ref + '1x',
                    'caddy-20260231.0', 'v20261120.2']:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                plan(bad, tags)
