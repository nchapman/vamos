"""Exercise a distinct deploy-only runner against the sudo scenario's live app."""
import json
import os
from pathlib import Path


def exercise(h, app, source):
    ssh = Path(h.user.pw_dir) / '.ssh'
    key = ssh / 'ledger_ci'
    h.command(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', key])
    # Only this alias uses the runner key; the administrator keeps the bootstrap identity.
    with (ssh / 'config').open('a') as config:
        config.write(f'\nHost scoped-ci\n  HostName 127.0.0.1\n  User vamos\n'
                     f'  IdentitiesOnly yes\n  IdentityFile {key}\n')
    os.chown(ssh / 'config', h.user.pw_uid, h.user.pw_gid)
    h.cli('server', 'principal', 'grant', '--yes', '--app', app, '--key', str(key) + '.pub', 'ci')
    entries = json.loads(h.cli('server', 'principal', 'list', '--json').stdout)
    if [(e['name'], e['app']) for e in entries] != [('ci', app)]:
        raise AssertionError(f'unexpected grants: {entries!r}')

    checkout = h.work / 'ci-checkout'
    checkout.mkdir()
    manifest = json.loads((source / 'vamos.yaml').read_text())
    (checkout / 'vamos.yaml').write_text(json.dumps(manifest))
    h.command(['chown', '-R', 'vamostest:vamostest', checkout], user='root')
    owner = h.env
    runner = dict(owner, XDG_CONFIG_HOME=str(h.work / 'ci-registry'))
    try:
        h.env = runner
        h.cli('server', 'connect', '--bridge', '--domain', 'vamos.localhost', 'local', 'scoped-ci')
        h.cli('service', 'bind', 'web', '--server', 'local', cwd=checkout)
        h.cli('deploy', '--service', 'web', '--server', 'local', cwd=checkout)
        manifest['app'] = app + '-sibling'
        (checkout / 'sibling.yaml').write_text(json.dumps(manifest))
        h.cli('deploy', '-f', 'sibling.yaml', '--server', 'local', cwd=checkout,
              expect_error='not authorized')
        h.env = owner
        h.cli('server', 'principal', 'revoke', '--yes', 'ci')
        h.env = runner
        result = h.command([h.root / 'bin/vamos', 'status', '--server', 'local', '--app', app], check=False)
        if result.returncode == 0:
            raise AssertionError('revoked runner key retained app access')
    finally:
        h.env = owner
    h.cli('status', '--server', 'local', '--app', app)
