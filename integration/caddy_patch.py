"""Single-boot live refresh acceptance. All fixture writes stay in the owned guest."""
import hashlib
import json
from pathlib import Path

from build_network import IMAGE
from linux_suite import ROOT, require_guest
from scenarios import require

UPSTREAM = 'ghcr.io/nchapman/vamos/caddy'
REPO = '127.0.0.1:5000/vamos/caddy'
LINE = REPO + ':20261120'
SERVICE = 'vamos-caddy-refresh.service'
TIMER = 'vamos-caddy-refresh.timer'


def run(h, owner):
    require_guest(owner)
    source = ROOT / 'internal/lifecycle/caddy.go'
    marker = f'\tcaddyRepo   = "{UPSTREAM}"\n'
    original = source.read_text()
    require(source.resolve() == source and original.count(marker) == 1, 'unexpected Caddy source')
    # cmd/build hashes its own source snapshot and stamps the entire bundle;
    # it intentionally ignores ambient GOFLAGS/overlays.
    try:
        source.write_text(original.replace(marker, marker.replace(UPSTREAM, REPO)))
        h.command(['go', 'run', './cmd/build'])
    finally:
        source.write_text(original)

    def root(*argv):
        return h.command(argv, user='root').stdout.decode().strip()

    # Installing docker-registry itself trips the unmanaged docker* package
    # preflight. Extract its executable instead; no Docker daemon or images yet.
    root('apt-get', 'install', '-y', '--no-install-recommends', 'skopeo')
    h.command(['apt-get', 'download', 'docker-registry'], cwd=h.work)
    package, = h.work.glob('docker-registry_*.deb')
    root('dpkg-deb', '-x', package, h.work / 'distro')
    config = h.work / 'registry.yml'
    config.write_text(f'version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: {h.work}/blobs\n'
                      'http:\n  addr: 127.0.0.1:5000\n')
    root('systemd-run', '--unit=caddy-patch-registry', h.work / 'distro/usr/bin/docker-registry', 'serve', config)
    root('curl', '-fsS', '--retry', '10', '--retry-connrefused', 'http://127.0.0.1:5000/v2/')
    image = h.work / 'image'
    local = 'dir:' + str(image)
    # Start with the same multi-platform index shape used in production.
    upstream = 'docker://' + UPSTREAM + '@sha256:b6a94eb93915955ea6df57a16dd69b80274fc21b188713d998261b49e4975eea'

    def promote(patch):
        tag = 'docker://' + LINE + '.' + str(patch)
        if patch == 0:
            root('skopeo', 'copy', '--all', '--dest-tls-verify=false', upstream, tag)
        else:
            root('skopeo', 'copy', '--dest-tls-verify=false', local, tag)
        digest = h.work / 'line.digest'
        root('skopeo', 'copy', '--all', '--preserve-digests', '--src-tls-verify=false', '--dest-tls-verify=false',
             '--digestfile', digest, tag, 'docker://' + LINE)
        return LINE + '@' + digest.read_text().strip()

    first = promote(0)
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost', 'local', 'root@127.0.0.1')
    root('systemctl', 'is-enabled', TIMER)
    root('systemctl', 'is-active', TIMER)
    require(root('systemctl', 'show', '-p', 'Triggers', '--value', TIMER) == SERVICE, 'wrong timer target')
    app = h.work / 'probe'
    app.mkdir()
    (app / 'vamos.yaml').write_text(json.dumps({'app': 'probe', 'services': {'web': {
        'image': IMAGE, 'command': ['python', '-m', 'http.server', '8080'], 'http': {'port': 8080}}}}))
    root('chown', '-R', 'vamostest:vamostest', app)
    h.cli('deploy', '--server', 'local', cwd=app)

    def observe(expected):
        container, = json.loads(root('docker', 'inspect', 'vamos-caddy'))
        require(container['Config']['Image'] == expected, 'Caddy not running expected immutable reference')
        require(b'Directory listing' in h.https('probe.vamos.localhost'), 'app route lost')
        return container['Id'], container['Image'], container['State']['StartedAt']

    def inventory(ref):
        # Compare names/digests, not Docker's listing ID to a config hash:
        # containerd-backed Docker and classic Docker report different IDs.
        digest = ref.split('@')[1]
        rows = root('docker', 'image', 'ls', '-a', '--digests', '--format',
                    '{{.Repository}}:{{.Tag}}@{{.Digest}}')
        names = root('ctr', '--namespace', 'moby', 'images', 'ls', '-q')
        return [sorted(text.replace(digest, '<current>').splitlines()) for text in (rows, names)]

    old = observe(first)
    baseline = inventory(first)
    root('systemctl', 'start', SERVICE)
    require(observe(first) == old, 'unchanged line restarted Caddy')

    # Change only the config blob's label, preserving the platform and every layer.
    root('skopeo', 'copy', '--format', 'v2s2', '--src-tls-verify=false', 'docker://' + LINE, local)
    manifest_path = image / 'manifest.json'
    manifest = json.loads(manifest_path.read_text())
    descriptor = manifest['config']
    cfg = json.loads((image / descriptor['digest'].removeprefix('sha256:')).read_bytes())
    cfg['config']['Labels'] = dict(cfg['config'].get('Labels') or {}, **{'integration.vamos.patch': '1'})
    blob = json.dumps(cfg).encode()
    digest = hashlib.sha256(blob).hexdigest()
    (image / digest).write_bytes(blob)
    descriptor.update(digest='sha256:' + digest, size=len(blob))
    manifest_path.write_text(json.dumps(manifest))
    second = promote(1)
    root('systemctl', 'start', SERVICE)
    new = observe(second)
    require(old[0] != new[0] and old[1] != new[1], 'patch did not replace container and image')
    root('systemctl', 'start', SERVICE)
    require(observe(second) == new, 'patched line restarted on repeat refresh')

    require(inventory(second) == baseline, 'refresh left unexpected Docker or containerd image entries')
    h.cli('destroy', '--server', 'local', '--app', 'probe', '--yes')
    root('docker', 'image', 'rm', IMAGE)  # Only the unowned probe image, never Caddy refs.
    # Capture both inventories. Production teardown checks the classic Docker
    # objects AND moby namespace images/content before it can purge the runtime.
    root('docker', 'info', '--format', '{{json .DriverStatus}}')
    root('docker', 'image', 'ls', '-a', '--no-trunc', '--digests')
    root('ctr', '--namespace', 'moby', 'images', 'ls')
    h.cli('server', 'teardown', '--yes', '--purge', 'local')
    require(json.loads(Path('/var/lib/vamos/host.json').read_text())['phase'] == 'removed', 'incomplete purge')
    for unit in (TIMER, SERVICE):
        require(root('systemctl', 'show', '-p', 'LoadState', '--value', unit) == 'not-found', f'{unit} remains')
