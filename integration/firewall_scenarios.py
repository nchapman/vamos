"""Focused managed-firewall acceptance in the runner-owned Ubuntu guest.

No policy/config repair: unsupported baselines are failures. Probes enter through
an isolated network namespace, not OUTPUT/loopback. The coordinator restarts the
machine between prepare and verify_restart. Rollback uses the real 300s timer.
"""
from contextlib import contextmanager
import ipaddress
import json
from pathlib import Path
import re
import secrets
import time

from build_network import IMAGE
from scenarios import require

GUEST = Path('/opt/vamos-test')
UNITS = Path('/etc/systemd/system')
HOST = '198.18.0.1'
PEER = '198.18.0.2'
HOST6 = 'fd7a:5641:4d4f:5300::1'
PEER6 = 'fd7a:5641:4d4f:5300::2'
HOST_PORT = 18990
TCP_PORT = 18991
UDP_PORT = 18992
CHECKPOINT = 'firewall-checkpoint.json'


def owned(h):
    require(h.root == GUEST, 'requires the owned guest harness')


def root(h, *args, **kwargs):
    return h.command(list(args), user='root', **kwargs)


def status(h, enabled, pending=False):
    value = json.loads(h.cli('server', 'firewall', 'status', '--json', 'local').stdout)
    require(value['enabled'] is enabled and value['pending'] is pending and
            value['degraded'] is False, f'unexpected firewall status: {value}')
    return value


def probe_source(address, port, protocol='tcp', allowed=True):
    # Only a timeout counts as filtering. Connection refused/unreachable and
    # namespace/tool errors must never masquerade as a passing deny assertion.
    return f'''import socket
s = socket.socket(socket.{'AF_INET6' if ':' in address else 'AF_INET'}, socket.SOCK_{'DGRAM' if protocol == 'udp' else 'STREAM'})
s.settimeout(3)
try:
    s.connect(({address!r}, {port}))
    {'s.send(b"firewall-probe"); assert s.recv(100) == b"firewall-probe"' if protocol == 'udp' else 'pass'}
except TimeoutError:
    assert not {allowed!r}, "allowed packet path timed out"
else:
    assert {allowed!r}, "protected packet path was reachable"
finally:
    s.close()
'''


class Peer:
    def __init__(self, h, name):
        self.h, self.name = h, name

    def command(self, *args, **kwargs):
        return root(self.h, 'ip', 'netns', 'exec', self.name, *args, **kwargs)

    def probe(self, address, port, protocol='tcp', allowed=True):
        self.command('python3', '-c', probe_source(address, port, protocol, allowed))

    def ssh(self):
        # No multiplexing: this must establish a new TCP/SSH connection through
        # INPUT, using the harness's pinned host key and guest-only test key.
        home = self.h.user.pw_dir
        return self.command('ssh', '-o', 'BatchMode=yes', '-o', 'ControlMaster=no',
                            '-o', 'ControlPath=none', '-o', 'StrictHostKeyChecking=yes',
                            '-o', 'HostKeyAlias=127.0.0.1', '-o', 'ConnectTimeout=10',
                            '-o', f'UserKnownHostsFile={home}/.ssh/known_hosts',
                            '-i', f'{home}/.ssh/id_ed25519', '-p', '22222', 'root@' + HOST, 'true')


@contextmanager
def peer(h):
    owned(h)
    # Fail on overlap rather than changing the guest's routes or firewall.
    routes = json.loads(root(h, 'ip', '-j', '-4', 'route', 'show', 'table', 'all').stdout)
    subnet = ipaddress.ip_network('198.18.0.0/30')
    for route in routes:
        dst = route.get('dst', 'default')
        if dst != 'default':
            require(not subnet.overlaps(ipaddress.ip_network(dst, strict=False)),
                    f'probe subnet overlaps existing route {dst}')
    tag = secrets.token_hex(4)
    ns, link, other = 'fw-' + tag, 'fwv' + tag, 'fwp' + tag
    created_ns = created_link = False
    ssh_config = Path('/etc/ssh/sshd_config.d/00-vamos-integration.conf')
    original = ssh_config.read_text()
    try:
        root(h, 'ip', 'netns', 'add', ns)
        created_ns = True
        root(h, 'ip', 'link', 'add', link, 'type', 'veth', 'peer', 'name', other)
        created_link = True
        root(h, 'ip', 'link', 'set', other, 'netns', ns)
        root(h, 'ip', 'address', 'add', HOST + '/30', 'dev', link)
        root(h, 'ip', '-6', 'address', 'add', HOST6 + '/64', 'dev', link, 'nodad')
        root(h, 'ip', 'link', 'set', link, 'up')
        p = Peer(h, ns)
        p.command('ip', 'link', 'set', 'lo', 'up')
        p.command('ip', 'address', 'add', PEER + '/30', 'dev', other)
        p.command('ip', '-6', 'address', 'add', PEER6 + '/64', 'dev', other, 'nodad')
        p.command('ip', 'link', 'set', other, 'up')
        p.command('ip', 'route', 'add', 'default', 'via', HOST)
        ssh_config.write_text(original + f'\nPort 22\nPort 22222\nListenAddress {HOST}\nListenAddress {HOST6}\n')
        root(h, 'systemctl', 'restart', 'ssh')
        yield p
    finally:
        ssh_config.write_text(original)
        root(h, 'systemctl', 'restart', 'ssh')
        if created_link:
            root(h, 'ip', 'link', 'delete', link)
        if created_ns:
            root(h, 'ip', 'netns', 'delete', ns)


def wait_http(h, url):
    # Readiness only; policy assertions themselves are never retried.
    deadline = time.monotonic() + 30
    while True:
        result = root(h, 'curl', '--fail', '--silent', '--noproxy', '*',
                      '--max-time', '2', url, check=False)
        if result.returncode == 0:
            return
        require(time.monotonic() < deadline, f'listener did not become ready: {url}')
        time.sleep(0.25)


def app_info(h, app):
    info = h.status(app)['services']['web']
    cid = info['container']
    obj = json.loads(root(h, 'docker', 'inspect', cid).stdout)[0]
    binding = [b for b in obj['NetworkSettings']['Ports']['8080/tcp'] if b['HostIp'] == '127.0.0.1']
    require(len(binding) == 1, 'HTTP upstream binding is not loopback-only')
    return cid, obj['NetworkSettings']['Networks']['vamos-' + app]['IPAddress'], int(binding[0]['HostPort'])


def checks(h, p, state):
    status(h, True)
    p.ssh()
    # Positive destination controls rule out a dead listener as a deny result.
    wait_http(h, f'http://127.0.0.1:{HOST_PORT}/')
    p.probe(HOST, HOST_PORT, allowed=False)
    p.probe(HOST6, HOST_PORT, allowed=False)
    p.probe(HOST6, 22222)
    p.probe(HOST6, 443)
    cid, address, upstream = app_info(h, state['app'])
    wait_http(h, f'http://127.0.0.1:{upstream}/')
    wait_http(h, f'http://{address}:8080/')
    p.probe(HOST, upstream, allowed=False)
    p.probe(address, 8080, allowed=False)
    p.probe(HOST, TCP_PORT)
    p.probe(HOST, UDP_PORT, protocol='udp')
    host = state['app'] + '.vamos.localhost'
    # HTTP redirects and real HTTPS responses through host-network Caddy.
    response = p.command('curl', '--silent', '--show-error', '--noproxy', '*', '--max-time', '10',
                         '--resolve', f'{host}:80:{HOST}', '-o', '/dev/null', '-w', '%{http_code}',
                         f'http://{host}/').stdout
    require(response in (b'301', b'302', b'307', b'308'), f'Caddy HTTP redirect missing: {response}')
    from linux_suite import CA
    response = p.command('curl', '--fail', '--silent', '--show-error', '--noproxy', '*',
                         '--max-time', '10', '--cacert', str(CA), '--resolve', f'{host}:443:{HOST}',
                         f'https://{host}/').stdout
    require(b'Directory listing' in response, 'external Caddy HTTPS did not reach app')


def prepare(h):
    owned(h)
    from build_network import configure_native
    from recovery import init_identity
    configure_native(h)
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost',
          'local', 'root@127.0.0.1')
    status(h, False)
    root(h, 'docker', 'pull', IMAGE)
    state = {'app': 'fw-' + secrets.token_hex(4), 'init': init_identity()}
    state['host_unit'] = 'vamos-integration-' + state['app'] + '.service'
    unit = UNITS / state['host_unit']
    require(not unit.exists(), 'listener unit already exists')
    unit.write_text('[Unit]\nDescription=Owned firewall acceptance listener\n'
                    '[Service]\nExecStart=/usr/bin/python3 -m http.server --bind :: ' + str(HOST_PORT) + '\n'
                    'Restart=always\n[Install]\nWantedBy=multi-user.target\n')
    root(h, 'systemctl', 'daemon-reload')
    root(h, 'systemctl', 'enable', '--now', state['host_unit'])
    with peer(h) as p:
        wait_http(h, f'http://127.0.0.1:{HOST_PORT}/')
        p.probe(HOST, HOST_PORT)  # baseline proves external fixture works
        p.probe(HOST6, HOST_PORT)
        p.probe(HOST6, 22)
        p.ssh()
        h.cli('server', 'firewall', 'enable', '--yes', 'local')
        status(h, True)
        p.ssh()
        p.probe(HOST, HOST_PORT, allowed=False)
        # CRITICAL: both the application bridge and build network first appear
        # after enable. A snapshot of existing bridges cannot pass these tests.
        networks = root(h, 'docker', 'network', 'ls', '--format', '{{.Name}}').stdout.splitlines()
        require(('vamos-' + state['app']).encode() not in networks, 'app bridge predates enable')
        directory = h.work / state['app']
        directory.mkdir()
        (directory / 'Dockerfile').write_text(
            f'FROM {IMAGE}\nRUN python -c "import urllib.request; '
            'assert urllib.request.urlopen(\'https://pypi.org/simple/pip/\', timeout=20).status == 200"\n')
        echo = ('import socket\n'
                's=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",8081))\n'
                'while True:\n data,peer=s.recvfrom(1024); s.sendto(data,peer)\n')
        (directory / 'vamos.yaml').write_text(json.dumps({'app': state['app'], 'services': {
            'web': {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
                    'command': ['python', '-m', 'http.server', '8080'], 'http': {'port': 8080},
                    'ports': [f'{TCP_PORT}:8080/tcp']},
            'echo': {'image': IMAGE, 'command': ['python', '-c', echo],
                     'ports': [f'{UDP_PORT}:8081/udp']}}}))
        root(h, 'chown', '-R', 'vamostest:vamostest', directory)
        h.cli('deploy', '--server', 'local', cwd=directory)
        # Both direct mappings are owned service declarations, not unmanaged fixtures.
        wait_http(h, f'http://127.0.0.1:{TCP_PORT}/')
        checks(h, p, state)
        root(h, 'systemctl', 'restart', 'docker', timeout=120)
        checks(h, p, state)
    (h.artifacts / CHECKPOINT).write_text(json.dumps(state))
    h.log('Firewall packet paths and Docker restart passed; ready for machine restart')


def unrelated_snapshot(h, chain):
    # Counters are intentionally excluded; unrelated rule content/policy is not.
    return {tool: root(h, tool, '-S', chain).stdout for tool in ('iptables', 'ip6tables')}


def unowned_rules(snapshot):
    owned = re.compile(r'^(?::VAMOS-FW-(?:IN|FWD) |'
                       r'-A VAMOS-FW-(?:IN|FWD) |'
                       r'-A (?:INPUT|FORWARD|DOCKER-USER) -j VAMOS-FW-(?:IN|FWD)$)')
    return '\n'.join(re.sub(r'\[\d+:\d+\]', '[0:0]', line)
                     for line in snapshot.decode().splitlines()
                     if not line.startswith('#') and not owned.match(line))


def rules_snapshot(h):
    return {tool: unowned_rules(root(h, tool).stdout)
            for tool in ('iptables-save', 'ip6tables-save')}


def rollback(h, p):
    # Bypass only the CLI auto-confirm, not the production executor/SSH path.
    # End the SSH session after enable, never send firewall-confirm, and wait for
    # the independent production systemd timer. Do not shorten its deadline.
    request = {'version': 1, 'operation': 'firewall-enable', 'confirm': True,
               'config': {'name': 'local', 'domain': 'vamos.localhost'}}
    response = h.command(['ssh', '-o', 'BatchMode=yes', '-o', 'ControlMaster=no',
                          '-o', 'ControlPath=none', '-o', 'StrictHostKeyChecking=yes',
                          'root@127.0.0.1', '/usr/local/lib/vamos/executor', 'internal-executor'],
                         input=json.dumps(request).encode())
    value = json.loads(response.stdout)['firewall']
    require(value['enabled'] and value['pending'] and value.get('token'), 'enable did not arm rollback')
    status(h, True, pending=True)
    p.probe(HOST, HOST_PORT, allowed=False)
    deadline = time.monotonic() + 340
    while Path('/var/lib/vamos/firewall.json').exists():
        require(time.monotonic() < deadline, 'unconfirmed firewall failed to roll back within 340s')
        time.sleep(1)
    status(h, False)
    p.probe(HOST, HOST_PORT)
    p.ssh()
    h.cli('server', 'firewall', 'disable', '--yes', 'local')


def verify_restart(h):
    owned(h)
    from recovery import init_identity
    state = json.loads((h.artifacts / CHECKPOINT).read_text())
    require(state['init'] != init_identity(), 'machine was not restarted')
    root(h, 'systemctl', 'is-system-running', '--wait', check=False, timeout=120)
    with peer(h) as p:
        checks(h, p, state)
        # An unreferenced administrator chain added AFTER enable must survive
        # disable exactly. Installing it before enable would rightly be refused.
        chain = 'TEST-FW-' + secrets.token_hex(4)
        for tool in ('iptables', 'ip6tables'):
            root(h, tool, '-N', chain)
            root(h, tool, '-A', chain, '-m', 'comment', '--comment', 'unrelated-state', '-j', 'RETURN')
        before = unrelated_snapshot(h, chain)
        all_before = rules_snapshot(h)
        h.cli('server', 'firewall', 'disable', '--yes', 'local')
        status(h, False)
        require(unrelated_snapshot(h, chain) == before, 'disable changed unrelated rules')
        require(rules_snapshot(h) == all_before, 'disable changed unrelated Docker/builtin rules or policies')
        for tool in ('iptables', 'ip6tables'):
            require(b'VAMOS-FW-' not in root(h, tool, '-S').stdout, 'owned rules remain after disable')
            root(h, tool, '-F', chain)
            root(h, tool, '-X', chain)
        p.probe(HOST, HOST_PORT)
        p.probe(HOST, TCP_PORT)
        p.probe(HOST, UDP_PORT, protocol='udp')
        # Docker state and Caddy must remain usable, not merely the foreign chain.
        h.https(state['app'] + '.vamos.localhost')
        rollback(h, p)
    root(h, 'systemctl', 'disable', '--now', state['host_unit'])
    (UNITS / state['host_unit']).unlink()
    root(h, 'systemctl', 'daemon-reload')
    h.cli('destroy', '--server', 'local', '--yes', cwd=h.work / state['app'])
    (h.artifacts / 'firewall-summary.json').write_text(json.dumps({
        'passed': True, 'packet_path': 'IPv4/IPv6 namespace/veth ingress (IPv4 container publishing)',
        'completed': ['fresh SSH', 'host deny', 'Caddy HTTP/HTTPS', 'public TCP/UDP',
                      'loopback upstream private', 'direct container deny',
                      'post-enable bridge', 'build HTTPS egress', 'Docker restart',
                      'machine restart', 'disable preserves unrelated chains', 'unconfirmed timer rollback'],
        'limitations': ['QUIC application traffic not exercised',
                        'OrbStack machine restart is not necessarily a shared-kernel reboot',
                        'restart checks assert final enforcement, not absence of a transient startup gap',
                        'direct-container denial may also be enforced by Docker raw rules'],
    }, indent=2) + '\n')
