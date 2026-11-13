"""Fast safety/command-contract tests; never start a VM or contact a server."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import linux_suite as suite


class OwnershipTests(unittest.TestCase):
    def test_non_linux_refused_before_reading_marker(self):
        with patch.object(suite.sys, 'platform', 'darwin'):
            with self.assertRaisesRegex(RuntimeError, 'disposable Linux guest'):
                suite.require_guest('a' * 32)

    def test_invalid_token_refused(self):
        with patch.object(suite.sys, 'platform', 'linux'), patch.object(suite.os, 'geteuid', return_value=0, create=True):
            with self.assertRaisesRegex(RuntimeError, 'invalid ownership token'):
                suite.require_guest('not-an-owner')

    def test_wrong_owner_refused_before_bootstrap(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / 'owner'
            marker.write_text('a' * 32)
            with patch.object(suite.sys, 'platform', 'linux'), patch.object(suite.os, 'geteuid', return_value=0, create=True), patch.object(suite, 'OWNER', marker):
                with self.assertRaisesRegex(RuntimeError, 'ownership mismatch'):
                    suite.require_guest('b' * 32)


class CommandTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.h = suite.Harness.__new__(suite.Harness)
        self.h.artifacts = Path(self.temp.name)
        self.h.sequence = 0
        self.h.env = {'HOME': '/home/soartest'}
        self.h.log = lambda text: None

    def test_secret_input_not_logged_or_placed_in_argv(self):
        result = subprocess.CompletedProcess([], 0, b'acknowledged', b'')
        with patch.object(suite.subprocess, 'run', return_value=result) as run:
            self.h.command(['soar', 'secrets', 'stage', '--stdin', 'password'], input=b'private-value')
        args, kwargs = run.call_args
        self.assertEqual(args[0][:4], ['runuser', '-u', 'soartest', '--'])
        self.assertEqual(kwargs['input'], b'private-value')
        for file in self.h.artifacts.iterdir():
            self.assertNotIn(b'private-value', file.read_bytes())

    def test_nonzero_command_preserves_output_and_fails(self):
        result = subprocess.CompletedProcess([], 3, b'assertion failed', b'error detail')
        with patch.object(suite.subprocess, 'run', return_value=result):
            with self.assertRaisesRegex(RuntimeError, 'failed'):
                self.h.command(['go', 'test', './...'])
        self.assertEqual((self.h.artifacts / '001.stdout').read_bytes(), b'assertion failed')
        self.assertEqual((self.h.artifacts / '001.stderr').read_bytes(), b'error detail')

    def test_expected_failure_requires_nonzero_exit_and_matching_error(self):
        with patch.object(suite.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'stale', b'')):
            with self.assertRaisesRegex(AssertionError, 'expected failure'):
                self.h.cli('deploy', expect_error='stale')
        with patch.object(suite.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, b'', b'unrelated')):
            with self.assertRaisesRegex(AssertionError, 'expected failure'):
                self.h.cli('deploy', expect_error='stale')

    def test_https_pins_local_ca_and_hostname_without_insecure_flag(self):
        with patch.object(suite.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}', b'')) as run:
            self.h.https('orders.soar.localhost', '/entries', {'amount_cents': 10})
        args, kwargs = run.call_args
        self.assertNotIn('--insecure', args[0])
        self.assertIn('--cacert', args[0])
        self.assertIn('orders.soar.localhost:443:127.0.0.1', args[0])
        self.assertEqual(json.loads(kwargs['input']), {'amount_cents': 10})


if __name__ == '__main__':
    unittest.main()
