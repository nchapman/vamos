"""Deterministic external-command checkpoints; install only after require_guest."""
import hmac
import json
import math
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time

try:
    from .command_gate import CONFIG, MAX_EVENT, RELEASE
except ImportError:
    from command_gate import CONFIG, MAX_EVENT, RELEASE


def install(h):
    if (Path(h.root) != Path('/opt/soar-test') or sys.platform != 'linux'
            or os.geteuid() != 0
            or not Path('/var/lib/soar-integration-owner').is_file()
            or 'orbstack' not in Path('/proc/sys/kernel/osrelease').read_text().lower()):
        raise RuntimeError('gate installation requires the owned OrbStack root guest')
    destinations = [Path('/usr/local/bin') / name for name in ('docker', 'caddy')]
    if any(os.path.lexists(p) for p in destinations):
        raise RuntimeError('refusing existing command wrapper destinations')
    directory = Path(CONFIG).parent
    directory.mkdir(mode=0o700, exist_ok=True)
    if directory.is_symlink() or directory.stat().st_uid != 0:
        raise RuntimeError('unsafe gate directory')
    directory.chmod(0o700)
    source = Path(__file__).with_name('command_gate.py').read_bytes()
    for destination in destinations:
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o755)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(source)
            os.fchmod(stream.fileno(), 0o755)


class Barrier:
    def __init__(self, command, prefix, contains=None, when='before', timeout=120,
                 *, configpath=CONFIG):
        if (type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout <= 1200
                or command not in ('docker', 'caddy') or when not in ('before', 'after')
                or not isinstance(prefix, list) or not prefix
                or not all(isinstance(v, str) for v in prefix)
                or (contains is not None and not isinstance(contains, str))):
            raise ValueError('invalid gate matcher')
        self.path = Path(configpath)
        self.timeout = timeout
        self.config = dict(command=command, prefix=prefix, contains=contains, when=when,
                           timeout=timeout, token=secrets.token_hex(32))
        self.listener = self.peer = None
        self.socketpath = self.path.parent / ('g-' + secrets.token_hex(8) + '.sock')
        self.owned = False

    def __enter__(self):
        # An exclusive side lock prevents competing controllers replacing a live gate.
        self.lock = self.path.with_suffix('.lock')
        fd = os.open(self.lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(fd)
        self.owned = True
        temporary = None
        try:
            if self.path.exists():
                raise RuntimeError('existing gate configuration')
            self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.listener.bind(str(self.socketpath))
            self.socketpath.chmod(0o600)
            self.listener.listen(1)
            self.config['socket'] = str(self.socketpath)
            fd, temporary = tempfile.mkstemp(prefix='.gate-', dir=self.path.parent)
            with os.fdopen(fd, 'w') as stream:
                json.dump(self.config, stream)
            os.replace(temporary, self.path)
            self.installed = True
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise
        finally:
            if temporary and os.path.exists(temporary):
                os.unlink(temporary)

    def wait(self):
        deadline = time.monotonic() + self.timeout
        self.listener.settimeout(self.timeout)
        self.peer, _ = self.listener.accept()
        payload = bytearray()
        while not payload.endswith(b'\n'):
            self.peer.settimeout(max(0.001, deadline - time.monotonic()))
            chunk = self.peer.recv(1)
            if not chunk or len(payload) >= MAX_EVENT:
                raise RuntimeError('invalid gate event framing')
            payload.extend(chunk)
        event = json.loads(payload)
        if (not isinstance(event.get('token'), str)
                or not hmac.compare_digest(event['token'], self.config['token'])
                or type(event.get('pid')) is not int or event['pid'] <= 0
                or not isinstance(event.get('argv'), list)
                or not all(isinstance(v, str) for v in event['argv'])):
            raise RuntimeError('invalid gate event')
        self.event = event
        return event

    def release(self):
        if not hasattr(self, 'event'):
            raise RuntimeError('wait for a validated event before release')
        if getattr(self, 'installed', False):
            self.path.unlink(missing_ok=True)
            self.installed = False
        if self.peer:
            try:
                self.peer.sendall(RELEASE)
            except (BrokenPipeError, ConnectionResetError):
                pass  # Worker cgroup may already have been killed.
            finally:
                self.peer.close()
                self.peer = None

    def __exit__(self, *exc):
        if getattr(self, 'installed', False):
            self.path.unlink(missing_ok=True)
        if self.peer:
            self.peer.close()  # EOF is deliberately not a release.
        if self.listener:
            self.listener.close()
        self.socketpath.unlink(missing_ok=True)
        if self.owned:
            self.lock.unlink(missing_ok=True)


class Client:
    """Asynchronous CLI with private artifacts, never sharing Harness.sequence."""
    def __init__(self, h, args, cwd):
        self.argv = [str(Path(h.root) / 'bin/soar'), *map(str, args)]
        self.directory = Path(tempfile.mkdtemp(prefix='checkpoint-', dir=h.artifacts))
        self.directory.joinpath('command.json').write_text(json.dumps(self.argv))
        self.stdout = self.directory / 'stdout'
        self.stderr = self.directory / 'stderr'
        with self.stdout.open('wb') as out, self.stderr.open('wb') as err:
            os.chmod(self.stdout, 0o600)
            os.chmod(self.stderr, 0o600)
            self.process = subprocess.Popen(['runuser', '-u', 'soartest', '--', *self.argv],
                                            cwd=cwd, env=h.env, stdin=subprocess.DEVNULL,
                                            stdout=out, stderr=err, start_new_session=True)

    def __enter__(self):
        return self

    def wait(self, timeout=1200, check=True):
        rc = self.process.wait(timeout=timeout)
        result = subprocess.CompletedProcess(self.argv, rc, self.stdout.read_bytes(), self.stderr.read_bytes())
        if check:
            result.check_returncode()
        return result

    def disconnect(self):
        try:
            os.killpg(self.process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def __exit__(self, *exc):
        self.disconnect()
        self.process.wait(timeout=30)
