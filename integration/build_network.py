"""Disposable-guest proof of the actual isolated build service's network boundary."""
from contextlib import contextmanager
import json
import re
import secrets
import socket
from pathlib import Path

IMAGE = 'python:3.14-alpine@sha256:5862d971d1f0ae26b258cefa4e893556b1d3aba81bd2ff30888704ad378bab67'
STATE = Path('/var/lib/vamos/buildkit/state.json')
NATIVE_CONFIG = Path('/etc/systemd/system.conf.d/vamos-integration-builder.conf')


def require(value, message):
    if not value:
        raise AssertionError(message)


def configure_native(h):
    require(h.root == Path('/opt/vamos-test'), 'requires the owned guest harness')
    # This is a disposable-guest accommodation for nested overlay mount denial,
    # not a different builder/network implementation or relaxed machine isolation.
    path = NATIVE_CONFIG
    path.parent.mkdir(parents=True, exist_ok=True)
    expected = '[Manager]\nDefaultEnvironment=VAMOS_BUILDKIT_SNAPSHOTTER=native\n'
    try:
        with path.open('x') as stream:
            stream.write(expected)
    except FileExistsError:
        require(not path.is_symlink() and path.is_file() and path.read_text() == expected,
                'existing guest builder configuration differs')
    h.command(['systemctl', 'daemon-reexec'], user='root')
    h.command(['systemd-run', '--quiet', '--collect', '--wait', '--pipe',
               '--unit=vamos-integration-builder-env', '/bin/sh', '-c',
               'test "$VAMOS_BUILDKIT_SNAPSHOTTER" = native'], user='root')


def probe_source(targets):
    """A successful public request prevents total network failure passing the test."""
    return ('import socket, urllib.request\n'
            'with urllib.request.urlopen("https://pypi.org/simple/pip/", timeout=20) as r:\n'
            ' assert r.status == 200\n'
            f'for address in {targets!r}:\n'
            ' try:\n'
            '  connection = socket.create_connection(address, timeout=2)\n'
            ' except OSError:\n'
            '  print("blocked", address)\n'
            ' else:\n'
            '  connection.close()\n'
            '  raise RuntimeError("build reached protected listener: " + str(address))\n')


@contextmanager
def host_listener(h, host_name, host_port, server):
    require(h.root == Path('/opt/vamos-test'), 'requires the owned guest harness')
    host_id = None
    try:
        with socket.socket() as ready:
            ready.bind(('127.0.0.1', 0))
            ready.listen(1)
            ready.settimeout(20)
            notify = ('import socket; peer=socket.create_connection(("127.0.0.1",'
                      + str(ready.getsockname()[1]) + ')); peer.sendall(b"ready"); peer.close()')
            candidate = h.command(['docker', 'run', '-d', '--name', host_name, '--network', 'host',
                                    IMAGE, 'python', '-c', server.replace('PORT', str(host_port)).replace('NOTIFY', notify)],
                                   user='root').stdout.decode().strip()
            require(re.fullmatch(r'[a-f0-9]{64}', candidate), 'invalid listener identity')
            host_id = candidate
            peer, _ = ready.accept()
            with peer, peer.makefile('rb') as stream:
                peer.settimeout(20)
                require(stream.read(5) == b'ready', 'host fixture did not announce readiness')
        yield host_id
    finally:
        # Only the exact returned fixture identity, never a name or broad prune.
        if host_id is not None:
            h.command(['docker', 'rm', '-f', host_id], user='root')


def run(h):
    require(h.root == Path('/opt/vamos-test'), 'requires the owned guest harness')
    app = 'buildnet-' + secrets.token_hex(4)
    directory = h.work / app
    directory.mkdir()
    host_name = app + '-host-listener'
    host_port, app_port = 18888, 18889
    server = ('import http.server,signal,sys; '
              'signal.signal(signal.SIGTERM,lambda *_: sys.exit(0)); '
              'server=http.server.ThreadingHTTPServer(("0.0.0.0",PORT),'
              'http.server.SimpleHTTPRequestHandler); NOTIFY; server.serve_forever()')
    idle = ('import signal,sys,time; '
            'signal.signal(signal.SIGTERM,lambda *_: sys.exit(0)); time.sleep(3600)')
    manifest = {'app': app, 'services': {
        'target': {'image': IMAGE, 'command': ['python', '-c', server.replace('PORT', str(app_port)).replace('NOTIFY', 'pass')]},
        'probe': {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
                  'command': ['python', '-c', idle]},
    }}
    (directory / 'vamos.yaml').write_text(json.dumps(manifest))
    (directory / 'Dockerfile').write_text(f'FROM {IMAGE}\n')
    h.command(['chown', '-R', 'vamostest:vamostest', directory], user='root')
    h.cli('deploy', '--server', 'local', cwd=directory)
    state = json.loads(STATE.read_text())
    for key in ('ContainerID', 'NetworkID'):
        require(re.fullmatch(r'[a-f0-9]{64}', state[key]), 'invalid builder identity')
    require(state['Snapshotter'] == 'native', 'guest snapshotter setting was not captured')
    container_id = state['ContainerID']
    target = h.status(app)['services']['target']['container']
    target_info = json.loads(h.command(['docker', 'inspect', target], user='root').stdout)[0]
    target_ip = target_info['NetworkSettings']['Networks']['vamos-' + app]['IPAddress']
    network = json.loads(h.command(['docker', 'network', 'inspect', state['NetworkID']], user='root').stdout)[0]
    gateway = network['IPAM']['Config'][0]['Gateway']
    addresses = json.loads(h.command(['ip', '-j', 'address', 'show'], user='root').stdout)
    host_ips = [a['local'] for interface in addresses for a in interface.get('addr_info', [])
                if a['family'] == 'inet' and a.get('scope') == 'global']
    require(host_ips, 'missing host address coverage')
    with host_listener(h, host_name, host_port, server) as host_id:
        # Destination-side positive controls: these listeners really accept HTTP.
        # TCP-only RUN probes do not add HTTP log entries; ADD probes must not either.
        h.command(['curl', '--fail', '--silent', '--max-time', '10', '--noproxy', '*',
                   f'http://{gateway}:{host_port}/'], user='root')
        h.command(['curl', '--fail', '--silent', '--max-time', '10', '--noproxy', '*',
                   f'http://{target_ip}:{app_port}/'], user='root')
        before = {cid: h.command(['docker', 'logs', cid], user='root').stderr
                  for cid in (host_id, target)}
        targets = [('127.0.0.1', host_port), ('::1', host_port), (gateway, host_port),
                   (target_ip, app_port), *[(ip, host_port) for ip in host_ips]]
        (directory / 'probe.py').write_text(probe_source(targets))
        (directory / 'Dockerfile').write_text(f'FROM {IMAGE}\nCOPY probe.py /probe.py\nRUN python /probe.py\n')
        h.command(['chown', '-R', 'vamostest:vamostest', directory], user='root')
        h.cli('deploy', '--server', 'local', '--service', 'probe', cwd=directory)
        builder = json.loads(h.command(['docker', 'inspect', container_id], user='root').stdout)[0]
        require(not builder['State']['Running'], 'builder left running after deployment')
        require(builder['HostConfig']['RestartPolicy']['Name'] == 'no', 'builder can restart without guard verification')
        # Direct protocol probes deliberately grant client-side entitlements: the
        # daemon must still refuse them. No app journals are edited or bypassed.
        env = ['env', '-u', 'DOCKER_CONTEXT', '-u', 'DOCKER_TLS', '-u', 'DOCKER_TLS_VERIFY',
               '-u', 'DOCKER_CERT_PATH', '-u', 'BUILDKIT_HOST', '-u', 'BUILDX_BUILDER',
               'DOCKER_HOST=unix:///var/run/docker.sock', 'BUILDX_CONFIG=/var/lib/vamos/buildkit/buildx']
        h.command(['docker', 'start', container_id], user='root')
        try:
            h.command([*env, 'docker', 'buildx', 'inspect', '--bootstrap', state['Builder']], user='root')
            for entitlement in ('network.host', 'security.insecure'):
                result = h.command([*env, 'docker', 'buildx', 'build', '--builder', state['Builder'],
                                    '--allow', entitlement, '--progress', 'plain', '-'],
                                   input=b'FROM scratch\n', user='root', check=False)
                require(result.returncode != 0 and b'not allowed by build daemon' in result.stderr,
                        'daemon accepted or did not validate insecure entitlement')
            for url in (f'http://{gateway}:{host_port}/', f'http://{target_ip}:{app_port}/',
                        f'http://{target_ip}:{app_port}/repository.git'):
                source = f'FROM scratch\nADD {url} /probe\n'.encode()
                result = h.command(['timeout', '12', *env, 'docker', 'buildx', 'build',
                                    '--builder', state['Builder'], '--no-cache', '--progress', 'plain', '-'],
                                   input=source, user='root', check=False, timeout=30)
                output = result.stdout + result.stderr
                require(result.returncode != 0 and url.encode() in output,
                        'source-fetch probe did not reach the intended build step')
            for cid, logs in before.items():
                require(h.command(['docker', 'logs', cid], user='root').stderr == logs,
                        'protected listener received a build source request')
        finally:
            h.command(['docker', 'stop', '--time', '5', container_id], user='root')
        h.cli('destroy', '--server', 'local', '--yes', cwd=directory)
        (h.artifacts / 'build-network.json').write_text(json.dumps({
            'passed': True, 'app': app, 'run_targets': targets,
            'source_fetches': ['host HTTP ADD', 'application HTTP ADD', 'application Git ADD'],
            'entitlements_denied': ['network.host', 'security.insecure'],
            'public_https': 'https://pypi.org/simple/pip/',
        }, indent=2))
        h.log('build network passed: dependency egress, host/app isolation, source fetches and denied entitlements')
