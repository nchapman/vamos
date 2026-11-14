import tempfile
import unittest
from pathlib import Path

from client_recovery import fresh_client


class FakeHarness:
    def __init__(self, work):
        self.work = Path(work)
        self.env = {'HOME': '/trusted-client', 'XDG_CONFIG_HOME': '/original-config'}
        self.calls = []
        self.fail = False

    def cli(self, *args):
        self.calls.append((args, dict(self.env)))
        if self.fail:
            raise RuntimeError('connection failed')


class ClientRecoveryTests(unittest.TestCase):
    def test_only_connects_and_restores_original_client(self):
        with tempfile.TemporaryDirectory() as directory:
            h = FakeHarness(directory)
            original = h.env
            with fresh_client(h):
                self.assertNotEqual(h.env['XDG_CONFIG_HOME'], original['XDG_CONFIG_HOME'])
                self.assertEqual(h.env['HOME'], original['HOME'])
            self.assertIs(h.env, original)
            self.assertEqual(h.calls[0][0], ('server', 'connect', '--domain', 'soar.localhost',
                                           'local', 'root@127.0.0.1'))
            self.assertEqual(len(h.calls), 1)

    def test_failure_does_not_leave_recovery_environment_active(self):
        for failure in ('connect', 'body'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                h = FakeHarness(directory)
                original = h.env
                h.fail = failure == 'connect'
                with self.assertRaises(RuntimeError):
                    with fresh_client(h):
                        raise RuntimeError('resume failed')
                self.assertIs(h.env, original)

    def test_existing_registry_cannot_masquerade_as_fresh(self):
        with tempfile.TemporaryDirectory() as directory:
            h = FakeHarness(directory)
            (h.work / 'recovered-client-config').mkdir()
            with self.assertRaises(AssertionError):
                with fresh_client(h):
                    self.fail('existing client accepted')
            self.assertEqual(h.calls, [])
