"""Guest-only checks and integration driver; invoked by the OrbStack runner."""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import time
import traceback
import urllib.request

ROOT = Path('/opt/vamos-test')
OWNER = Path('/var/lib/vamos-integration-owner')
CA = Path('/var/lib/caddy/data/caddy/pki/authorities/local/root.crt')


def require_guest(owner):
    # Never bootstrap a workstation or an arbitrary existing Linux host.
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise RuntimeError('suite requires the disposable Linux guest, as root')
    if not re.fullmatch(r'[0-9a-f]{32}', owner):
        raise RuntimeError('invalid ownership token')
    if not hmac.compare_digest(OWNER.read_text().strip(), owner):
        raise RuntimeError('guest ownership mismatch')
    if 'orbstack' not in Path('/proc/sys/kernel/osrelease').read_text().lower():
        raise RuntimeError('suite requires an OrbStack guest')
    if Path(__file__).resolve() != ROOT / 'integration/linux_suite.py':
        raise RuntimeError('suite must use the runner-owned source snapshot')
    if Path('/mnt/mac').exists() or Path('/Users').exists():
        raise RuntimeError('refusing a guest with host file integration')


class Harness:
    def __init__(self):
        import pwd  # Guest-only dependency; safety tests also run on Windows.
        self.root = ROOT
        self.work = ROOT / '.integration-work'
        self.artifacts = ROOT / '.integration-artifacts'
        self.work.mkdir(mode=0o700, exist_ok=True)
        self.artifacts.mkdir(mode=0o700, exist_ok=True)
        self.sequence = max((int(p.name.split('.')[0]) for p in self.artifacts.glob('*.command.json')
                             if p.name.split('.')[0].isdigit()), default=0)
        self.user = pwd.getpwnam('vamostest')
        self.env = dict(os.environ, PATH='/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                        GOTOOLCHAIN='local', DEBIAN_FRONTEND='noninteractive',
                        HOME=self.user.pw_dir, XDG_CONFIG_HOME=str(self.work / 'config'))
        for key in ('SSH_AUTH_SOCK', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'GOFLAGS', 'GOOS', 'GOARCH', 'GOPATH', 'GOCACHE'):
            self.env.pop(key, None)

    def log(self, text):
        print(text, flush=True)

    def command(self, argv, input=None, cwd=None, user='vamostest', check=True, timeout=1200):
        self.sequence += 1
        argv = [str(v) for v in argv]
        self.log(f'[{self.sequence:03}] {" ".join(argv)}')
        actual = ['runuser', '-u', user, '--', *argv] if user != 'root' else argv
        env = dict(self.env, HOME='/root') if user == 'root' else self.env
        prefix = self.artifacts / f'{self.sequence:03}'
        (prefix.with_suffix('.command.json')).write_text(json.dumps(argv))
        # Input can contain staged secrets. Never log it or dump process environments.
        try:
            result = subprocess.run(actual, input=input, capture_output=True, cwd=cwd or ROOT,
                                    env=env, timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            prefix.with_suffix('.stdout').write_bytes(exc.stdout or b'')
            prefix.with_suffix('.stderr').write_bytes(exc.stderr or b'')
            raise
        prefix.with_suffix('.stdout').write_bytes(result.stdout)
        prefix.with_suffix('.stderr').write_bytes(result.stderr)
        if check and result.returncode:
            raise RuntimeError(f'command {self.sequence} failed ({result.returncode}): '
                               f'{(result.stdout + result.stderr).decode(errors="replace")[-6000:]}')
        return result

    def cli(self, *args, cwd=None, input=None, expect_error=None):
        result = self.command([ROOT / 'bin/vamos', *args], cwd=cwd, input=input, check=expect_error is None)
        if expect_error is not None:
            output = (result.stdout + result.stderr).decode(errors='replace')
            if result.returncode == 0 or expect_error not in output:
                raise AssertionError(f'expected failure containing {expect_error!r}, got {result.returncode}: {output}')
        return result

    def status(self, app):
        return json.loads(self.cli('status', '--server', 'local', '--app', app, '--json').stdout)

    def hosts(self, names):
        for name in names:
            if not re.fullmatch(r'[a-z0-9.-]+\.localhost', name):
                raise ValueError('test hostname must end in .localhost')
        with Path('/etc/hosts').open('a') as file:
            file.write('\n127.0.0.1 ' + ' '.join(names) + '\n')

    def trust_local_ca(self):
        # Trust is deliberately confined to the disposable guest, never macOS.
        shutil.copyfile(CA, '/usr/local/share/ca-certificates/vamos-integration.crt')
        self.command(['update-ca-certificates'], user='root')

    def https(self, host, path='/', data=None):
        args = ['curl', '--fail-with-body', '--silent', '--show-error', '--max-time', '30',
                '--noproxy', '*', '--cacert', str(CA), '--resolve', f'{host}:443:127.0.0.1',
                f'https://{host}{path}']
        body = None
        if data is not None:
            args += ['--header', 'Content-Type: application/json', '--data-binary', '@-']
            body = json.dumps(data).encode()
        # Caddy's private CA directory is readable by root, not by the CLI user.
        return self.command(args, input=body, user='root').stdout

    def diagnostics(self):
        for args in (['systemctl', '--failed', '--no-pager'],
                     ['systemctl', 'status', 'docker', 'vamos-agent', '--no-pager'],
                     ['journalctl', '-u', 'docker', '-u', 'vamos-agent', '--no-pager', '-n', '150'],
                     ['docker', 'logs', '--tail', '150', 'vamos-caddy'],
                     ['docker', 'ps', '-a', '--format', '{{.Names}}\t{{.Status}}'],
                     ['docker', 'volume', 'ls'], ['docker', 'network', 'ls']):
            try:
                self.command(args, user='root', check=False, timeout=30)
            except (OSError, subprocess.TimeoutExpired) as exc:
                self.log(f'Diagnostics unavailable: {exc}')


def install_go(h):
    match = re.search(r'^go = "(\d+\.\d+\.\d+)"$', (ROOT / 'mise.toml').read_text(), re.M)
    if match is None:
        raise RuntimeError('mise.toml must pin an exact Go version')
    version = 'go' + match.group(1)
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()]
    filename = f'{version}.linux-{arch}.tar.gz'
    with urllib.request.urlopen('https://go.dev/dl/?mode=json&include=all', timeout=60) as response:
        releases = json.load(response)
    candidates = [f for release in releases if release['version'] == version
                  for f in release['files'] if f['filename'] == filename]
    if len(candidates) != 1:
        raise RuntimeError(f'official Go checksum not found for {filename}')
    archive = Path('/tmp') / filename
    digest = hashlib.sha256()
    h.log(f'Downloading and verifying {filename}')
    with urllib.request.urlopen('https://go.dev/dl/' + filename, timeout=120) as response, archive.open('wb') as output:
        while chunk := response.read(1024 * 1024):
            output.write(chunk)
            digest.update(chunk)
    if not hmac.compare_digest(digest.hexdigest(), candidates[0]['sha256']):
        raise RuntimeError('Go archive checksum mismatch')
    with tarfile.open(archive) as source:
        source.extractall('/usr/local', filter='data')
    archive.unlink()
    h.command(['go', 'version'])


def bootstrap(h):
    h.command(['apt-get', 'update'], user='root')
    h.command(['apt-get', 'install', '-y', '--no-install-recommends', 'ca-certificates', 'curl',
               'openssh-server', 'build-essential', 'git'], user='root')
    h.command(['chown', '-R', 'vamostest:vamostest', str(ROOT)], user='root')
    install_go(h)
    ssh_dir = Path(h.user.pw_dir) / '.ssh'
    ssh_dir.mkdir(mode=0o700, exist_ok=True)
    os.chown(ssh_dir, h.user.pw_uid, h.user.pw_gid)
    h.command(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(ssh_dir / 'id_ed25519')])
    root_ssh = Path('/root/.ssh')
    root_ssh.mkdir(mode=0o700, exist_ok=True)
    (root_ssh / 'authorized_keys').write_text((ssh_dir / 'id_ed25519.pub').read_text())
    (root_ssh / 'authorized_keys').chmod(0o600)
    h.command(['ssh-keygen', '-A'], user='root')
    # Read the host key through the already-owned VM control channel, not TOFU.
    key = Path('/etc/ssh/ssh_host_ed25519_key.pub').read_text().split()
    known = ssh_dir / 'known_hosts'
    known.write_text('127.0.0.1 ' + ' '.join(key[:2]) + '\n')
    known.chmod(0o600)
    os.chown(known, h.user.pw_uid, h.user.pw_gid)
    Path('/etc/ssh/sshd_config.d/00-vamos-integration.conf').write_text(
        'ListenAddress 127.0.0.1\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n'
        'PermitRootLogin prohibit-password\n')
    h.command(['systemctl', 'disable', '--now', 'ssh.socket'], user='root')
    h.command(['systemctl', 'enable', 'ssh'], user='root')
    h.command(['systemctl', 'restart', 'ssh'], user='root')
    # Match Ubuntu cloud VM /run mount flags in container-backed OrbStack guests.
    h.command(['mount', '-o', 'remount,noexec', '/run'], user='root')


def checks(h):
    h.log('Running Linux formatting, vet, race-enabled tests and builds')
    result = h.command(['gofmt', '-l', 'cmd', 'internal', 'integration/orbstack'])
    if result.stdout.strip():
        raise AssertionError('Go files are not formatted: ' + result.stdout.decode())
    h.command(['go', 'vet', './...'])
    coverage = h.artifacts / 'coverage.out'
    h.command(['go', 'test', '-race', '-coverprofile=' + str(coverage), './...'])
    h.command(['go', 'tool', 'cover', '-func=' + str(coverage)])
    h.command(['go', 'tool', 'cover', '-html=' + str(coverage), '-o', str(h.artifacts / 'coverage.html')])
    h.command(['go', 'run', './cmd/build'])
    import versioning
    executor = ROOT / 'bin' / ('vamos-linux-' + ('arm64' if platform.machine() == 'aarch64' else 'amd64'))
    versioning.run(h, executor)
    for example, tests in (('journal', '.'), ('ledger', 'tests')):
        h.command(['python3', '-m', 'unittest', 'discover', '-s', tests, '-v'], cwd=ROOT / 'examples' / example)
    h.command(['python3', '-m', 'unittest', 'discover', '-s', 'integration', '-p', 'test_*.py', '-v'])
    for manifest in sorted((ROOT / 'examples').rglob('vamos.yaml')):
        h.cli('validate', '-f', str(manifest))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--owner', required=True)
    parser.add_argument('--phase', choices=('prepare', 'verify-restart'), default='prepare')
    parser.add_argument('--scenario', choices=('all', 'firewall', 'waf-cache'), default='all')
    args = parser.parse_args()
    require_guest(args.owner)
    h = Harness()
    if args.phase == 'prepare':
        result = {'passed': False, 'started_at': time.time(), 'architecture': platform.machine(),
                  'os_release': Path('/etc/os-release').read_text()}
    else:
        result = json.loads(h.artifacts.joinpath('result.json').read_text())
    result['phase'] = args.phase
    try:
        import recovery
        if args.scenario == 'waf-cache':
            import waf_cache_scenarios
            if args.phase == 'prepare':
                bootstrap(h)
                h.command(['go', 'run', './cmd/build'])
                waf_cache_scenarios.prepare(h)
                result['prepare_passed'] = True
                result['restart_required'] = True
            else:
                waf_cache_scenarios.verify_restart(h)
                result['restart_required'] = False
                result['passed'] = True
            return
        if args.scenario == 'firewall':
            import firewall_scenarios
            if args.phase == 'prepare':
                bootstrap(h)
                h.command(['go', 'run', './cmd/build'])
                firewall_scenarios.prepare(h)
                result['prepare_passed'] = True
                result['restart_required'] = True
            else:
                firewall_scenarios.verify_restart(h)
                result['restart_required'] = False
                result['passed'] = True
            return
        if args.phase == 'prepare':
            bootstrap(h)
            checks(h)
            import scenarios
            scenarios.run(h)
            import cli_scenarios
            cli_scenarios.run(h)
            import runtime_scenarios
            runtime_scenarios.run(h)
            # Command gates are installed once for every checkpoint scenario.
            from checkpoints import install
            install(h)
            import daemon_restart
            daemon_restart.run(h)
            recovery.prepare(h, args.owner)
            result['prepare_passed'] = True
            result['restart_required'] = True
            h.log('Recovery checkpoints passed; prepared for coordinator-owned machine restart')
        else:
            recovery.verify_restart(h, args.owner)
            result['restart_required'] = False
            result['passed'] = True
            h.log('All Linux checks, integration scenarios, and restart recovery passed')
    except Exception:
        result['error'] = traceback.format_exc()
        h.artifacts.joinpath('failure.txt').write_text(result['error'])
        raise
    finally:
        h.diagnostics()
        result['finished_at'] = time.time()
        h.artifacts.joinpath('result.json').write_text(json.dumps(result, indent=2) + '\n')


if __name__ == '__main__':
    main()
