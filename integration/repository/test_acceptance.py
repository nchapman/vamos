"""Deterministic safety tests; do not start Docker, SSH or a VM."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('repository_acceptance', Path(__file__).with_name('acceptance.py'))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class SafetyTests(unittest.TestCase):
    def test_tls_proxy_cannot_create_automatic_port_80_listener(self):
        config = acceptance.fixture_proxy_config(Path('cert.pem'), Path('key.pem'))
        self.assertTrue(config['admin']['disabled'])
        server = config['apps']['http']['servers']['fixture']
        self.assertEqual(server['listen'], ['127.0.0.1:19001'])
        self.assertTrue(server.get('automatic_https', {}).get('disable'))
        self.assertEqual(server['tls_connection_policies'], [{}])
        self.assertEqual(config['apps']['tls']['certificates']['load_files'],
                         [{'certificate': 'cert.pem', 'key': 'key.pem'}])

    def test_secret_output_rejected(self):
        for field in ('access_key_id', 'secret_access_key', 'password'):
            with self.subTest(field=field):
                with self.assertRaisesRegex(AssertionError, 'credential exposed'):
                    acceptance.clean_output(b'prefix synthetic-canary suffix', {field: 'synthetic-canary'})
        acceptance.clean_output(b'repository metadata', {'password': 'synthetic-canary'})

    def test_private_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'kit'
            path.write_text('synthetic')
            path.chmod(0o600)
            acceptance.private(path)
            path.chmod(0o644)
            with self.assertRaisesRegex(AssertionError, 'private file mode'):
                acceptance.private(path)

    def test_container_is_removed_even_if_proxy_removal_fails(self):
        harness = Mock()
        harness.command.side_effect = [RuntimeError('proxy cleanup failed'), None]
        with self.assertRaisesRegex(RuntimeError, 'proxy cleanup failed'):
            acceptance.cleanup_fixture(harness, 'c' * 64, 'a' * 64)
        self.assertEqual([c.args[0] for c in harness.command.call_args_list],
                         [['docker', 'rm', '-f', 'c' * 64], ['docker', 'rm', '-f', '-v', 'a' * 64]])

    def test_proxy_uses_host_loopback_and_read_only_fixture_files(self):
        command = acceptance.fixture_proxy_command('caddy@sha256:' + 'd' * 64, Path('/work'), Path('/work/caddy.json'))
        self.assertEqual(command[command.index('--network') + 1], 'host')
        self.assertIn('type=bind,source=/work,target=/work,readonly', command)
        self.assertEqual(command[-4:], ['caddy', 'run', '--config', '/work/caddy.json'])

    def test_network_cleanup_is_attempted_even_if_container_removal_fails(self):
        harness = Mock()
        harness.command.side_effect = [RuntimeError('container cleanup failed'), None]
        with self.assertRaisesRegex(RuntimeError, 'container cleanup failed'):
            acceptance.cleanup_fixture(harness, None, 'a' * 64, 'b' * 64)
        self.assertEqual(harness.command.call_args_list[-1].args[0], ['docker', 'network', 'rm', 'b' * 64])

    def test_cleanup_never_uses_unvalidated_resource_ids(self):
        harness = Mock()
        acceptance.cleanup_fixture(harness, 'proxy-name', 'fixture-name', 'network-name')
        harness.command.assert_not_called()

    def test_fixture_uses_explicit_network_not_disabled_default_bridge(self):
        network = 'b' * 64
        command = acceptance.fixture_container_command('fixture', 'private.env', network)
        self.assertIn('--network', command)
        self.assertEqual(command[command.index('--network') + 1], network)
        self.assertIn('127.0.0.1:19000:9000', command)

    def test_image_is_digest_pinned(self):
        self.assertRegex(acceptance.IMAGE, r'^rustfs/rustfs@sha256:[a-f0-9]{64}$')

    def test_ownership_precedes_any_fixture_work(self):
        with patch('sys.argv', ['acceptance.py', '--owner', 'a' * 32]), patch.object(acceptance, 'require_guest', side_effect=RuntimeError('unowned')), patch.object(acceptance, 'Harness') as harness, patch.object(acceptance, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, 'unowned'):
                acceptance.main()
            harness.assert_not_called()
            run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
