"""Real runtime acceptance, called only after linux_suite's guest ownership gate.

No feature detection/skips: merged replicas/resources/release APIs are required.
All remote work uses the existing local SSH target and Harness command artifacts.
On failure leave the owned guest intact for diagnostics; on success retain volumes.
"""
import copy
from datetime import datetime
import json
import re
import secrets
import shlex
import shutil
import time

from scenarios import require


LIMITS = {"cpus": 0.5, "memory": "512m"}
# Do not dump docker inspect wholesale: Config.Env contains injected secrets.
INSPECT = '{' + ','.join('"' + key + '":{{json ' + field + '}}' for key, field in (
    ('id', '.Id'), ('image', '.Image'), ('hostname', '.Config.Hostname'),
    ('running', '.State.Running'), ('started', '.State.StartedAt'),
    ('finished', '.State.FinishedAt'), ('created', '.Created'),
    ('cpus', '.HostConfig.NanoCpus'), ('memory', '.HostConfig.Memory'),
    ('swap', '.HostConfig.MemorySwap'), ('ports', '.NetworkSettings.Ports'),
    ('networks', '.NetworkSettings.Networks'), ('mounts', '.Mounts'),
)) + '}'


def instances(status, counts):
    """Require the public worktree schema, including singleton instance records."""
    require(set(status['services']) == set(counts), 'unexpected service membership')
    result = {}
    for name, count in counts.items():
        service = status['services'][name]
        values = service.get('instances', [])
        require(len(values) == count, f'{name}: missing instances')
        require([v['ordinal'] for v in values] == list(range(1, count + 1)),
                f'{name}: invalid 1-based ordinals')
        require(service['running'] and service['available'], f'{name}: unavailable service')
        require(all(v['running'] and v['available'] and v['container'] for v in values),
                f'{name}: unavailable instance')
        result[name] = tuple(v['container'] for v in values)
    flat = [v for values in result.values() for v in values]
    require(len(set(flat)) == len(flat), 'instances share a container')
    return result


def resources(info):
    require(info['cpus'] == 500_000_000, 'Docker CPU limit not enforced')
    require(info['memory'] == 512 * 1024 * 1024, 'Docker memory limit not enforced')
    require(info['swap'] == info['memory'], 'Docker permits additional/unlimited swap')


def cgroup_limits(output):
    cpu, memory, swap = output.decode().splitlines()
    quota, period = map(int, cpu.split())
    require(quota > 0 and quota * 2 == period, 'kernel CPU quota differs from limit')
    require(int(memory) == 512 * 1024 * 1024, 'kernel memory ceiling differs from limit')
    require(int(swap) == 0, 'kernel permits additional swap')


def release_result(status, code):
    value = status['release']
    require(value['operation'] and value['operation'] != status['operation'],
            'release needs its own operation identity')
    require(value['parent_operation'] == status['operation'], 'release parent mismatch')
    require(value['service'] == 'web' and value['phase'] == 'complete', 'release not complete')
    require(value['exit_code'] == code, 'release exit status mismatch')
    return value


def wait_processed(fetch, timeout=90, clock=time.monotonic, sleep=time.sleep):
    """Poll only the documented asynchronous job effect, never retry a deploy."""
    deadline = clock() + timeout
    while True:
        row = fetch()
        require(row['status'] in ('pending', 'processed'), 'unknown processing state')
        if row['status'] == 'processed':
            require(row['processed_at'] and row['worker_marker'], 'incomplete worker effect')
            return row
        require(clock() < deadline, 'Solid Queue job did not finish before deadline')
        sleep(0.5)


def parse_http(output):
    headers, body = output.split(b'\r\n\r\n', 1)
    require(headers.splitlines()[0].split()[1] == b'200', 'replica HTTP was not 200')
    values = dict(line.split(b':', 1) for line in headers.split(b'\r\n')[1:] if b':' in line)
    markers = [v.strip().decode() for k, v in values.items() if k.lower() == b'x-demo-instance']
    require(len(markers) == 1 and markers[0], 'missing demo instance marker')
    return markers[0], json.loads(body)


def run(h):
    summary = {'completed': [], 'limitations': [
        'Rails fixture has one PostgreSQL database with app and Solid Queue migrations; '
        'bin/release runs db:migrate, not db:prepare or a three-database layout.',
        'Replica console selection is checked through a PTY; terminal resize/interrupt behavior is covered by cli_scenarios.',
        'No SQL password rotation, data purge, or production capacity claim.',
    ]}

    def report():
        (h.artifacts / 'runtime-summary.json').write_text(json.dumps(summary, indent=2) + '\n')

    def done(name):
        summary['completed'].append(name)
        report()
        h.log('runtime scenario passed: ' + name)

    report()
    try:
        rails(h, summary, done)
        python_ledger(h, summary, done)
        summary['passed'] = True
    except Exception as exc:
        summary['passed'] = False
        summary['failure'] = type(exc).__name__
        raise
    finally:
        report()


class Project:
    def __init__(self, h, example, prefix):
        self.h = h
        self.app = prefix + secrets.token_hex(4)
        self.directory = h.work / self.app
        shutil.copytree(h.root / 'examples' / example, self.directory,
                        ignore=shutil.ignore_patterns('.vamos', '__pycache__', '.bundle', 'vendor', 'log', 'tmp'))
        self.host = f'{self.app}.vamos.localhost'
        self.alias = f'{self.app}-alias.vamos.localhost'
        h.hosts([self.host, self.alias])

    def save(self, manifest):
        (self.directory / 'vamos.yaml').write_text(json.dumps(manifest, indent=2) + '\n')
        self.h.command(['chown', '-R', 'vamostest:vamostest', self.directory], user='root')

    def cli(self, operation, *args, error=None):
        return self.h.cli(operation, '--server', 'local', *args,
                          cwd=self.directory, expect_error=error)

    def stage(self, name, value):
        self.h.cli('secrets', 'stage', '--server', 'local', '--app', self.app,
                   '--stdin', name, input=value)

    def status(self):
        return self.h.status(self.app)  # Raw Status, not {status: ...}.

    def inspect(self, container):
        return json.loads(self.h.command(['docker', 'inspect', '--format', INSPECT, container],
                                         user='root').stdout)

    def snapshot(self, counts):
        status = self.status()
        names = instances(status, counts)
        actual = {name: [self.inspect(c) for c in containers] for name, containers in names.items()}
        ids = [v['id'] for group in actual.values() for v in group]
        require(len(set(ids)) == len(ids), 'Docker IDs are not distinct')
        for name, group in actual.items():
            for value in group:
                require(value['running'], f'Docker reports {name} stopped')
                resources(value)
                # The Ubuntu acceptance guest uses cgroup v2. Check the kernel
                # ceilings as well as Docker's requested configuration.
                limits = self.h.command([
                    'docker', 'exec', value['id'], 'sh', '-c',
                    'read quota period < /sys/fs/cgroup/cpu.max; '
                    'read memory < /sys/fs/cgroup/memory.max; '
                    'read swap < /sys/fs/cgroup/memory.swap.max; '
                    'printf "%s %s\\n%s\\n%s\\n" "$quota" "$period" "$memory" "$swap"'
                ], user='root').stdout
                cgroup_limits(limits)
        return status, actual

    def stable(self, before, counts, selected=None):
        status, after = self.snapshot(counts)
        for name in selected if selected is not None else before:
            require([(v['id'], v['started']) for v in after[name]] ==
                    [(v['id'], v['started']) for v in before[name]],
                    f'unselected/blocked {name} changed Docker identity or restarted')
        return status, after

    def release(self, status, code=0):
        hook = release_result(status, code)
        info = self.inspect(hook['container'])
        require(not info['running'], 'release container still running')
        resources(info)
        logs = self.cli('logs', '--run', hook['operation']).stdout
        # Exactly one retained release runner for this parent, not one per replica/service.
        found = self.h.command(['docker', 'ps', '-aq', '--no-trunc', '--filter',
                                'label=vamos.release=' + hook['parent_operation']], user='root').stdout
        require(found.decode().split() == [info['id']], 'release did not run once per operation')
        return hook, info, logs

    def destroy(self, volume):
        self.cli('cleanup', '--yes')
        self.cli('destroy', '--yes')
        status = self.status()
        require(status['phase'] == 'destroyed', 'app not destroyed')
        require(any(v['id'] == volume and not v['missing'] for v in status['volumes']),
                'destroy removed retained PostgreSQL data')
        require(status.get('secrets'), 'destroy removed secret provenance')


def database(name, user):
    return {'image': 'postgres:17', 'update': 'manual', 'resources': dict(LIMITS),
            'env': {'POSTGRES_DB': name, 'POSTGRES_USER': user},
            'secrets': {'POSTGRES_PASSWORD': 'database-password'},
            'volume': {'name': 'data', 'mount': '/var/lib/postgresql/data'}}


def rails_database_tests(project):
    # Production images exclude tests. Build a separate one-off image from the
    # same captured application with only the test directory added, then restore
    # the production context before any later deploy. Never use its live DB.
    ignore = project.directory / '.dockerignore'
    original = ignore.read_text()
    try:
        ignore.write_text(original + '\n!/test\n!/test/**\n')
        # Separate processes close setup connections before Rails prepares its
        # test schema; a combined Rake invocation can report a failed DB purge
        # while still running tests and returning zero.
        result = project.cli('run', '--service', 'web', '--', 'env',
                             'RAILS_ENV=test', 'DB_TEST_NAME=rails_ledger_test',
                             'sh', '-c', 'bin/rails db:create db:migrate && exec bin/rails test')
        require(b'aborted!' not in result.stdout + result.stderr,
                'Rails database setup or test task aborted despite its exit status')
        summary = re.search(rb'(\d+) runs, (\d+) assertions, 0 failures, 0 errors, 0 skips',
                            result.stdout)
        require(summary is not None and int(summary[1]) >= 12,
                'Rails database test suite did not run all expected tests successfully')
    finally:
        ignore.write_text(original)


def rails(h, summary, done):
    p = Project(h, 'rails-ledger', 'runtime-rails-')
    summary['rails_app'] = p.app
    counts = {'database': 1, 'web': 2, 'worker': 1}
    db = database('rails_ledger', 'rails_ledger')
    app_service = {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
                   'resources': dict(LIMITS),
                   'env': {'RAILS_ENV': 'production', 'DB_HOST': 'database',
                           'DB_NAME': 'rails_ledger', 'DB_USER': 'rails_ledger',
                           'ALLOWED_HOSTS': p.host + ',' + p.alias},
                   'secrets': {'DB_PASSWORD': 'app-password', 'SECRET_KEY_BASE': 'secret-key-base'}}
    web, worker = copy.deepcopy(app_service), copy.deepcopy(app_service)
    web.update(command=['bundle', 'exec', 'puma', '-C', 'config/puma.rb'], replicas=2,
               http={'port': 3000, 'health': '/readyz', 'domains': [p.alias]})
    worker['command'] = ['bin/jobs']
    manifest = {'app': p.app, 'services': {'database': db, 'web': web, 'worker': worker},
                'release': {'service': 'web', 'command': ['bin/release']}}
    password = secrets.token_urlsafe(36).encode()
    p.stage('database-password', password)
    p.stage('app-password', password)
    p.stage('secret-key-base', secrets.token_hex(64).encode())
    # There are no implicit dependencies. Excluding the release runner from a
    # scoped deploy is invalid, so bootstrap with no release directive at all.
    p.save({'app': p.app, 'services': {'database': db}})
    p.cli('deploy')
    _, bootstrap = p.snapshot({'database': 1})
    require(not any(bootstrap['database'][0]['ports'].values()), 'database publicly published')
    p.save(manifest)
    p.cli('deploy')
    status, current = p.stable(bootstrap, counts, ['database'])
    hook, hook_info, _ = p.release(status)
    require(current['web'][0]['image'] == current['web'][1]['image'] == current['worker'][0]['image']
            == hook_info['image'], 'web/worker/release did not share the captured build')
    finished = datetime.fromisoformat(hook_info['finished'].replace('Z', '+00:00'))
    for name in ('web', 'worker'):
        for info in current[name]:
            require(datetime.fromisoformat(info['created'].replace('Z', '+00:00')) >= finished,
                    'runtime created before release completed')
    volume = status['volumes'][0]['id']
    mount = next(m for m in current['database'][0]['mounts']
                 if m['Destination'] == '/var/lib/postgresql/data')
    require(mount['Name'] == 'vamos-vol-' + volume, 'PostgreSQL volume identity mismatch')
    for name in ('web', 'worker'):
        require(all(not info['mounts'] for info in current[name]), 'stateless app has mounts')
    networks = current['database'][0]['networks']
    require(len(networks) == 1 and 'database' in next(iter(networks.values()))['Aliases'],
            'private PostgreSQL DNS alias missing')

    def sql(statement):
        return p.cli('exec', '--service', 'database', '--', 'psql', '-U', 'rails_ledger',
                     '-d', 'rails_ledger', '-Atc', statement).stdout.decode().strip()

    versions = sql('SELECT version FROM schema_migrations ORDER BY version').splitlines()
    expected = sorted(f.name.split('_')[0] for f in (p.directory / 'db/migrate').glob('*.rb'))
    require(versions == expected, 'release did not apply all application/queue migrations')
    require(sql("SELECT count(*) FROM pg_tables WHERE schemaname='public' AND "
                "tablename IN ('entries','solid_queue_jobs','solid_queue_ready_executions')") == '3',
            'application and Solid Queue tables missing')
    done('Rails release gates all runtime creation; shared image, Docker resources and private DB')
    rails_database_tests(p)
    p.stable(current, counts)
    done('Rails database-backed test suite passes in a separate disposable test database')

    row = json.loads(h.https(p.host, '/entries', {'description': '  coffee   beans  ', 'amount': -25}))
    path = '/entries/' + str(row['id'])
    processed = wait_processed(lambda: json.loads(h.https(p.host, path)))
    require(processed['normalized_description'] == 'COFFEE BEANS' and processed['amount'] == -25,
            'Solid Queue produced incorrect durable effect')

    def replicas_read(status):
        markers = set()
        for instance in status['services']['web']['instances']:
            port = instance['port']
            info = p.inspect(instance['container'])
            require(info['ports']['3000/tcp'] == [{'HostIp': '127.0.0.1', 'HostPort': port}],
                    'web replica is not bound exclusively to its loopback port')
            h.command(['curl', '--fail-with-body', '--silent', '--show-error', '--max-time', '30',
                       '--noproxy', '*', f'http://127.0.0.1:{port}/readyz'], user='root')
            marker, found = parse_http(h.command([
                'curl', '--fail-with-body', '--silent', '--show-error', '--max-time', '30',
                '--noproxy', '*', '--include', '--header', 'Host: ' + p.host,
                '--header', 'X-Forwarded-Proto: https', f'http://127.0.0.1:{port}{path}'
            ], user='root').stdout)
            require(found == processed, 'replica cannot read shared durable row')
            markers.add(marker)
        require(len(markers) == 2, 'web replicas share a process marker')
        require(processed['worker_marker'] not in markers, 'job executed inside a web process')
        return markers

    def public_read(markers):
        seen = set()
        for _ in range(32):
            marker, found = parse_http(h.command([
                'curl', '--fail-with-body', '--silent', '--show-error', '--max-time', '30',
                '--noproxy', '*', '--cacert', '/var/lib/caddy/data/caddy/pki/authorities/local/root.crt',
                '--resolve', f'{p.host}:443:127.0.0.1', '--include', f'https://{p.host}{path}'
            ], user='root').stdout)
            require(found == processed and marker in markers, 'public route reached unexpected runtime/data')
            seen.add(marker)
            if seen == markers:
                break
        require(seen == markers, 'verified HTTPS did not reach every web replica in 32 requests')
        require(json.loads(h.https(p.alias, path)) == processed, 'public alias lost durable data')

    markers = replicas_read(status)
    public_read(markers)
    # No arbitrary selection is permitted for multi-instance services.
    p.cli('exec', '--service', 'web', '--', 'true', error='--replica')
    p.cli('logs', '--service', 'web', error='--replica')
    # console checks terminals before resolving replicas. Give it a real PTY,
    # rather than mistaking the pipe/TTY refusal for replica ambiguity.
    console = [h.root / 'bin/vamos', 'console', '--server', 'local', '--app', p.app,
               '--service', 'web', '--shell', '/bin/sh']
    refused = h.command(['script', '--quiet', '--return', '--command',
                         shlex.join(map(str, console)), '/dev/null'],
                        cwd=p.directory, input=b'', check=False, timeout=30)
    require(refused.returncode != 0 and b'--replica' in refused.stdout + refused.stderr,
            'console did not require a replica')
    for ordinal, info in enumerate(current['web'], 1):
        terminal = h.command(['script', '--quiet', '--return', '--command',
                              shlex.join(map(str, console + ['--replica', str(ordinal)])), '/dev/null'],
                             cwd=p.directory,
                             input=b'ruby -rsocket -e \'puts "__CONSOLE_HOST__#{Socket.gethostname}__"\'\nexit\n',
                             timeout=30)
        require(('__CONSOLE_HOST__' + info['hostname'] + '__').encode() in terminal.stdout,
                'console selected wrong replica')
        selected = p.cli('exec', '--service', 'web', '--replica', str(ordinal), '--',
                         'ruby', '-rsocket', '-e', 'puts Socket.gethostname').stdout.decode().strip()
        require(selected == info['hostname'], 'exec selected wrong replica')
        token = 'replica-log-' + secrets.token_hex(8)
        p.cli('exec', '--service', 'web', '--replica', str(ordinal), '--', 'ruby', '-e',
              f'File.open("/proc/1/fd/1", "w") {{ |f| f.puts "{token}" }}')
        require(token.encode() in p.cli('logs', '--service', 'web', '--replica', str(ordinal)).stdout,
                'selected replica logs missing marker')
        other = 3 - ordinal
        require(token.encode() not in p.cli('logs', '--service', 'web', '--replica', str(other)).stdout,
                'replica logs leaked across selection')
    done('all replicas ready over loopback and verified HTTPS; durable Solid Queue effect; replica exec/logs')

    p.stage('database-password', secrets.token_urlsafe(36).encode())
    pending = next(s for s in p.status()['secrets'] if s['name'] == 'database-password')
    require(pending['pending'], 'new DB secret version not pending')
    worker['env']['RUNTIME_REVISION'] = 'worker-two'
    del manifest['release']  # A scoped worker deployment must not select web's hook.
    p.save(manifest)
    p.cli('deploy', '--service', 'worker')
    status, updated = p.stable(current, counts, ['database', 'web'])
    require(updated['worker'][0]['id'] != current['worker'][0]['id'], 'worker did not update')
    require(not status.get('release'), 'scoped worker deployment inherited a release hook')
    require(next(s for s in status['secrets'] if s['name'] == 'database-password') == pending,
            'manual database consumed pending secret version')
    public_read(markers)
    done('scoped worker replacement leaves web and manually pinned DB/secret untouched')

    # A whole-manifest deploy must refuse a changed protected DB secret, not
    # consume it. Omit DB from subsequent application-only manifests so its
    # effective state/pins survive while BOTH web and worker remain selected.
    p.cli('deploy', error='protected')
    p.stable(updated, counts)
    del manifest['services']['database']

    # Persist a count from the failed runner: a second execution is observable,
    # unlike checking only an error message or an unchanged release operation ID.
    sentinel = 'runtime-release-failed-' + secrets.token_hex(8)
    script = ('c = ActiveRecord::Base.connection; '
              'c.execute("CREATE TABLE IF NOT EXISTS runtime_release_attempts (id integer)"); '
              'c.execute("INSERT INTO runtime_release_attempts VALUES (1)"); '
              f'puts "{sentinel}"; exit 23')
    manifest['release'] = {'service': 'web', 'command': ['bin/rails', 'runner', script]}
    web['env']['RUNTIME_REVISION'] = 'blocked-web'
    worker['env']['RUNTIME_REVISION'] = 'blocked-worker'
    p.save(manifest)
    p.cli('deploy', error='exit code 23')
    failed, _ = p.stable(updated, counts)
    failed_hook, failed_info, logs = p.release(failed, 23)
    require(logs.count(sentinel.encode()) == 1, 'failed release logs missing/duplicated')
    require(sql('SELECT count(*) FROM runtime_release_attempts') == '1', 'release executed more than once')
    public_read(markers)
    p.cli('resume', failed['operation'], error='exit code 23')
    resumed, _ = p.stable(updated, counts)
    again, again_info, again_logs = p.release(resumed, 23)
    require(again == failed_hook and again_info == failed_info and again_logs == logs,
            'resume changed/restarted failed release')
    require(sql('SELECT count(*) FROM runtime_release_attempts') == '1', 'resume reran failed release')
    public_read(markers)
    done('failed operation-scoped release preserves ALL Docker IDs/routes; resume cannot rerun')

    manifest['release'] = {'service': 'web', 'command': ['bin/release']}
    p.save(manifest)
    p.cli('deploy', '--supersede', failed['operation'])
    status, replaced = p.stable(updated, counts, ['database'])
    new_hook, new_hook_info, _ = p.release(status)
    finished = datetime.fromisoformat(new_hook_info['finished'].replace('Z', '+00:00'))
    require(new_hook['operation'] != failed_hook['operation'], 'supersede reused failed release')
    for name in ('web', 'worker'):
        require(not ({v['id'] for v in updated[name]} & {v['id'] for v in replaced[name]}),
                'explicit supersede did not replace all selected runtimes')
        for info in replaced[name]:
            require(datetime.fromisoformat(info['created'].replace('Z', '+00:00')) >= finished,
                    'superseding runtime created before release completed')
    public_read(replicas_read(status))
    require(status['volumes'][0]['id'] == volume, 'deployment changed PostgreSQL volume')
    require(next(s for s in status['secrets'] if s['name'] == 'database-password') == pending,
            'full deployment consumed manual DB secret')
    p.destroy(volume)
    summary['rails_retained_volume'] = volume
    done('explicit supersede succeeds, data/aliases survive, cleanup/destroy retains volume')


def python_ledger(h, summary, done):
    """Small generic check: existing Python image, same release/replica contract."""
    p = Project(h, 'ledger', 'runtime-python-')
    summary['python_app'] = p.app
    db = database('ledger', 'ledger')
    password = secrets.token_urlsafe(36).encode()
    p.stage('database-password', password)
    p.stage('app-password', password)
    p.save({'app': p.app, 'services': {'database': db}})
    p.cli('deploy')
    _, before = p.snapshot({'database': 1})
    web = {'build': {'context': '.', 'dockerfile': 'Dockerfile'}, 'replicas': 2,
           'resources': dict(LIMITS), 'env': {'HOST': 'database', 'DB': 'ledger', 'USER': 'ledger'},
           'secrets': {'PASSWORD': 'app-password'}, 'http': {'port': 8080, 'health': '/readyz'}}
    p.save({'app': p.app, 'services': {'database': db, 'web': web},
            'release': {'service': 'web', 'command': ['./bin/migrate']}})
    p.cli('deploy')
    status, _ = p.stable(before, {'database': 1, 'web': 2}, ['database'])
    _, _, logs = p.release(status)
    require(b'Ledger schema initialized.' in logs, 'Python migration release did not complete')
    row = json.loads(h.https(p.host, '/entries', {'amount_cents': 173, 'description': p.app}))
    for instance in status['services']['web']['instances']:
        body = h.command(['curl', '--fail-with-body', '--silent', '--show-error', '--max-time', '30',
                          '--noproxy', '*', f'http://127.0.0.1:{instance["port"]}/entries'], user='root').stdout
        require(row in json.loads(body), 'Python replica lost shared PostgreSQL row')
    require(row in json.loads(h.https(p.host, '/entries')), 'Python HTTPS lost shared row')
    volume = status['volumes'][0]['id']
    p.destroy(volume)
    summary['python_retained_volume'] = volume
    done('generic Python ledger release/replicas/resources/HTTPS and retained-volume destroy')
