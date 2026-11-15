"""Docker daemon restarts at real build/deploy boundaries, controlled by command gates.

Live-restore is off, so every container stops with dockerd. Recovery comes from
restart policies plus re-inspection: apps keep serving their effective release
and the interrupted operation completes in-line. Release commands are the
deliberate exception: a killed execution is a durable failure, never retried.
"""
import json
import secrets
import time

from checkpoints import Barrier, Client
from recovery import Worker, docker_state, fixture, progress, save, version
from scenarios import identities, require


def restart_docker(h):
    # docker.service is Type=notify: restart returns once the new daemon serves.
    h.command(['systemctl', 'restart', 'docker.service'], user='root')
    h.command(['docker', 'version', '--format', '{{.Server.Version}}'], user='root')


def wait_serving(h, host, build, release, deadline=90):
    # Restart policies start containers asynchronously; poll observable HTTPS only.
    end = time.monotonic() + deadline
    while True:
        try:
            return version(h, host, build, release)
        except (AssertionError, RuntimeError, json.JSONDecodeError):
            if time.monotonic() > end:
                raise
            time.sleep(1)


def guards(h):
    for tool in ('iptables', 'ip6tables'):
        rules = h.command([tool, '-w', '5', '-t', 'mangle', '-S', 'FORWARD'], user='root').stdout
        require(b'vamos-build0' in rules, tool + ' build guards missing after Docker restart')


def interrupted(h, app, directory, gate_args):
    """Run one deploy, restart dockerd at the gate, and require in-line recovery.

    Each step after the gate re-inspects Docker state rather than trusting what
    it observed before the restart, so the same operation completes.
    """
    with Barrier('docker', **gate_args, timeout=300) as gate:
        with Client(h, ['deploy', '--server', 'local'], directory) as client:
            gate.wait()
            with Worker(h) as remote:
                operation = h.status(app)['operation']
                restart_docker(h)
                gate.release()
                remote.wait(300)
            client.wait()
    status = h.status(app)
    require(status['operation'] == operation and status['phase'] == 'ready' and not status['active'],
            'operation did not complete after Docker restart')


def run(h):
    app = 'dockerd-' + secrets.token_hex(4)
    base = h.work / app
    base.mkdir()
    db = {'image': 'postgres:17', 'update': 'manual',
          'env': {'POSTGRES_DB': 'ledger', 'POSTGRES_USER': 'ledger'},
          'secrets': {'POSTGRES_PASSWORD': 'database-password'},
          'volume': {'name': 'data', 'mount': '/var/lib/postgresql/data'}}
    web = {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
           'env': {'HOST': 'database', 'DB': 'ledger', 'USER': 'ledger', 'RELEASE': 'initial'},
           'secrets': {'PASSWORD': 'app-password'}, 'http': {'port': 8080, 'health': '/readyz'}}
    am = {'app': app, 'services': {'api': web}}
    wm = {'app': app, 'services': {'worker': {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
          'env': {'REVISION': 'one'}, 'command': ['python', '-m', 'http.server', '8081']}}}
    database = fixture(h, base, 'database', {'app': app, 'services': {'database': db}})
    api = fixture(h, base, 'api', am)
    worker = fixture(h, base, 'worker', wm)
    password = secrets.token_urlsafe(36).encode()
    for name in ('database-password', 'app-password'):
        h.cli('secrets', 'stage', '--server', 'local', '--app', app, '--stdin', name, input=password)
    h.cli('deploy', '--server', 'local', cwd=database)
    h.cli('run', '--server', 'local', '--service', 'api', '--', './bin/migrate', cwd=api)
    h.cli('deploy', '--server', 'local', cwd=api)
    h.cli('deploy', '--server', 'local', cwd=worker)
    host = app + '-api.vamos.localhost'
    h.hosts([host])
    row = json.loads(h.https(host, '/entries', {'amount_cents': 4242, 'description': app}))
    version(h, host, 'initial', 'initial')

    def converged(build, release, before=None, keep=()):
        wait_serving(h, host, build, release)
        status = h.status(app)
        for name, service in status['services'].items():
            require(service['running'], 'service not running after Docker restart: ' + name)
        for name in keep:
            require(identities(status)[name] == before[name], 'unselected service replaced: ' + name)
        require(row in json.loads(h.https(host, '/entries')), 'row lost across Docker restart')
        require(docker_state(h, 'vamos-caddy')['Running'], 'Caddy container did not return')
        return status

    before = identities(h.status(app))
    restart_docker(h)
    converged('initial', 'initial', before, ('api', 'database', 'worker'))
    progress(h, 'idle Docker restart returns every service, Caddy and data without intervention')

    am['services']['api']['env']['RELEASE'] = 'build'
    save(h, api, am, 'build')
    before = identities(h.status(app))
    interrupted(h, app, api, dict(prefix=['buildx', 'build']))
    converged('build', 'build', before, ('database', 'worker'))
    guards(h)
    progress(h, 'Docker restart at build start: builder and guards recover, build retries once')

    am['services']['api']['env']['RELEASE'] = 'promotion'
    save(h, api, am, 'promotion')
    before = identities(h.status(app))
    interrupted(h, app, api, dict(prefix=['exec', 'vamos-caddy', 'caddy', 'reload'], when='after'))
    converged('promotion', 'promotion', before, ('database', 'worker'))
    progress(h, 'Docker restart after Caddy reload converges the promotion')

    am['services']['api']['env']['RELEASE'] = 'retire'
    save(h, api, am, 'retire')
    before = identities(h.status(app))
    old = before['api'][0]
    interrupted(h, app, api, dict(prefix=['stop'], contains=old))
    status = converged('retire', 'retire', before, ('database', 'worker'))
    require(status['services']['api']['container'] != old, 'retired container still selected')
    state = docker_state(h, old) if h.command(['docker', 'container', 'inspect', old], user='root',
                                               check=False).returncode == 0 else None
    require(state is None or not state['Running'], 'retired container resurrected by Docker restart')
    progress(h, 'Docker restart during retirement keeps the old container retired')

    wm['services']['worker']['env']['REVISION'] = 'two'
    save(h, worker, wm, 'worker-two')
    before = identities(h.status(app))
    interrupted(h, app, worker, dict(prefix=['run', '--detach'], when='after'))
    status = converged('retire', 'retire', before, ('api', 'database'))
    require(status['services']['worker']['container'] != before['worker'][0], 'private replacement not applied')
    progress(h, 'Docker restart after private candidate start converges the replacement')

    # Release commands are never retried automatically: an execution the daemon
    # killed is a known failure that needs an explicit superseding deployment.
    am['release'] = {'service': 'api', 'command': ['sh', '-c', 'sleep 60 && ./bin/migrate']}
    am['services']['api']['env']['RELEASE'] = 'release'
    save(h, api, am, 'release')
    before = identities(h.status(app))
    with Barrier('docker', ['start'], when='after', timeout=300) as gate:
        with Client(h, ['deploy', '--server', 'local'], api) as client:
            gate.wait()
            with Worker(h) as remote:
                operation = h.status(app)['operation']
                restart_docker(h)
                gate.release()
                remote.wait(300)
            require(client.wait(check=False).returncode != 0, 'interrupted release reported success')
    status = h.status(app)
    require(status['release']['phase'] == 'complete' and status['release']['exit_code'] != 0,
            'killed release command not recorded as a known failure')
    wait_serving(h, host, 'retire', 'retire')
    am['release']['command'] = ['./bin/migrate']
    save(h, api, am, 'release')
    h.cli('deploy', '--server', 'local', '--supersede', operation, cwd=api)
    converged('release', 'release', before, ('database', 'worker'))
    progress(h, 'Docker restart during a release command fails it durably; a superseding deploy recovers')

    h.cli('destroy', '--server', 'local', '--app', app, '--yes')
    require(h.status(app)['phase'] == 'destroyed', 'Docker restart app not destroyed')
