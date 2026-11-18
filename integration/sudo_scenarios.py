"""Sudo-only administration with root SSH disabled in a runner-owned guest."""
import json
import os
import secrets
from pathlib import Path

from firewall_scenarios import IMAGE


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def account(h, name, passwordless):
    import pwd  # Guest-only; keep module importable on non-Unix test clients.
    h.command(['useradd', '--create-home', '--shell', '/bin/bash', name], user='root')
    user = pwd.getpwnam(name)
    ssh = Path(user.pw_dir) / '.ssh'
    ssh.mkdir(mode=0o700)
    key = ssh / 'authorized_keys'
    key.write_text((Path(h.user.pw_dir) / '.ssh/id_ed25519.pub').read_text())
    key.chmod(0o600)
    for p in (ssh, key):
        os.chown(p, user.pw_uid, user.pw_gid)
    rule = Path('/etc/sudoers.d') / ('vamos-integration-' + name)
    rule.write_text(f'{name} ALL=(root) ' + ('NOPASSWD: ' if passwordless else 'PASSWD: ') + 'ALL\n')
    rule.chmod(0o440)
    h.command(['visudo', '-cf', rule], user='root')


def prepare(h):
    from build_network import configure_native
    from recovery import init_identity
    configure_native(h)
    h.command(['apt-get', 'install', '-y', '--no-install-recommends', 'sudo'], user='root')
    account(h, 'vamosadmin', True)
    account(h, 'vamosdenied', False)
    config = Path('/etc/ssh/sshd_config.d/00-vamos-integration.conf')
    config.write_text(config.read_text().replace('PermitRootLogin prohibit-password', 'PermitRootLogin no'))
    h.command(['systemctl', 'restart', 'ssh'], user='root')
    refused = h.command(['ssh', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
                         'root@127.0.0.1', 'true'], check=False, timeout=20)
    require(refused.returncode != 0, 'root SSH must be disabled for sudo coverage')
    identity = h.command(['ssh', '-o', 'BatchMode=yes', 'vamosadmin@127.0.0.1', 'id -u']).stdout.strip()
    require(identity != b'0', 'administrative SSH account is unexpectedly root')
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost',
          'denied', 'vamosdenied@127.0.0.1', expect_error='passwordless sudo')
    require(not Path('/var/lib/vamos/host.json').exists(), 'denied sudo setup mutated host state')
    h.cli('server', 'inspect', 'vamosadmin@127.0.0.1')
    h.cli('server', 'add', '--yes', '--firewall=true', '--domain', 'vamos.localhost',
          'local', 'vamosadmin@127.0.0.1')
    firewall = json.loads(h.cli('server', 'firewall', 'status', '--json', 'local').stdout)
    require(firewall['enabled'] and not firewall.get('pending'), 'sudo firewall not confirmed')
    app = 'sudo-' + secrets.token_hex(4)
    directory = h.work / app
    directory.mkdir()
    (directory / 'vamos.yaml').write_text(json.dumps({'app': app, 'services': {
        'web': {'image': IMAGE, 'command': ['python', '-m', 'http.server', '8080'], 'http': {'port': 8080}}
    }}))
    h.command(['chown', '-R', 'vamostest:vamostest', directory], user='root')
    h.cli('deploy', '--server', 'local', cwd=directory)
    require(h.https(f'{app}.vamos.localhost'), 'app not served')
    result = h.cli('exec', '--server', 'local', '--app', app, '--', 'python', '-c', 'print("agent-exec-ok")')
    require(b'agent-exec-ok' in result.stdout, 'restricted coordinator session failed')
    from principal_scenario import exercise
    exercise(h, app, directory)
    h.command(['go', 'run', './integration/sudo_transport', app])
    h.cli('server', 'upgrade', '--yes', 'local')
    (h.artifacts / 'sudo-checkpoint.json').write_text(json.dumps({'app': app, 'init': init_identity()}))


def verify_restart(h):
    from recovery import init_identity
    state = json.loads((h.artifacts / 'sudo-checkpoint.json').read_text())
    require(init_identity() != state['init'], 'machine was not restarted')
    h.command(['systemctl', 'is-system-running', '--wait'], user='root', check=False, timeout=120)
    h.cli('server', 'status', 'local')
    h.cli('status', '--server', 'local', '--app', state['app'])
    h.command(['go', 'run', './integration/sudo_transport', state['app']])
    h.cli('server', 'firewall', 'disable', '--yes', 'local')
    h.cli('server', 'firewall', 'enable', '--yes', 'local')
    # Reproduce a crash after rule removal and the first artifact unlink.
    # This is confined to the owned disposable guest and uses the real script.
    prefix = ['ssh', '-o', 'BatchMode=yes', 'vamosadmin@127.0.0.1', 'sudo', '-n', '--']
    h.command(prefix + ['/bin/sh', '/usr/local/lib/vamos/firewall.sh', 'remove'])
    h.command(prefix + ['systemctl', 'disable', '--now', 'vamos-firewall-rollback.timer'])
    h.command(prefix + ['systemctl', 'stop', 'vamos-firewall-rollback.service'])
    h.command(prefix + ['systemctl', 'disable', 'vamos-firewall.service'])
    h.command(prefix + ['rm', '/usr/local/lib/vamos/firewall.sh'])
    h.cli('server', 'firewall', 'disable', '--yes', 'local')
    running = h.command(prefix + ['docker', 'inspect', '--format', '{{.State.Running}}', 'vamos-caddy']).stdout.strip()
    require(running == b'true', 'resumed firewall cleanup stopped Docker/Caddy')
    h.cli('destroy', '--server', 'local', '--app', state['app'], '--yes')
    # Pulled images are not adopted as owned build artifacts. Remove only our
    # pinned fixture image explicitly before asking lifecycle to purge the host.
    h.command(['ssh', '-o', 'BatchMode=yes', 'vamosadmin@127.0.0.1',
               'sudo', '-n', '--', 'docker', 'image', 'rm', IMAGE])
    h.cli('server', 'teardown', '--yes', '--purge', 'local')
    require(json.loads(Path('/var/lib/vamos/host.json').read_text())['phase'] == 'removed', 'sudo teardown incomplete')
    (h.artifacts / 'sudo-summary.json').write_text(json.dumps({
        'passed': True,
        'verified': ['root SSH disabled', 'password-required sudo rejected before setup',
                     'passwordless sudo setup and firewall fresh-SSH confirmation',
                     'scoped runner grant, bind, deploy, sibling refusal, revoke and owner continuity',
                     'restricted coordinator deploy and exec', 'uploaded administrative status and binary-stdin session',
                     'sudo upgrade with app and firewall enabled', 'machine restart',
                     'sudo firewall disable/re-enable', 'interrupted firewall cleanup retry preserves Docker', 'sudo teardown'],
    }, indent=2) + '\n')
