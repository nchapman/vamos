import contextlib
import io
from pathlib import Path
from types import SimpleNamespace
import unittest
import tempfile
from unittest.mock import MagicMock, patch

try:
    from . import build_network
except ImportError:
    import build_network


class BuildNetworkTests(unittest.TestCase):
    def test_native_configuration_accepts_only_identical_existing_content(self):
        h = SimpleNamespace(root=Path('/opt/soar-test'), command=MagicMock())
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(build_network, 'NATIVE_CONFIG', Path(directory) / 'native.conf'):
            build_network.configure_native(h)
            expected = build_network.NATIVE_CONFIG.read_text()
            build_network.configure_native(h)
            self.assertEqual(build_network.NATIVE_CONFIG.read_text(), expected)
            build_network.NATIVE_CONFIG.write_text('foreign configuration')
            h.command.reset_mock()
            with self.assertRaisesRegex(AssertionError, 'configuration differs'):
                build_network.configure_native(h)
            h.command.assert_not_called()
            self.assertEqual(build_network.NATIVE_CONFIG.read_text(), 'foreign configuration')

    def test_host_listener_is_removed_if_readiness_fails(self):
        container = 'a' * 64
        h = SimpleNamespace(root=Path('/opt/soar-test'),
                            command=MagicMock(return_value=SimpleNamespace(stdout=container.encode())))
        with patch.object(build_network.socket, 'socket') as socket_factory:
            ready = socket_factory.return_value.__enter__.return_value
            ready.getsockname.return_value = ('127.0.0.1', 12345)
            ready.accept.side_effect = TimeoutError('readiness deadline')
            with self.assertRaises(TimeoutError):
                with build_network.host_listener(h, 'fixture', 18888, 'PORT; NOTIFY'):
                    self.fail('unready listener was yielded')
        h.command.assert_any_call(['docker', 'rm', '-f', container], user='root')

    def test_invalid_listener_identity_never_authorizes_removal(self):
        h = SimpleNamespace(root=Path('/opt/soar-test'),
                            command=MagicMock(return_value=SimpleNamespace(stdout=b'foreign-container')))
        with patch.object(build_network.socket, 'socket') as socket_factory:
            ready = socket_factory.return_value.__enter__.return_value
            ready.getsockname.return_value = ('127.0.0.1', 12345)
            with self.assertRaisesRegex(AssertionError, 'invalid listener identity'):
                with build_network.host_listener(h, 'fixture', 18888, 'PORT; NOTIFY'):
                    self.fail('invalid identity was yielded')
            ready.accept.assert_not_called()
        self.assertEqual(h.command.call_count, 1)
        self.assertEqual(h.command.call_args.args[0][:3], ['docker', 'run', '-d'])

    def test_probe_requires_successful_public_https_and_all_private_failures(self):
        response = MagicMock()
        response.__enter__.return_value.status = 200
        targets = [('127.0.0.1', 2019), ('172.18.0.1', 18888), ('172.19.0.2', 5432), ('::1', 2019)]
        with patch('urllib.request.urlopen', return_value=response) as public, \
                patch('socket.create_connection', side_effect=OSError('blocked')) as connect, \
                contextlib.redirect_stdout(io.StringIO()):
            exec(build_network.probe_source(targets), {})
        public.assert_called_once_with('https://pypi.org/simple/pip/', timeout=20)
        self.assertEqual([c.args[0] for c in connect.call_args_list], targets)
        self.assertTrue(all(c.kwargs['timeout'] == 2 for c in connect.call_args_list))

    def test_probe_fails_if_any_protected_listener_is_reachable(self):
        response = MagicMock()
        response.__enter__.return_value.status = 200
        connection = MagicMock()
        with patch('urllib.request.urlopen', return_value=response), \
                patch('socket.create_connection', return_value=connection), \
                self.assertRaisesRegex(RuntimeError, 'reached protected listener'):
            exec(build_network.probe_source([('172.18.0.1', 18888)]), {})
        connection.close.assert_called_once()

    def test_total_egress_failure_cannot_pass_as_isolation(self):
        with patch('urllib.request.urlopen', side_effect=OSError('no egress')), \
                patch('socket.create_connection') as connect, \
                self.assertRaises(OSError):
            exec(build_network.probe_source([('172.18.0.1', 18888)]), {})
        connect.assert_not_called()

    def test_guest_configuration_and_scenarios_refuse_other_harness_roots(self):
        harness = SimpleNamespace(root=Path('/not-the-owned-guest'))
        for operation in (build_network.configure_native, build_network.run):
            with self.subTest(operation=operation.__name__), self.assertRaisesRegex(AssertionError, 'owned guest'):
                operation(harness)


if __name__ == '__main__':
    unittest.main()
