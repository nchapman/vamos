"""Driver contracts; no SSH, Docker, namespace or host firewall is touched."""
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
import tempfile
from unittest.mock import MagicMock, patch

import firewall_scenarios as fw


class ProbeTests(unittest.TestCase):
    def execute(self, *, allowed, error=None, protocol='tcp'):
        sock = MagicMock()
        sock.connect.side_effect = error
        sock.recv.return_value = b'firewall-probe'
        with patch('socket.socket', return_value=sock):
            exec(fw.probe_source(fw.HOST, 1234, protocol, allowed), {})
        sock.close.assert_called_once()
        return sock

    def test_allow_requires_connection(self):
        self.execute(allowed=True)
        with self.assertRaisesRegex(AssertionError, 'allowed packet path timed out'):
            self.execute(allowed=True, error=TimeoutError())

    def test_deny_requires_timeout_not_refusal_or_unreachable(self):
        self.execute(allowed=False, error=TimeoutError())
        for error in (ConnectionRefusedError(), OSError('network unreachable')):
            with self.subTest(error=error), self.assertRaises(type(error)):
                self.execute(allowed=False, error=error)
        with self.assertRaisesRegex(AssertionError, 'protected packet path was reachable'):
            self.execute(allowed=False)

    def test_udp_requires_echo_not_just_send_success(self):
        sock = self.execute(allowed=True, protocol='udp')
        sock.send.assert_called_once_with(b'firewall-probe')
        sock.recv.assert_called_once_with(100)
        with patch('socket.socket') as factory:
            factory.return_value.recv.return_value = b'wrong'
            with self.assertRaises(AssertionError):
                exec(fw.probe_source(fw.HOST, 1234, 'udp'), {})


class DriverTests(unittest.TestCase):
    def harness(self):
        return SimpleNamespace(root=fw.GUEST, user=SimpleNamespace(pw_dir='/home/vamostest'),
                               command=MagicMock(), cli=MagicMock())

    def test_entrypoints_reject_unowned_root_before_mutation(self):
        h = SimpleNamespace(root=Path('/not-owned'), command=MagicMock(), cli=MagicMock())
        for operation in (fw.prepare, fw.verify_restart):
            with self.assertRaisesRegex(AssertionError, 'owned guest'):
                operation(h)
        with self.assertRaisesRegex(AssertionError, 'owned guest'):
            with fw.peer(h):
                self.fail('unsafe peer yielded')
        h.command.assert_not_called()
        h.cli.assert_not_called()

    def test_status_never_accepts_pending_or_degraded_as_confirmed(self):
        h = self.harness()
        for enabled, pending, degraded in ((False, False, False), (True, True, False), (True, False, True)):
            h.cli.return_value.stdout = json.dumps(dict(enabled=enabled, pending=pending, degraded=degraded))
            with self.subTest(enabled=enabled, pending=pending, degraded=degraded), self.assertRaises(AssertionError):
                fw.status(h, True)
        h.cli.return_value.stdout = b'{"enabled":true,"pending":false,"degraded":false}'
        fw.status(h, True)
        h.cli.assert_called_with('server', 'firewall', 'status', '--json', 'local')

    def test_every_packet_probe_uses_namespace_ingress(self):
        h = self.harness()
        p = fw.Peer(h, 'fixture')
        p.probe(fw.HOST, fw.HOST_PORT, allowed=False)
        argv = h.command.call_args.args[0]
        self.assertEqual(argv[:6], ['ip', 'netns', 'exec', 'fixture', 'python3', '-c'])
        self.assertIn('assert not False', argv[6])
        self.assertEqual(h.command.call_args.kwargs['user'], 'root')

    def test_fresh_ssh_is_pinned_and_not_multiplexed(self):
        h = self.harness()
        fw.Peer(h, 'fixture').ssh()
        argv = h.command.call_args.args[0]
        self.assertEqual(argv[:5], ['ip', 'netns', 'exec', 'fixture', 'ssh'])
        for option in ('ControlMaster=no', 'ControlPath=none', 'StrictHostKeyChecking=yes',
                       'HostKeyAlias=127.0.0.1', 'BatchMode=yes'):
            self.assertIn(option, argv)
        self.assertIn('root@' + fw.HOST, argv)

    def test_subnet_overlap_fails_without_creating_namespace(self):
        h = self.harness()
        h.command.return_value.stdout = b'[{"dst":"198.18.0.0/24"}]'
        with self.assertRaisesRegex(AssertionError, 'overlaps existing route'):
            with fw.peer(h):
                self.fail('overlapping namespace created')
        self.assertEqual(h.command.call_count, 1)

    def test_peer_cleanup_uses_only_created_names_and_restores_sshd(self):
        h = self.harness()
        h.command.return_value.stdout = b'[]'
        with patch.object(fw, 'Path') as path, patch.object(fw.secrets, 'token_hex', return_value='abcd1234'):
            path.return_value.read_text.return_value = 'original\n'
            with self.assertRaisesRegex(RuntimeError, 'probe failed'):
                with fw.peer(h):
                    raise RuntimeError('probe failed')
            path.return_value.write_text.assert_called_with('original\n')
        h.command.assert_any_call(['ip', 'link', 'delete', 'fwvabcd1234'], user='root')
        h.command.assert_any_call(['ip', 'netns', 'delete', 'fw-abcd1234'], user='root')
        self.assertFalse(any('-F' in c.args[0] for c in h.command.call_args_list))

    def test_app_inspection_requires_loopback_only_publication(self):
        h = self.harness()
        h.status = MagicMock(return_value={'services': {'web': {'container': 'abc'}}})
        def info(bind):
            return json.dumps([{'NetworkSettings': {'Ports': {'8080/tcp': bind},
                                                   'Networks': {'vamos-app': {'IPAddress': '172.18.0.2'}}}}])
        h.command.return_value.stdout = info([{'HostIp': '127.0.0.1', 'HostPort': '12345'}])
        self.assertEqual(fw.app_info(h, 'app'), ('abc', '172.18.0.2', 12345))
        h.command.return_value.stdout = info([{'HostIp': '0.0.0.0', 'HostPort': '12345'}])
        with self.assertRaisesRegex(AssertionError, 'not loopback-only'):
            fw.app_info(h, 'app')

    def test_disable_snapshot_checks_both_families_without_counters(self):
        h = self.harness()
        fw.unrelated_snapshot(h, 'TEST-FW-test')
        self.assertEqual([c.args[0] for c in h.command.call_args_list],
                         [['iptables', '-S', 'TEST-FW-test'], ['ip6tables', '-S', 'TEST-FW-test']])

    def test_rule_snapshot_ignores_only_counters_comments_and_owned_rules(self):
        before = b'# Generated\n*filter\n:INPUT ACCEPT [14:600]\n:VAMOS-FW-IN - [0:0]\n-A INPUT -j VAMOS-FW-IN\n-A TEST -j RETURN\nCOMMIT\n'
        after = b'*filter\n:INPUT ACCEPT [99:800]\n-A TEST -j RETURN\nCOMMIT\n'
        self.assertEqual(fw.unowned_rules(before), fw.unowned_rules(after))
        self.assertNotEqual(fw.unowned_rules(before), fw.unowned_rules(after.replace(b'ACCEPT', b'DROP')))
        self.assertNotEqual(fw.unowned_rules(before), fw.unowned_rules(after.replace(b'RETURN', b'ACCEPT')))

    def test_prepare_creates_app_and_build_only_after_enable_then_rechecks_restart(self):
        h = self.harness()
        h.log = MagicMock()
        events = []
        h.cli.side_effect = lambda *args, **kwargs: events.append(args[:3])
        with tempfile.TemporaryDirectory() as directory, \
                patch('build_network.configure_native'), patch('recovery.init_identity', return_value={'boot': 'one'}), \
                patch.object(fw, 'status'), patch.object(fw, 'peer'), patch.object(fw, 'wait_http'), \
                patch.object(fw, 'UNITS', Path(directory)), patch.object(fw, 'checks') as checks, \
                patch.object(fw, 'root', return_value=SimpleNamespace(stdout=b'host\nnone\n')) as root:
            h.work = h.artifacts = Path(directory)
            fw.prepare(h)
            checkpoint = json.loads((h.artifacts / fw.CHECKPOINT).read_text())
            dockerfile = (h.work / checkpoint['app'] / 'Dockerfile').read_text()
            self.assertIn('RUN python', dockerfile)
            self.assertIn('https://pypi.org/simple/pip/', dockerfile)
            self.assertIn('--firewall=false', h.cli.call_args_list[0].args)
        self.assertLess(events.index(('server', 'firewall', 'enable')),
                        events.index(('deploy', '--server', 'local')))
        self.assertEqual(checks.call_count, 2)
        root.assert_any_call(h, 'systemctl', 'restart', 'docker', timeout=120)

    def test_rollback_never_confirms_and_waits_for_real_timer(self):
        h = self.harness()
        h.command.return_value.stdout = b'{"firewall":{"enabled":true,"pending":true,"token":"abc"}}'
        p = MagicMock()
        with patch.object(fw, 'status') as status, patch.object(fw, 'Path') as path, \
                patch.object(fw.time, 'sleep') as sleep:
            path.return_value.exists.side_effect = [True, False]
            fw.rollback(h, p)
        argv = h.command.call_args.args[0]
        self.assertIn('root@127.0.0.1', argv)
        request = json.loads(h.command.call_args.kwargs['input'])
        self.assertEqual(request['operation'], 'firewall-enable')
        self.assertNotIn('token', request)
        sleep.assert_called_once_with(1)
        self.assertEqual(status.call_count, 2)
        p.probe.assert_any_call(fw.HOST, fw.HOST_PORT, allowed=False)
        p.probe.assert_any_call(fw.HOST, fw.HOST_PORT)
        h.cli.assert_called_once_with('server', 'firewall', 'disable', '--yes', 'local')


if __name__ == '__main__':
    unittest.main()
