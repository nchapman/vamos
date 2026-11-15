"""Real crash boundaries controlled by guest-only command gates, not sleeps."""
import copy
import json
import os
from pathlib import Path
import secrets
import select
import selectors
import shutil
import subprocess
import time

from checkpoints import Barrier, Client, install
from client_recovery import fresh_client
from scenarios import identities, require

UNIT = 'vamos-host-operation.service'


def active_worker_unit(h):
    out = h.command(['systemctl', 'list-units', '--type=service', '--state=active,activating',
                     '--plain', '--no-legend', '--no-pager', 'vamos-op-*.service', UNIT],
                    user='root').stdout.decode()
    units = [line.split()[0] for line in out.splitlines() if line.strip()]
    require(len(units) == 1, f'expected exactly one active worker unit, found {units!r}')
    return units[0]


def init_identity():
    # OrbStack machine restart is not necessarily a shared-kernel reboot.
    stat = Path('/proc/1/stat').read_text().rsplit(')', 1)[1].split()
    return {'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
            'init_start_ticks': stat[19]}


class Worker:
    def __init__(self, h):
        self.h = h
        self.unit = active_worker_unit(h)
        pid = int(h.command(['systemctl', 'show', '--property=MainPID', '--value', self.unit], user='root').stdout)
        require(pid > 1, 'checkpoint did not capture an active worker')
        self.fd = os.pidfd_open(pid)

    def __enter__(self):
        return self

    def wait(self, timeout=120):
        require(bool(select.select([self.fd], [], [], timeout)[0]), 'worker did not exit before deadline')

    def kill(self):
        self.h.command(['systemctl', 'kill', '--signal=SIGKILL', self.unit], user='root')
        self.wait()
        # Wait for cgroup cleanup too. A collected transient unit may be gone.
        self.h.command(['systemctl', 'stop', self.unit], user='root', check=False)

    def __exit__(self, *exc):
        os.close(self.fd)


def progress(h, name):
    path = h.artifacts / 'recovery-summary.json'
    state = json.loads(path.read_text()) if path.exists() else {'completed': []}
    state['completed'].append(name)
    path.write_text(json.dumps(state, indent=2) + '\n')
    h.log('recovery scenario passed: ' + name)


def docker_state(h, container):
    return json.loads(h.command(['docker', 'inspect', '--format', '{{json .State}}', container], user='root').stdout)


def no_restart(h, container):
    state = docker_state(h, container)
    policy = h.command(['docker', 'inspect', '--format', '{{.HostConfig.RestartPolicy.Name}}', container], user='root').stdout.strip()
    require(not state['Running'] and policy == b'no', 'retired container can run or restart')


def stable(h, app, before, names):
    status = h.status(app)
    for name in names:
        service = status['services'][name]
        require(tuple(service[k] for k in ('container', 'image', 'incarnation')) == tuple(before[name]),
                'unselected service identity changed: ' + name)
        require(service['running'] and service['available'], 'unselected service unavailable: ' + name)
    return status


def fixture(h, base, name, manifest, build='initial'):
    directory = base / name
    directory.mkdir()
    source = h.root / 'examples/ledger'
    for filename in ('Dockerfile', 'ledger.py', 'migrate.py', 'requirements.txt', 'bin/migrate', '.dockerignore'):
        target = directory / filename
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source / filename, target)
    save(h, directory, manifest, build)
    return directory


def save(h, directory, manifest, build):
    text = (h.root / 'examples/ledger/ledger.py').read_text()
    text = text.replace("return {'status': 'ok'}", "return {'status': 'ok', 'build': BUILD, 'release': os.environ['RELEASE']}")
    (directory / 'ledger.py').write_text('BUILD = ' + repr(build) + '\n' + text)
    (directory / 'vamos.yaml').write_text(json.dumps(manifest, indent=2) + '\n')
    h.command(['chown', '-R', 'vamostest:vamostest', str(directory)], user='root')


def version(h, host, build, release):
    body = json.loads(h.https(host, '/readyz'))
    require(body == {'status': 'ok', 'build': build, 'release': release}, 'unexpected serving source/config')


def routes_contain(value, host):
    if isinstance(value, dict):
        if host in value.get('host', []):
            return True
        return any(routes_contain(child, host) for child in value.values())
    return isinstance(value, list) and any(routes_contain(child, host) for child in value)


def build_diagnostics(h, app, base, api, manifest, host):
    bad = fixture(h, base, 'bad-build', manifest)
    marker = 'BUILD_DIAGNOSTIC_SENTINEL'
    value = secrets.token_urlsafe(32)
    h.cli('secrets', 'stage', '--server', 'local', '--app', app, '--stdin', 'diagnostic-only', input=value.encode())
    (bad / 'Dockerfile').write_text('FROM python:3.13-slim\nRUN printf "' + marker + ' ' + value + '\\n" >&2; exit 17\n')
    h.command(['chown', '-R', 'vamostest:vamostest', str(bad)], user='root')
    before = identities(h.status(app))
    result = h.cli('run', '--server', 'local', '--service', 'api', '--', 'python', '-c', 'print("never run")',
                   cwd=bad, expect_error='build failed')
    require(marker.encode() in result.stderr and value.encode() not in result.stderr + result.stdout,
            'run submission lost diagnostic or exposed a staged secret')
    status = h.status(app)
    operation = status['run']['operation']
    require(status['run']['phase'] == 'not-started' and status['run']['exit_code'] is None,
            'failed build fabricated command execution')
    observed = h.cli('status', '--server', 'local', '--app', app, '--run', operation, '--json').stdout
    logs = h.cli('logs', '--server', 'local', '--app', app, '--run', operation).stdout
    record = Path('/var/lib/vamos/apps') / app / (operation + '.run.json')
    for output in (observed, logs, record.read_bytes()):
        require(marker.encode() in output and value.encode() not in output, 'persisted run diagnostic missing or unsafe')
    h.cli('service', 'bind', 'api', '--server', 'local', '--app', app, cwd=bad)
    h.cli('deploy', '--server', 'local', cwd=bad, expect_error='build failed')
    failed = stable(h, app, before, before)
    require(marker in failed['last_error'] and value not in json.dumps(failed), 'deploy diagnostic missing or unsafe')
    require(value.encode() not in (Path('/var/lib/vamos/apps') / app / 'state.json').read_bytes(),
            'deploy journal contains a staged secret')
    version(h, host, 'initial', 'initial')
    h.cli('deploy', '--server', 'local', '--supersede', failed['operation'], cwd=api)
    stable(h, app, before, before)
    progress(h, 'build failures persist bounded redacted diagnostics without running commands or replacing services')


def prepare(h, owner):
    install(h)
    app = 'recovery-' + secrets.token_hex(4)
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
    row = json.loads(h.https(host, '/entries', {'amount_cents': 812, 'description': app}))
    version(h, host, 'initial', 'initial')
    build_diagnostics(h, app, base, api, am, host)

    before = identities(h.status(app))
    am['services']['api']['env']['RELEASE'] = 'detached'
    save(h, api, am, 'detached')
    with Barrier('docker', ['buildx', 'build'], timeout=180) as gate:
        with Client(h, ['deploy', '--server', 'local'], api) as client:
            gate.wait()
            with Worker(h) as remote:
                pending = h.status(app)
                require(pending['active'], 'worker not active at build checkpoint')
                operation = pending['operation']
                version(h, host, 'initial', 'initial')
                client.disconnect()
                require(client.wait(check=False).returncode != 0, 'client disconnect did not interrupt transport')
                require(h.status(app)['active'], 'client disconnect killed the independent worker')
                am['services']['api']['env']['RELEASE'] = 'unsubmitted'
                save(h, api, am, 'unsubmitted')
                gate.release()
                remote.wait(180)
    settled = stable(h, app, before, ('database', 'worker'))
    require(settled['phase'] == 'ready' and not settled['active'] and settled['operation'] == operation,
            'detached deployment did not settle')
    version(h, host, 'detached', 'detached')
    progress(h, 'client/SSH disconnect leaves worker running and captured source/config unchanged')

    before = identities(h.status(app))
    am['services']['api']['env']['RELEASE'] = 'promoted'
    save(h, api, am, 'promoted')
    with Barrier('docker', ['exec', 'vamos-caddy', 'caddy', 'reload'], when='after', timeout=180) as gate:
        with Client(h, ['deploy', '--server', 'local'], api) as client:
            gate.wait()
            with Worker(h) as remote:
                pending = h.status(app)
                operation = pending['operation']
                candidate = pending['services']['api']['candidate']
                require(candidate and pending['active'], 'missing pending promotion candidate')
                version(h, host, 'promoted', 'promoted')
                remote.kill()
            require(client.wait(check=False).returncode != 0, 'killed worker reported success')
    am['services']['api']['env']['RELEASE'] = 'never-submitted'
    save(h, api, am, 'never-submitted')
    # Lose only the client registry, retaining independently trusted SSH material.
    # Re-registration must not bootstrap, rewrite the pending journal or restart apps.
    observed = h.status(app)
    journal_path = '/var/lib/vamos/apps/' + app + '/state.json'
    journal_before = h.command(['sha256sum', journal_path], user='root').stdout
    with fresh_client(h):
        require(h.status(app) == observed, 'connect changed pending app state')
        h.cli('apps', '--server', 'local', '--json')
        require(h.command(['sha256sum', journal_path], user='root').stdout == journal_before,
                'connect or inventory rewrote the interrupted operation')
        h.cli('resume', '--server', 'local', '--app', app, operation)
        recovered = fixture(h, base, 'recovered-client-project', am, 'never-submitted')
        before_bind = h.status(app)
        h.cli('service', 'bind', 'api', '--server', 'local', '--app', app, cwd=recovered)
        require(h.status(app) == before_bind, 'explicit project binding changed remote state')
    progress(h, 'fresh client reconnects to populated host, inspects and resumes captured operation, then binds locally')
    after = stable(h, app, before, ('database', 'worker'))
    require(after['phase'] == 'ready' and after['services']['api']['container'] == candidate,
            'resume replaced the promoted candidate')
    version(h, host, 'promoted', 'promoted')
    progress(h, 'worker killed after real Caddy reload resumes the same candidate and captured inputs')

    retire = fixture(h, base, 'retire', {'app': app, 'services': {'retire': copy.deepcopy(web)}}, 'retire')
    h.cli('deploy', '--server', 'local', cwd=retire)
    retire_host = app + '-retire.vamos.localhost'
    h.hosts([retire_host])
    before = identities(h.status(app))
    target = before['retire'][0]
    target_id = h.command(['docker', 'inspect', '--format', '{{.Id}}', target], user='root').stdout.decode().strip()
    with Barrier('docker', ['container', 'rm'], contains=target_id, timeout=180) as gate:
        with Client(h, ['service', 'remove', 'retire', '--server', 'local', '--app', app, '--yes'], base) as client:
            gate.wait()
            with Worker(h) as remote:
                pending = h.status(app)
                # Maintenance has its own resume ID, distinct from the last deployment.
                operation = pending['operation']
                require(not pending['services']['retire']['available'], 'removing route advertised available')
                live = json.loads(h.command(['curl', '--fail', '--silent', 'http://127.0.0.1:2019/config/'], user='root').stdout)
                require(not routes_contain(live, retire_host), 'removal did not withdraw route before container removal')
                remote.kill()
            require(client.wait(check=False).returncode != 0, 'interrupted removal reported success')
    h.cli('deploy', '--server', 'local', cwd=api, expect_error='resume')
    h.cli('resume', '--server', 'local', '--app', app, operation)
    require('retire' not in h.status(app)['services'], 'resume resurrected removed service')
    stable(h, app, before, ('api', 'database', 'worker'))
    h.cli('deploy', '--server', 'local', cwd=retire, expect_error='--re-add')
    progress(h, 'interrupted removal withdraws routes durably and cannot resurrect retired service')

    before = identities(h.status(app))
    h.cli('secrets', 'stage', '--server', 'local', '--app', app, '--stdin', 'database-password',
          input=secrets.token_urlsafe(36).encode())
    wm['services']['worker']['env']['REVISION'] = 'two'
    save(h, worker, wm, 'worker-two')
    old_worker = before['worker'][0]
    with Barrier('docker', ['stop'], contains=old_worker, when='after', timeout=180) as gate:
        with Client(h, ['deploy', '--server', 'local'], worker) as client:
            gate.wait()
            with Worker(h) as remote:
                pending = h.status(app)
                operation = pending['operation']
                no_restart(h, old_worker)
                remote.kill()
            require(client.wait(check=False).returncode != 0, 'interrupted private replacement reported success')
    status = stable(h, app, before, ('api', 'database'))
    require(any(s['name'] == 'database-password' and s['pending'] for s in status['secrets']),
            'staged database secret was not left pending')
    require(row in json.loads(h.https(host, '/entries')), 'row lost before restart')
    checkpoint = {'prepared': True, 'owner': owner, 'init': init_identity(), 'app': app, 'host': host,
                  'operation': operation, 'old_worker': old_worker, 'before': before,
                  'row': row, 'volume': status['volumes'][0]['id'], 'secrets': status['secrets']}
    path = h.work / 'restart.json'
    path.write_text(json.dumps(checkpoint))
    path.chmod(0o600)
    progress(h, 'private replacement interrupted after retirement, with data and pending secrets checkpointed')


def wait_database(h, container):
    state = docker_state(h, container)
    require(state['Running'], 'database did not restart automatically')
    # Follow only this start's log stream: a blocking event wait, not sleep/polling
    # and not an implicit migration. PostgreSQL emits this after recovery completes.
    args = ['/usr/bin/docker', 'logs', '--follow', '--since', state['StartedAt'], container]
    with subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
        selector = selectors.DefaultSelector()
        selector.register(process.stdout, selectors.EVENT_READ)
        deadline = time.monotonic() + 90
        output = b''
        try:
            while time.monotonic() < deadline:
                if not selector.select(max(0, deadline - time.monotonic())):
                    break
                chunk = os.read(process.stdout.fileno(), 8192)
                if not chunk:
                    break
                output = (output + chunk)[-65536:]
                if b'database system is ready to accept connections' in output:
                    return
            raise AssertionError('database startup readiness event missing')
        finally:
            selector.close()
            process.terminate()
            process.wait(timeout=10)


def verify_restart(h, owner):
    checkpoint = json.loads((h.work / 'restart.json').read_text())
    require(checkpoint['prepared'] and checkpoint['owner'] == owner, 'restart checkpoint ownership mismatch')
    require(checkpoint['init'] != init_identity(), 'machine init was not restarted')
    boot = h.command(['systemctl', 'is-system-running', '--wait'], user='root', check=False, timeout=180)
    require(boot.stdout.strip() in (b'running', b'degraded'), 'machine did not finish boot')
    h.command(['systemctl', 'is-active', '--quiet', 'ssh', 'docker', 'vamos-agent'], user='root')
    require(h.command(['docker', 'container', 'inspect', '--format', '{{.State.Running}}', 'vamos-caddy'],
                      user='root').stdout.strip() == b'true', 'Caddy container did not restart with Docker')
    app, host = checkpoint['app'], checkpoint['host']
    no_restart(h, checkpoint['old_worker'])
    wait_database(h, checkpoint['before']['database'][0])
    status = stable(h, app, checkpoint['before'], ('api', 'database'))
    require(status['operation'] == checkpoint['operation'] and status['phase'] != 'ready',
            'restart lost the captured incomplete operation')
    require(status['secrets'] == checkpoint['secrets'], 'restart changed pinned or pending secret metadata')
    require(status['volumes'][0]['id'] == checkpoint['volume'], 'restart changed volume identity')
    require('retire' not in status['services'], 'removed service resurrected after restart')
    require(checkpoint['row'] in json.loads(h.https(host, '/entries')), 'restart lost persisted row or applied pending password')
    version(h, host, 'promoted', 'promoted')
    h.cli('resume', '--server', 'local', '--app', app, checkpoint['operation'])
    status = stable(h, app, checkpoint['before'], ('api', 'database'))
    require(status['phase'] == 'ready' and status['services']['worker']['running'], 'private replacement did not recover')
    require(status['services']['worker']['container'] != checkpoint['old_worker'], 'retired worker resurrected')
    no_restart(h, checkpoint['old_worker'])
    require(status['secrets'] == checkpoint['secrets'], 'resume selected pending database secret')
    progress(h, 'machine restart preserves data, pinned/pending secrets and retirement; resume completes private replacement')
    h.cli('destroy', '--server', 'local', '--app', app, '--yes')
    require(h.status(app)['phase'] == 'destroyed', 'recovery app not destroyed')
    checkpoint['prepared'] = False
    (h.work / 'restart.json').write_text(json.dumps(checkpoint))
