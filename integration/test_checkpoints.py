import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

try:
    from . import checkpoints, command_gate
except ImportError:
    import checkpoints
    import command_gate


@unittest.skipIf(sys.platform == 'win32', 'requires Unix sockets and executable scripts')
class Gates(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='gate-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / 'gate.json'
        self.real = self.root / 'real'
        self.marker = self.root / 'ran'
        self.real.write_text('#!' + sys.executable + '\nimport pathlib,sys\n'
                             + 'pathlib.Path(' + repr(str(self.marker)) + ').touch()\n'
                             + 'sys.stdout.buffer.write(sys.stdin.buffer.read())\n'
                             + 'sys.exit(7 if "fail" in sys.argv else 0)\n')
        self.real.chmod(0o700)

    def launch(self, args, data=b''):
        code = ('import sys;sys.path.insert(0,' + repr(str(Path(command_gate.__file__).parent)) + ');'
                'from command_gate import gate_execute;'
                'sys.exit(gate_execute("docker",' + repr(args) + ',' + repr(str(self.real))
                + ',' + repr(str(self.config)) + '))')
        # A file rather than PIPE avoids introducing any test-owned stdin synchronization.
        stream = tempfile.TemporaryFile()
        stream.write(data)
        stream.seek(0)
        process = subprocess.Popen([sys.executable, '-c', code], stdin=stream,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        stream.close()
        def cleanup():
            if process.poll() is None:
                process.kill()
            process.communicate(timeout=5)
        self.addCleanup(cleanup)
        return process

    def barrier(self, **kwargs):
        return checkpoints.Barrier('docker', ['build'], timeout=5, configpath=self.config, **kwargs)

    def test_before_and_stream(self):
        with self.barrier(contains='target') as barrier:
            process = self.launch(['build', 'target'], b'tar\x00secret\xff')
            event = barrier.wait()
            self.assertEqual(event['pid'], process.pid)
            self.assertEqual(event['argv'], ['build', 'target'])
            self.assertFalse(self.marker.exists())
            barrier.release()
            out, err = process.communicate(timeout=5)
            self.assertEqual((process.returncode, out, err), (0, b'tar\x00secret\xff', b''))
            # A released gate must not catch the same command a second time.
            again = self.launch(['build', 'target'], b'second')
            self.assertEqual(again.communicate(timeout=5)[0], b'second')
            self.assertEqual(again.returncode, 0)
        self.assertFalse(self.config.exists())
        self.assertFalse(barrier.socketpath.exists())

    def test_after(self):
        with self.barrier(when='after') as barrier:
            process = self.launch(['build'])
            barrier.wait()
            self.assertTrue(self.marker.exists())
            self.assertIsNone(process.poll())
            barrier.release()
            process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0)

    def test_eof_fails_closed(self):
        with self.barrier() as barrier:
            process = self.launch(['build'])
            barrier.wait()
        process.communicate(timeout=5)
        self.assertNotEqual(process.returncode, 0)
        self.assertFalse(self.marker.exists())

    def test_unresponsive_controller_times_out_without_execution(self):
        with self.barrier() as barrier:
            config = json.loads(self.config.read_text())
            config['timeout'] = 0.1
            self.config.write_text(json.dumps(config))
            process = self.launch(['build'])
            barrier.wait()
            # Keep the peer open, but never release it. The worker must enforce
            # its own deadline even if the controller remains alive but stuck.
            process.communicate(timeout=2)
            self.assertNotEqual(process.returncode, 0)
            self.assertFalse(self.marker.exists())

    def test_bypass_and_exit_status(self):
        for when, args in [('before', ['other', 'fail']), ('after', ['build', 'fail'])]:
            with self.subTest(when=when), self.barrier(when=when):
                process = self.launch(args, b'untouched')
                out, _ = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 7)
                self.assertEqual(out, b'untouched')
        process = self.launch(['anything'], b'no config')
        self.assertEqual(process.communicate(timeout=5)[0], b'no config')
        self.assertEqual(process.returncode, 0)

    def test_stale_token(self):
        with self.barrier() as barrier:
            with socket.socket(socket.AF_UNIX) as peer:
                peer.connect(str(barrier.socketpath))
                peer.sendall(json.dumps(dict(token='stale', pid=123, argv=['build'])).encode() + b'\n')
                with self.assertRaisesRegex(RuntimeError, 'invalid gate event'):
                    barrier.wait()
                with self.assertRaises(RuntimeError):
                    barrier.release()


class Contracts(unittest.TestCase):
    def test_matching_explicit_local_docker_endpoint(self):
        config = dict(command='docker', prefix=['buildx', 'build'], when='before')
        self.assertTrue(command_gate.matches(config, 'docker',
                        ['--host', 'unix:///var/run/docker.sock', 'buildx', 'build', '--load']))
        self.assertFalse(command_gate.matches(config, 'docker',
                         ['--host', 'tcp://foreign:2375', 'buildx', 'build']))

    def test_matching(self):
        config = dict(command='docker', prefix=['rm'], contains='exact', when='before')
        self.assertTrue(command_gate.matches(config, 'docker', ['rm', '-f', 'exact']))
        for name, args in [('caddy', ['rm', 'exact']), ('docker', ['rmi', 'exact']),
                           ('docker', ['rm', 'exact-more'])]:
            self.assertFalse(command_gate.matches(config, name, args))
        self.assertFalse(command_gate.matches(dict(config, prefix=[]), 'docker', ['rm']))

    @unittest.skipIf(sys.platform == 'win32', 'requires Unix process groups')
    def test_client_contract(self):
        with tempfile.TemporaryDirectory() as directory:
            h = SimpleNamespace(root=Path(directory), artifacts=Path(directory),
                                env={'PATH': '/private/bin', 'HOME': '/home/vamostest'}, sequence=42)
            with mock.patch.object(checkpoints.subprocess, 'Popen') as popen, \
                    mock.patch.object(checkpoints.os, 'killpg', create=True) as killpg:
                process = popen.return_value
                process.pid = 991
                process.wait.return_value = 0
                with checkpoints.Client(h, ['deploy', '--server', 'local'], Path(directory)) as client:
                    call = popen.call_args
                    self.assertEqual(call.args[0], ['runuser', '-u', 'vamostest', '--',
                                                   str(h.root / 'bin/vamos'), 'deploy', '--server', 'local'])
                    self.assertEqual(call.kwargs['env'], h.env)
                    self.assertEqual(call.kwargs['cwd'], Path(directory))
                    self.assertTrue(call.kwargs['start_new_session'])
                    self.assertEqual(call.kwargs['stdin'], subprocess.DEVNULL)
                    client.stdout.write_bytes(b'out\xff')
                    client.stderr.write_bytes(b'err')
                    result = client.wait(timeout=3)
                    self.assertEqual((result.stdout, result.stderr), (b'out\xff', b'err'))
                    self.assertEqual(h.sequence, 42)
                killpg.assert_called_once_with(991, checkpoints.signal.SIGKILL)


if __name__ == '__main__':
    unittest.main()
