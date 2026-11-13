"""Live CLI usability checks, only called inside the already-authorized guest."""
import errno
import json
import os
from pathlib import Path
import select
import signal
import struct
import subprocess
import time
import uuid


class Capture:
    """Read real stream events with a deadline, not sleep-based polling."""
    def __init__(self, descriptors):
        self.descriptors = list(descriptors)
        self.output = bytearray()

    def until(self, marker, timeout=30):
        deadline = time.monotonic() + timeout
        while marker not in self.output:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not self.descriptors:
                raise AssertionError(f'stream did not produce {marker!r}: {bytes(self.output[-4000:])!r}')
            ready, _, _ = select.select(self.descriptors, [], [], remaining)
            if not ready:
                raise AssertionError(f'timed out waiting for {marker!r}: {bytes(self.output[-4000:])!r}')
            for descriptor in ready:
                try:
                    data = os.read(descriptor, 65536)
                except OSError as exc:
                    if exc.errno != errno.EIO:
                        raise
                    data = b''
                if data:
                    self.output.extend(data)
                else:
                    self.descriptors.remove(descriptor)
        return bytes(self.output)


def terminal_check(h, app, directory):
    import fcntl
    import pty
    import termios

    argv = ['runuser', '-u', 'soartest', '--', str(h.root / 'bin/soar'),
            'console', '--server', 'local', '--app', app, '--service', 'web']
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(directory)
        os.execvpe(argv[0], argv, h.env)
    capture = Capture([master])
    before = termios.tcgetattr(master)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 80, 0, 0))
    reaped = False
    try:
        capture.until(b'Session: app=')
        # Split sentinels so terminal echo cannot impersonate command output.
        os.write(master, b"printf '__SOAR_%s__\\n' READY; id -u; pwd; stty size\n")
        capture.until(b'__SOAR_READY__\r\n10001\r\n/app\r\n24 80')
        os.write(master, b"python -u -c 'import time; print(\"__BLOCK_\"+\"READY__\",flush=True); time.sleep(60)'\n")
        capture.until(b'__BLOCK_READY__')
        os.write(master, b'\x03')
        os.write(master, b"printf '__AFTER_%s__\\n' INTERRUPT\n")
        capture.until(b'__AFTER_INTERRUPT__')
        watch = ("python -u -c 'import os,signal,sys; "
                 "signal.signal(signal.SIGWINCH,lambda *_: (print(\"__RESIZED__\",flush=True),sys.exit(0)) "
                 "if tuple(os.get_terminal_size())==(100,40) else None); "
                 "print(\"__RESIZE_\"+\"READY__\",flush=True); signal.pause()'\n")
        os.write(master, watch.encode())
        capture.until(b'__RESIZE_READY__')
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 100, 0, 0))
        capture.until(b'__RESIZED__\r\n')
        os.write(master, b'exit 7\n')
        # pidfd gives an event-driven bounded wait on the exact child identity.
        pidfd = os.pidfd_open(pid)
        try:
            if not select.select([pidfd], [], [], 30)[0]:
                raise AssertionError('console did not exit')
        finally:
            os.close(pidfd)
        _, status = os.waitpid(pid, 0)
        reaped = True
        if os.waitstatus_to_exitcode(status) != 7:
            raise AssertionError(f'console lost shell exit status: {status}')
        after = termios.tcgetattr(master)
        mask = termios.ECHO | termios.ICANON | termios.ISIG
        if before[3] & mask != after[3] & mask:
            raise AssertionError('console did not restore local terminal modes')
    finally:
        (h.artifacts / 'cli-terminal.output').write_bytes(capture.output)
        if not reaped:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.waitpid(pid, 0)
        os.close(master)


def follow_check(h, app, host, directory):
    initial, later = 'initial-' + uuid.uuid4().hex, 'later-' + uuid.uuid4().hex
    h.https(host, '/' + initial)
    argv = [str(h.root / 'bin/soar'), 'logs', '--server', 'local', '--app', app,
            '--follow', '--tail', '1', '--timeout', '10s']
    process = subprocess.Popen(['runuser', '-u', 'soartest', '--', *argv],
                               cwd=directory, env=h.env, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    capture = Capture([process.stdout.fileno(), process.stderr.fileno()])
    try:
        capture.until(initial.encode())
        h.https(host, '/' + later)
        capture.until(later.encode())
        if process.wait(timeout=30) == 0:
            raise AssertionError('follow deadline was not reported as interruption')
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=30)
        tail_out, tail_err = process.communicate()
        (h.artifacts / 'cli-follow.output').write_bytes(capture.output + tail_out + tail_err)
        process.stdout.close()
        process.stderr.close()


def run(h):
    app = 'cli-' + uuid.uuid4().hex[:8]
    directory = h.work / app
    directory.mkdir()
    h.command(['chown', '-R', 'soartest:soartest', directory], user='root')
    h.cli('init', app, '--port', '8080', cwd=directory)
    original = (directory / 'soar.yaml').read_bytes()
    h.cli('init', app, '--port', '9090', cwd=directory, expect_error='never overwrites')
    assert (directory / 'soar.yaml').read_bytes() == original
    h.cli('validate', cwd=directory)
    (directory / 'Dockerfile').write_text('FROM python:3.13-slim\nWORKDIR /app\nCOPY server.py .\nUSER 10001:10001\nCMD ["python", "-u", "server.py"]\n')
    (directory / 'server.py').write_text('''from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"cli acceptance")
HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
''')
    h.command(['chown', '-R', 'soartest:soartest', directory], user='root')
    host = app + '.soar.localhost'
    h.hosts([host])
    h.cli('deploy', cwd=directory)
    status = h.status(app)
    identity = status['services']['web']['container']
    inventory = json.loads(h.cli('apps', '--server', 'local', '--json').stdout)['entries']
    entry = next(value for value in inventory if value['app'] == app)
    assert entry['phase'] == 'ready' and entry['services'] == ['web']
    assert any(value['phase'] == 'destroyed' and value['retained_secrets'] for value in inventory)
    command_args = ['', 'two words', '$(touch /tmp/soar-never-create)', "'\"; café", 'line\nbreak', '--']
    code = 'import os,sys,json; print(json.dumps([os.getuid(),os.getcwd(),sys.argv[1:]]))'
    result = h.cli('exec', '--service', 'web', '--', 'python', '-c', code, *command_args, cwd=directory)
    assert json.loads(result.stdout) == [10001, '/app', command_args]
    payload = bytes(range(256)) * 1024
    result = h.cli('exec', '-i', '--service', 'web', '--', 'python', '-c',
                   'import sys; sys.stdout.buffer.write(sys.stdin.buffer.read()); sys.stderr.write("separate-stderr")',
                   cwd=directory, input=payload)
    assert result.stdout == payload and b'separate-stderr' in result.stderr
    for exit_code in (0, 1, 42, 127, 255):
        result = h.command([h.root / 'bin/soar', 'exec', '--server', 'local', '--app', app,
                            '--service', 'web', '--', 'python', '-c', f'raise SystemExit({exit_code})'], check=False)
        assert result.returncode == (1 if exit_code == 255 else exit_code)
        if exit_code == 255:
            assert b'outcome unknown' in result.stderr
    h.cli('console', '--server', 'local', '--app', app, expect_error='real input/output terminals')
    h.cli('exec', '--server', 'local', '--app', app, '--', '/missing-executable', expect_error='executable')
    terminal_check(h, app, directory)
    session_root = Path('/run/soar-sessions')
    assert not [p for p in session_root.iterdir() if p.name != '.lock'], 'ordinary sessions left staging behind'
    follow_check(h, app, host, directory)
    assert h.status(app)['services']['web']['container'] == identity
    h.cli('destroy', '--server', 'local', '--app', app, '--yes')
    inventory = json.loads(h.cli('apps', '--json').stdout)['entries']
    assert next(value for value in inventory if value['app'] == app)['phase'] == 'destroyed'
    result = dict(passed=True, app=app, scenarios=[
        'init validates, binds default and refuses overwrite',
        'apps lists deployed and destroyed/retained metadata',
        'exec preserves argv, 256 KiB binary stdin/output, user, cwd and separate stderr',
        'exec propagates known exits and treats SSH 255 as ambiguous',
        'console rejects pipes; real PTY preserves Ctrl-C, resize, shell exit and terminal modes',
        'logs follow emits initial tail and subsequent output, then respects deadline',
        'sessions do not replace the service and ordinary completion removes staging'])
    (h.artifacts / 'cli-summary.json').write_text(json.dumps(result, indent=2))
    h.log('CLI acceptance scenarios passed')
