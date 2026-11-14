"""Late owned-guest plan-only acceptance; no capture, schedules or auth proof."""
import copy
import json
import os
from pathlib import Path
import re
import secrets
import shutil

from linux_suite import Harness, OWNER, ROOT, require_guest
from runtime_scenarios import database, instances
from scenarios import require


def unchanged(before, after, label):
    require(before == after, label + ' changed')


def private_root(path, mode):
    stat = path.stat()
    require(stat.st_uid == 0 and stat.st_mode & 0o777 == mode,
            'unsafe retained plan permissions')


def validate_bindings(record, status, repository_id):
    bindings = record['bindings']
    require(set(bindings['sources']) == {'primary', 'entries', 'uploads'}, 'source coverage')
    for source, service in [('primary', 'database'), ('entries', 'web'), ('uploads', 'web')]:
        binding = bindings['sources'][source]
        require(binding['service'] == service and binding['incarnation'] == status['services'][service]['incarnation'], 'source incarnation')
    primary = bindings['sources']['primary']
    require(primary['credential_service'] == 'database' and primary['credential_incarnation'] == status['services']['database']['incarnation'], 'credential binding')
    web = bindings['sources']['entries']['volume']
    unchanged(web, bindings['sources']['uploads']['volume'], 'shared volume binding')
    require(web['name'] == 'data' and web['mount'] == '/data', 'web volume selection')
    require(any(v['id'] == web['id'] and v['service'] == 'web' and not v['missing'] for v in status['volumes']), 'unregistered volume')
    unchanged(bindings['stops'], {'web': status['services']['web']['incarnation']}, 'stop binding')
    require(bindings['repositories']['fixture-new']['id'] == repository_id, 'repository pin')
    require(re.fullmatch('[0-9a-f]{64}', bindings['repositories']['fixture-new']['config_digest']), 'repository config pin')
    require(re.fullmatch('[0-9a-f]{64}', record['revision']), 'plan revision')


def fixture_manifest(app):
    postgres = database('plan_db', 'plan_user')
    postgres['volume']['name'] = 'postgres-data'
    return {'app': app, 'services': {
        'database': postgres,
        'web': {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
                'volume': {'name': 'data', 'mount': '/data'},
                'http': {'port': 8080, 'health': '/readyz'}}}}


def run(h, repository_id, repository_credentials):
    # Recheck the exact existing runner's guards, even when imported directly.
    require_guest(OWNER.read_text().strip())
    require(isinstance(h, Harness) and h.root == ROOT and h.work == ROOT / '.integration-work', 'requires existing main harness')
    app = 'plan-' + secrets.token_hex(8)
    work = h.work / app
    shutil.copytree(h.root / 'examples/journal', work,
                    ignore=shutil.ignore_patterns('.soar', '__pycache__'))
    work.chmod(0o700)
    values = [v.encode() for k, v in repository_credentials.items() if k in ('password', 'access_key_id', 'secret_access_key') and v]
    password, staged = secrets.token_urlsafe(36).encode(), secrets.token_urlsafe(36).encode()
    values += [password, staged]

    def command(argv, **kwargs):
        result = h.command(argv, check=False, **kwargs)
        require(all(v not in result.stdout + result.stderr for v in values), 'secret exposed in plan phase output')
        return result

    def cli(*args, success=True, input=None):
        result = command([h.root / 'bin/soar', *args], cwd=work, input=input)
        require((result.returncode == 0) == success, 'plan phase CLI unexpected exit status')
        return result.stdout

    def save(name, data):
        path = work / name
        path.write_text(json.dumps(data, indent=2))  # JSON is a YAML subset.
        path.chmod(0o600)
        os.chown(path, h.user.pw_uid, h.user.pw_gid)

    manifest = fixture_manifest(app)
    result = command(['chown', '-R', 'soartest:soartest', work], user='root')
    require(result.returncode == 0, 'fixture ownership')
    save('soar.yaml', manifest)
    def stage(value):
        cli('secrets', 'stage', '--server', 'local', '--app', app, '--stdin', 'database-password', input=value)
    def status():
        return json.loads(cli('status', '--server', 'local', '--app', app, '--json'))
    def snapshot():
        current = status()
        names = instances(current, {'web': 1, 'database': 1})
        result = {}
        # Never inspect Config.Env: it contains the applied password.
        fmt = '{"id":{{json .Id}},"started":{{json .State.StartedAt}},"running":{{json .State.Running}},"restarts":{{json .RestartCount}},"policy":{{json .HostConfig.RestartPolicy}},"mounts":{{json .Mounts}}}'
        for name, containers in names.items():
            inspected = command(['docker', 'inspect', '--format', fmt, containers[0]], user='root')
            require(inspected.returncode == 0, 'runtime inspect')
            result[name] = json.loads(inspected.stdout)
            require(result[name]['running'], 'runtime stopped')
        return current, result
    def show():
        return json.loads(cli('backup', 'plan', '--server', 'local', '--app', app, '--json'))
    def configure(plan, yes=False, success=True):
        save('backup.yaml', plan)
        output = cli('backup', 'configure', '--server', 'local', '--app', app, '-f', 'backup.yaml', '--json', *(['--yes'] if yes else []), success=success)
        return json.loads(output) if success else None

    stage(password)
    cli('deploy', '--server', 'local')
    state = json.loads((Path('/var/lib/soar/apps') / app / 'state.json').read_text())
    require(state['version'] in (7, 8), 'requires fixed-incarnation v7/v8 runtime')
    before, runtime = snapshot()
    plan = {'app': app, 'sources': {
        'primary': {'type': 'postgres', 'service': 'database', 'database': 'plan_db', 'user': 'plan_user', 'credentials': {'service': 'database', 'env': 'POSTGRES_PASSWORD'}, 'restore': {'profile': 'single-owner'}},
        'entries': {'type': 'sqlite', 'service': 'web', 'volume': 'data', 'path': 'journal.sqlite'},
        'uploads': {'type': 'files', 'service': 'web', 'volume': 'data', 'path': 'uploads'}},
        'sets': {'daily': {'sources': ['primary', 'entries', 'uploads'], 'consistency': 'quiesced', 'stop': ['web'], 'repository': 'fixture-new'}}}
    record = Path('/var/lib/soar/backups/plans') / (app + '.json')
    require(show()['current_revision'] == 'none' and not record.exists(), 'plan already installed')
    preview = configure(plan)
    require(not record.exists() and preview['current_revision'] == 'none', 'preview wrote plan')
    installed = configure(plan, yes=True)
    require(installed['applied'] and installed['current_revision'] == preview['digest'], 'CAS apply revision')
    private_root(record.parent, 0o700)
    private_root(record, 0o600)
    validate_bindings(installed['current'], before, repository_id)
    unchanged(json.loads(record.read_text()), installed['current'], 'persisted record')
    unchanged(runtime, snapshot()[1], 'runtime across plan installation')
    web_volume = installed['current']['bindings']['sources']['entries']['volume']['id']
    require(any(m.get('Name') == 'soar-vol-' + web_volume for m in runtime['web']['mounts']), 'actual web volume differs')
    baseline = record.read_bytes()
    for source, key, value in [('primary', 'service', 'absent'), ('entries', 'volume', 'absent'), ('primary', 'credentials', {'service': 'database', 'env': 'ABSENT'})]:
        invalid = copy.deepcopy(plan)
        invalid['sources'][source][key] = value
        configure(invalid, yes=True, success=False)
        unchanged(baseline, record.read_bytes(), 'refused replacement')
    for field in ('credentials', 'service'):
        invalid = copy.deepcopy(plan)
        del invalid['sources']['primary'][field]
        configure(invalid, yes=True, success=False)
        unchanged(baseline, record.read_bytes(), 'missing selector replacement')
    invalid = copy.deepcopy(plan)
    del invalid['sources']['entries']
    configure(invalid, yes=True, success=False)
    unchanged(baseline, record.read_bytes(), 'missing source replacement')
    # A full replacement changes policy, without removing historical data.
    plan['sets']['database-only'] = {'sources': ['primary'], 'consistency': 'online-per-source', 'repository': 'fixture-new'}
    replacement = configure(plan, yes=True)
    require(replacement['current_revision'] != installed['current_revision'], 'replacement did not revise plan')
    baseline = record.read_bytes()
    digest = configure(plan)['digest']
    old_secret = next(s for s in status()['secrets'] if s['name'] == 'database-password')
    require(old_secret.get('applied_versions', {}).get('database'), 'missing applied DB secret pin')
    stage(staged)
    pending = status()['secrets']
    new_secret = next(s for s in pending if s['name'] == 'database-password')
    unchanged(old_secret['applied_versions'], new_secret['applied_versions'], 'applied DB pins after staging')
    require(new_secret['staged_version'] != old_secret['staged_version'], 'staging did not create a new version')
    require(any(s['name'] == 'database-password' and s['pending'] for s in pending), 'password not pending')
    unchanged(digest, configure(plan)['digest'], 'staged-secret preview digest')
    unchanged(replacement['current_revision'], show()['current_revision'], 'staged-secret current revision')
    unchanged(runtime, snapshot()[1], 'runtime across staging/refusals/replacement')
    manifest['services']['web']['env'] = {'PLAN_ACCEPTANCE': 'compatible'}
    save('soar.yaml', manifest)
    cli('deploy', '--server', 'local', '--service', 'web')
    after, updated = snapshot()
    unchanged(runtime['database'], updated['database'], 'protected database')
    require(updated['web']['id'] != runtime['web']['id'], 'scoped deploy did not replace web runtime')
    require(any(m.get('Name') == 'soar-vol-' + web_volume for m in updated['web']['mounts']), 'scoped deploy changed actual web volume')
    unchanged(pending, after['secrets'], 'applied/pending secret provenance')
    validate_bindings(replacement['current'], after, repository_id)
    unchanged(digest, configure(plan)['digest'], 'compatible deploy digest')
    unchanged(baseline, record.read_bytes(), 'compatible deploy record')
    cli('destroy', '--server', 'local', '--yes')
    require(status()['phase'] == 'destroyed', 'owned app not destroyed')
    unchanged(replacement['current'], show()['current'], 'record after destroy')
    unchanged(baseline, record.read_bytes(), 'retained plan bytes')
    (h.artifacts / 'plan-summary.json').write_text(json.dumps({'passed': True, 'app': app, 'revision': replacement['current_revision'], 'checks': ['preview read-only', 'private CAS record', 'three source bindings', 'runtime unchanged', 'invalid replacement refused', 'full replacement', 'pending secret ignored', 'compatible scoped deploy', 'destroy retains plan'], 'limitations': 'Plan only: no schedules, capture, source-path existence or database authentication checked; no offsite pruning.'}, indent=2))
    h.log('Shared plan acceptance passed. No schedules/capture/authentication proof; plan and backups intentionally retained.')
