"""Late, guest-only live repository acceptance. Never invoke on a workstation."""
import argparse
import hashlib
import platform
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from linux_suite import Harness, require_guest
from scenarios import require

# Docker Hub public metadata, retrieved 2026-11-13; multiarch amd64/arm64 index.
IMAGE = 'rustfs/rustfs@sha256:b81d16a90ee25bd9511280127ca1471788c6e874cf0f0da759645f46b2dc1d74'
RESTIC = Path('/usr/local/lib/vamos/tools/restic/0.19.1/restic')


def private(path):
    require(path.stat().st_mode & 0o777 == 0o600, 'private file mode: ' + str(path))


def clean_output(output, credentials):
    for key in ('access_key_id', 'secret_access_key', 'password'):
        value = credentials.get(key)
        require(not value or value.encode() not in output, 'credential exposed in command output')


def fixture_container_command(container, envfile, network):
    return ['docker', 'run', '-d', '--name', container, '--label', 'vamos.integration.repository=true', '--env-file', envfile, '--network', network, '-p', '127.0.0.1:19000:9000', IMAGE, '/data']


def fixture_proxy_config(cert, key):
    return {'admin': {'disabled': True}, 'apps': {
        'tls': {'certificates': {'load_files': [{'certificate': str(cert), 'key': str(key)}]}},
        'http': {'servers': {'fixture': {
            'listen': ['127.0.0.1:19001'],
            # Static TLS only: automatic redirects would steal Vamos's port 80.
            'automatic_https': {'disable': True},
            'tls_connection_policies': [{}],
            'routes': [{'handle': [{'handler': 'reverse_proxy', 'upstreams': [{'dial': '127.0.0.1:19000'}]}]}],
        }}}}}


def cleanup_fixture(h, proc, cid, network=None):
    try:
        if proc is not None:
            try:
                proc.terminate()
            except ProcessLookupError:
                pass
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=15)
    finally:
        # Proxy cleanup must never suppress exact-container cleanup.
        try:
            if cid is not None and re.fullmatch('[0-9a-f]{64}', cid):
                h.command(['docker', 'rm', '-f', '-v', cid], user='root')
        finally:
            if network is not None and re.fullmatch('[0-9a-f]{64}', network):
                h.command(['docker', 'network', 'rm', network], user='root')


def run(h):
    work = h.work / 'repository-acceptance'
    work.mkdir(mode=0o700)  # Fail closed on reruns; never overwrite fixture state.
    os.chown(work, h.user.pw_uid, h.user.pw_gid)
    credentials = {'access_key_id': 'vamos' + secrets.token_hex(10), 'secret_access_key': secrets.token_hex(32)}
    envfile = work / 'fixture.env'
    envfile.write_text('RUSTFS_ACCESS_KEY=' + credentials['access_key_id'] + '\nRUSTFS_SECRET_KEY=' + credentials['secret_access_key'] + '\nRUSTFS_CONSOLE_ENABLE=false\n')
    envfile.chmod(0o600)
    cert, key = work / 'cert.pem', work / 'key.pem'
    h.command(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=vamos-s3-fixture', '-addext', 'subjectAltName=IP:127.0.0.1', '-keyout', key, '-out', cert], user='root')
    key.chmod(0o600)
    credentials['ca_certificate'] = cert.read_text()
    proxy = work / 'caddy.json'
    proxy.write_text(json.dumps(fixture_proxy_config(cert, key)))
    helper = work / 's3-admin'
    h.command(['go', 'build', '-o', helper, './integration/repository'])
    container = 'vamos-repository-' + secrets.token_hex(8)
    cid = None
    network = None
    proc = None
    try:
        # Vamos deliberately disables Docker's default bridge. This fixture owns
        # a separate explicit network; never adopt an application network.
        network = h.command(['docker', 'network', 'create', '--label', 'vamos.integration.repository=true', container], user='root').stdout.decode().strip()
        require(re.fullmatch('[0-9a-f]{64}', network), 'invalid fixture network ID')
        cid = h.command(fixture_container_command(container, envfile, network), user='root').stdout.decode().strip()
        require(re.fullmatch('[0-9a-f]{64}', cid), 'invalid fixture container ID')
        proc = subprocess.Popen(['caddy', 'run', '--config', str(proxy)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=h.env)
        context = ssl.create_default_context(cafile=str(cert))
        deadline = time.monotonic() + 90
        while True:
            try:
                with urllib.request.urlopen('https://127.0.0.1:19001/health', context=context, timeout=2) as response:
                    if response.status == 200:
                        break
            except Exception:
                if time.monotonic() >= deadline:
                    raise AssertionError('fixture HTTPS readiness deadline') from None
                time.sleep(0.2)
        def admin(action):
            return h.command([helper, action], input=json.dumps(credentials).encode(), user='root').stdout
        admin('create')  # The backend must never create the bucket.
        require(json.loads(admin('list')) == {}, 'fixture bucket not empty')
        require(not RESTIC.exists(), 'first-install test requires no preinstalled pinned restic')
        require(Path('/run').stat().st_dev != Path('/usr/local').stat().st_dev, 'fixture requires cross-filesystem runtime scratch')
        before_tmp = set(Path('/run').rglob('restic-*'))
        outputs = []
        def cli(action, *args, creds=None, success=True):
            result = h.command([h.root / 'bin/vamos', 'backup', 'repository', action, '--server', 'local', '--json', *args], input=json.dumps(creds).encode() if creds else None, check=False)
            outputs.append(result.stdout + result.stderr)
            clean_output(outputs[-1], credentials)
            if creds:
                clean_output(outputs[-1], creds)
            require((result.returncode == 0) == success, 'repository ' + action + ' unexpected exit status')
            return json.loads(result.stdout) if success else None
        def add(name, mode, prefix, creds, identity=None, success=True):
            args = ['--mode', mode, '--endpoint', 'https://127.0.0.1:19001', '--bucket', 'vamos-acceptance', '--prefix', prefix, '--region', 'us-east-1', '--lookup', 'path', '--credentials-stdin', '--recovery-file', str(work / (name + '.kit.json')), '--yes']
            if identity:
                args += ['--repository-id', identity]
            return cli('add', *args, name, creds=creds, success=success)
        created = add('fixture-new', 'new', 'base', credentials)['repository']
        kitpath = work / 'fixture-new.kit.json'
        private(kitpath)
        kit = json.loads(kitpath.read_text())
        identity = kit['repository_id']
        require(re.fullmatch('[0-9a-f]{64}', identity) and created['repository_id'] == identity, 'full config identity missing')
        require(kit['format'] == 2 and kit['restic_version'] == '0.19.1', 'unexpected recovery format/tool')
        credentials = kit['credentials']
        for output in outputs:
            clean_output(output, credentials)
        version = h.command([RESTIC, 'version'], user='root').stdout
        require(version.startswith(b'restic 0.19.1 '), 'not pinned real restic')
        pins = {'x86_64': '20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639',
                'aarch64': '2fb45ac6f9071b6f20eb883953a188f9e7c7cb6bbe43c67a2e47ada4e85ee7f0'}
        require(hashlib.sha256(RESTIC.read_bytes()).hexdigest() == pins[platform.machine()], 'installed restic checksum differs from release pin')
        require(RESTIC.stat().st_uid == 0 and RESTIC.stat().st_mode & 0o777 == 0o755, 'installed restic ownership/mode unsafe')
        baseline = admin('list')
        objects = json.loads(baseline)
        prefix = kit['config']['prefix']
        require(prefix + '/config' in objects and any(k.startswith(prefix + '/keys/') for k in objects), 'real restic config/key missing')
        def unchanged():
            require(admin('list') == baseline, 'read-only operation changed S3 objects (including locks)')
            require(set(Path('/run').rglob('restic-*')) == before_tmp, 'private restic scratch leaked')
        observed = h.command([helper, 'probe', prefix], input=json.dumps(credentials).encode(), user='root')
        require(json.loads(observed.stdout)['id'] == identity, 'independent real config probe identity mismatch')
        unchanged()
        missing = h.command([helper, 'probe', 'never-initialize'], input=json.dumps(credentials).encode(), user='root', check=False)
        require(missing.returncode != 0, 'missing-prefix read-only probe succeeded')
        clean_output(missing.stdout + missing.stderr, credentials)
        unchanged()
        for _ in range(2):
            verified = cli('verify', 'fixture-new')['repository']
            require(verified['repository_id'] == identity, 'verify identity changed')
            unchanged()
        cli('list')
        cli('show', 'fixture-new')
        cli('resume', 'fixture-new', created['operation'])
        unchanged()
        add('fixture-connect', 'connect', prefix, credentials, identity)
        unchanged()
        wrong = dict(credentials, password=secrets.token_hex(32))
        add('fixture-password', 'connect', prefix, wrong, identity, success=False)
        unchanged()
        add('fixture-identity', 'connect', prefix, credentials, '0' * 64, success=False)
        unchanged()
        add('fixture-missing', 'connect', 'never-initialize', credentials, identity, success=False)
        unchanged()
        for directory in (Path('/var/lib/vamos/backups'), Path('/var/lib/vamos/backups/repositories')):
            require(directory.stat().st_mode & 0o777 == 0o700 and directory.stat().st_uid == 0, 'retained credential directory is not root-private')
        for name in ('fixture-new', 'fixture-connect'):
            record = Path('/var/lib/vamos/backups/repositories') / (name + '.json')
            private(record)
            require(record.stat().st_uid == 0, 'repository record not root-owned')
            retained = json.loads(record.read_text())
            require(retained['credentials'] == credentials, 'retained credentials differ from recovery kit')
            denied = h.command(['test', '-r', record], check=False)
            require(denied.returncode != 0, 'CLI user can read retained host credentials')
        for artifact in h.artifacts.iterdir():
            if artifact.suffix in ('.stdout', '.stderr') or artifact.name.endswith('.command.json'):
                clean_output(artifact.read_bytes(), credentials)
        for output in outputs:
            clean_output(output, credentials)
        (h.artifacts / 'repository-summary.json').write_text(json.dumps({'passed': True, 'image': IMAGE, 'repository_id': identity, 'objects': objects, 'checks': ['first cross-filesystem install', 'create', 'verify twice', 'list/show/resume', 'connect', 'wrong password', 'wrong ID', 'missing prefix never initialized', 'private kit/record/scratch/output']}))
        h.log('Live S3 repository acceptance passed; synthetic records retained until owned VM deletion')
        from repository.plan_acceptance import run as run_plan_acceptance
        run_plan_acceptance(h, identity, credentials)
        unchanged()
    finally:
        cleanup_fixture(h, proc, cid, network)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--owner', required=True)
    args = parser.parse_args()
    require_guest(args.owner)
    run(Harness())


if __name__ == '__main__':
    main()
