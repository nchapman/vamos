"""Single-phase guest-loopback host networking and authenticated TURN acceptance."""
import argparse
import asyncio
import json
import secrets
import socket
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from build_network import IMAGE, configure_native
from linux_suite import Harness, bootstrap, require_guest
from scenarios import identities, require

APP = 'hostcheck'
PORT = 39471
SERVER = '''import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.bind(('127.0.0.1',int(sys.argv[1])))
while True:
    data,peer=s.recvfrom(256)
    s.sendto(sys.argv[2].encode()+data,peer)
'''


def probe(version):
    challenge = secrets.token_bytes(16)
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        sock.connect(('127.0.0.1', PORT))
        sock.settimeout(0.5)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            sock.send(challenge)
            try:
                reply = sock.recv(256)
            except (TimeoutError, ConnectionRefusedError):
                time.sleep(0.1)
                continue
            require(reply == version.encode() + challenge, 'unexpected UDP responder')
            return
    raise AssertionError('no guest loopback UDP reply')


TURN_IMAGE = 'coturn/coturn:4.19.1@sha256:81f69dc7cb672eb73ab3414afbb199637c37ab8069919d1c6caaed96ff16fb88'
TURN_PORT = 39472
RELAY_MIN, RELAY_MAX = 49160, 49191
LOOPBACK = '127.0.0.1'


async def relay_probe(password):
    # Guest distro client library only; neither credentials nor messages cross
    # the runner's process/log boundary. No STUN discovery or public ICE peer.
    from aioice import stun
    from aioice.turn import create_turn_endpoint

    replies = asyncio.Queue()

    class Client(asyncio.DatagramProtocol):
        def datagram_received(self, data, addr):
            replies.put_nowait((data, addr))

    async def allocate(transport, credential):
        return await create_turn_endpoint(Client, (LOOPBACK, TURN_PORT), username='guest',
                                          password=credential, transport=transport)

    loop = asyncio.get_running_loop()
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as peer:
        peer.bind((LOOPBACK, 0))
        peer.setblocking(False)
        for transport in ('udp', 'tcp'):
            try:
                unexpected, _ = await allocate(transport, secrets.token_hex(32))
            except stun.TransactionFailed as exc:
                require(exc.response.attributes['ERROR-CODE'][0] in (401, 441), 'expected TURN auth rejection')
            else:
                unexpected.close()
                raise AssertionError('TURN accepted incorrect credentials')
            client, _ = await allocate(transport, password)
            try:
                relay = client.get_extra_info('sockname')
                require(relay[0] == LOOPBACK and RELAY_MIN <= relay[1] <= RELAY_MAX, 'unexpected relay address')
                for _ in range(4):
                    payload = secrets.token_bytes(64)
                    client.sendto(payload, peer.getsockname())
                    data, source = await asyncio.wait_for(loop.sock_recvfrom(peer, 256), 5)
                    require((data, source) == (payload, relay), 'peer did not receive allocated relay payload')
                    peer.sendto(data[::-1], source)
                    require(await asyncio.wait_for(replies.get(), 5) == (payload[::-1], peer.getsockname()),
                            'client did not receive relayed reply')
                print(f'TURN/{transport}: 4 of 4 relayed round trips verified', flush=True)
            finally:
                client.close()


def coturn(h):
    name = 'turncheck'
    app = h.work / name
    app.mkdir()
    # Override upstream argument expansion. Shell builtin printf writes the
    # credential only into a private file on the container's /dev/shm tmpfs.
    (app / 'Dockerfile').write_text(f'FROM {TURN_IMAGE}\nCOPY --chmod=0444 start.sh /start.sh\n'
                                    'ENTRYPOINT ["/bin/sh", "/start.sh"]\nCMD []\n')
    (app / 'start.sh').write_text('''set -eu
umask 077
printf 'user=guest:%s\\n' "$TURN_PASSWORD" > /dev/shm/guest-turn.conf
unset TURN_PASSWORD
exec turnserver -c /dev/shm/guest-turn.conf "$@"
''')
    service = {'build': {'context': '.', 'dockerfile': 'Dockerfile'}, 'network_mode': 'host',
               'volume': {'name': 'data', 'mount': '/var/lib/coturn'},
               'secrets': {'TURN_PASSWORD': 'turn-password'},
               'command': ['--lt-cred-mech', '--realm=turn.localhost', '--no-tls',
                           '--no-tcp-relay', '--no-multicast-peers', '--allow-loopback-peers',
                           '--denied-peer-ip=0.0.0.0-255.255.255.255', '--allowed-peer-ip='+LOOPBACK,
                           '--userdb=/tmp/guest-turn.db', '--listening-ip='+LOOPBACK, '--relay-ip='+LOOPBACK,
                           f'--listening-port={TURN_PORT}', f'--min-port={RELAY_MIN}', f'--max-port={RELAY_MAX}']}
    (app / 'vamos.yaml').write_text(json.dumps({'app': name, 'services': {'turn': service}}))
    h.command(['chown', '-R', 'vamostest:vamostest', app], user='root')
    h.command(['apt-get', 'install', '-y', '--no-install-recommends', 'python3-aioice'], user='root')
    password = secrets.token_hex(32)
    target = ('--server', 'local', '--app', name)
    h.cli('secrets', 'stage', *target, '--stdin', 'turn-password', input=password.encode())
    h.cli('deploy', '--server', 'local', cwd=app)
    try:
        container = identities(h.status(name))['turn'][0]
        require(h.command(['docker', 'inspect', '--format', '{{.HostConfig.NetworkMode}}', container],
                          user='root').stdout.strip() == b'host', 'TURN service not on guest host network')
        for _ in range(50):
            try:
                with socket.create_connection((LOOPBACK, TURN_PORT), timeout=0.5):
                    break
            except (ConnectionRefusedError, TimeoutError):
                time.sleep(0.1)
        else:
            raise AssertionError('TURN listener did not start on guest loopback')
        asyncio.run(asyncio.wait_for(relay_probe(password), 60))
    finally:
        h.cli('destroy', *target, '--yes')


def run(h):
    bootstrap(h)
    h.command(['go', 'run', './cmd/build'])
    configure_native(h)
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost', 'local', 'root@127.0.0.1')
    app = h.work / APP
    app.mkdir()
    h.command(['chown', 'vamostest:vamostest', app], user='root')
    target = ('--server', 'local', '--app', APP)
    relay = {'image': IMAGE, 'network_mode': 'host'}
    manifest = {'app': APP, 'services': {'echo': relay, 'keeper': {'image': IMAGE, 'command': ['sleep', '3600']}}}

    def docker(*args):
        return h.command(['docker', *args], user='root').stdout.decode().strip()

    previous = None
    for version in ('first', 'second', 'third'):
        relay['command'] = ['python', '-c', SERVER, str(PORT), version]
        (app / 'vamos.yaml').write_text(json.dumps(manifest))
        h.cli('deploy', '--server', 'local', cwd=app)
        services = identities(h.status(APP))
        container = json.loads(docker('inspect', services['echo'][0]))[0]
        net = container['NetworkSettings']
        config = container['HostConfig']
        labels = container['Config']['Labels']
        bridge = json.loads(docker('network', 'inspect', 'vamos-' + APP))[0]
        require(labels['vamos.host'] and labels['vamos.host'] == bridge['Labels']['vamos.host'] and
                labels['vamos.operation'] and labels['vamos.app'] == APP and labels['vamos.service'] == 'echo',
                'status container lacks expected ownership')
        require(config['NetworkMode'] == 'host' and set(net['Networks']) == {'host'} and
                not net['Networks']['host']['Aliases'] and not config['PortBindings'] and not net['Ports'],
                'expected host endpoint without aliases or published ports')
        require(container['Id'] != previous, 'redeploy did not replace echo')
        previous = container['Id']
        probe(version)

    # The one-off must resolve the private keeper alias and reach it via the app bridge.
    keeper = json.loads(docker('inspect', services['keeper'][0]))[0]
    address = keeper['NetworkSettings']['Networks']['vamos-' + APP]['IPAddress']
    h.cli('run', '--server', 'local', '--service', 'echo', '--', 'python', '-c',
          'import socket,sys; assert socket.gethostbyname("keeper")==sys.argv[1]', address, cwd=app)
    h.cli('cleanup', *target, '--yes')
    h.cli('service', 'remove', 'echo', *target, '--yes')
    require(set(identities(h.status(APP))) == {'keeper'}, 'unexpected remaining membership')
    require(not docker('ps', '-aq', '--filter', 'label=vamos.app='+APP, '--filter', 'label=vamos.service=echo'),
            'echo containers survived removal')
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        sock.bind(('127.0.0.1', PORT))  # the removed service released its socket
    h.cli('destroy', *target, '--yes')
    require(not docker('ps', '-aq', '--filter', 'label=vamos.app='+APP) and
            not docker('network', 'ls', '-q', '--filter', 'label=vamos.app='+APP), 'app resources survived destroy')
    coturn(h)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--owner', required=True)
    require_guest(parser.parse_args().owner)
    harness = Harness()
    try:
        run(harness)
    except Exception:
        harness.diagnostics()
        raise
