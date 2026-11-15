"""Guest-only secrets acceptance; invoked by scenarios after harness startup.

Only selected digest probes and narrow Docker metadata are recorded, never Env.
"""
import copy
import hashlib
import json
from pathlib import Path
import secrets

from runtime_scenarios import instances
from scenarios import require

INSPECT = '{"id":{{json .Id}},"image":{{json .Image}},"started":{{json .State.StartedAt}}}'
SELECTED = ('ONE', 'TWO', 'OTHER', 'MARKER')
PROBE = ('import hashlib,json,os,socket; print(json.dumps({"hostname":socket.gethostname(),'
         '"digests":{k:hashlib.sha256(os.environ[k].encode()).hexdigest() '
         'for k in ' + repr(SELECTED) + ' if k in os.environ}}))')
SERVER = '''import http.server,json,hashlib,os,socket
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  body=json.dumps({'hostname':socket.gethostname(),'digests':{k:hashlib.sha256(os.environ[k].encode()).hexdigest() for k in ('ONE','TWO','OTHER','MARKER') if k in os.environ}}).encode()
  self.send_response(200); self.end_headers(); self.wfile.write(body)
 def log_message(self,*args): pass
http.server.ThreadingHTTPServer(('0.0.0.0',8080),Handler).serve_forever()
'''


def digests(values):
    return {name: hashlib.sha256(value).hexdigest() for name, value in values.items()}


def payload(values):
    require(bool(values), 'empty secret request')
    if len(values) == 1:
        return next(iter(values.values()))
    require(all(b'\n' not in v and b'\r' not in v for v in values.values()), 'invalid batch value')
    return b''.join(name.encode() + b'=' + value + b'\n' for name, value in values.items())


def response(value, names, applied=False):
    require(value['version'] == 1, 'unexpected response version')
    entries = [value['secret']] if len(names) == 1 else value['secret_versions']
    require(len(entries) == len(names) and {e['name'] for e in entries} == set(names),
            'response secret membership differs')
    if applied:
        require(value.get('secret_applied') is True, 'set did not apply')


def compare(before, after, changed):
    require(before.keys() == after.keys(), 'service membership changed')
    for name in before:
        require(len(before[name]) == len(after[name]), 'replica count changed')
        for old, new in zip(before[name], after[name]):
            require(old['image'] == new['image'], 'immutable image changed')
            if name in changed:
                require(old['id'] != new['id'], 'consumer was not replaced')
            else:
                require(old == new, 'unselected container changed')


def secret_args(action, app, names):
    return ('secrets', action, '--stdin', '--json', '--server', 'local', '--app', app, *names)


def run(h):
    require(h.root == Path('/opt/vamos-test'), 'requires the owned guest harness')
    app = 'secretset-' + secrets.token_hex(4)
    directory = h.work / app
    directory.mkdir()
    summary = {'passed': False, 'checks': []}
    counts = {'web': 2, 'worker': 1, 'unrelated': 1, 'protected': 1}
    base = {'build': {'context': '.', 'dockerfile': 'Dockerfile'},
            'command': ['python', '/server.py'], 'env': {'MARKER': 'deployed'}}
    services = {name: copy.deepcopy(base) for name in counts}
    for name in ('web', 'worker'):
        services[name]['secrets'] = {'ONE': 'key-one', 'TWO': 'key-two', 'OTHER': 'other-key'}
    services['web'].update(replicas=2, http={'port': 8080, 'health': '/'})
    services['protected'].update(update='manual', secrets={'ONE': 'protected-key'})
    manifest = {'app': app, 'services': services}
    (directory / 'vamos.yaml').write_text(json.dumps(manifest))
    (directory / 'server.py').write_text(SERVER)
    (directory / 'Dockerfile').write_text('FROM python:3.13-slim\nCOPY server.py /server.py\n')
    h.command(['chown', '-R', 'vamostest:vamostest', directory], user='root')

    def cli(action, values, error=None):
        result = h.cli(*secret_args(action, app, values), cwd=h.work,
                       input=payload(values), expect_error=error)
        if error is None:
            response(json.loads(result.stdout), values, action == 'set')

    def snapshot():
        ids = instances(h.status(app), counts)
        return {name: [json.loads(h.command(['docker', 'inspect', '--format', INSPECT, cid],
                                           user='root').stdout) for cid in containers]
                for name, containers in ids.items()}

    def infrastructure():
        # Includes stopped release/build containers; no raw inspect or environment.
        ids = h.command(['docker', 'ps', '-aq', '--no-trunc'], user='root').stdout.decode().split()
        return {cid: json.loads(h.command(['docker', 'inspect', '--format', INSPECT, cid],
                                         user='root').stdout) for cid in ids}

    def verify(state, values):
        expected = digests({'ONE': values['key-one'], 'TWO': values['key-two'],
                            'OTHER': initial['other-key'], 'MARKER': b'deployed'})
        hosts = set()
        for name in ('web', 'worker'):
            for container in state[name]:
                result = json.loads(h.command(['docker', 'exec', container['id'], 'python', '-c', PROBE],
                                              user='root').stdout)
                require(result['digests'] == expected, 'runtime digests differ')
                hosts.add(result['hostname'])
        require(len(hosts) == 3, 'replica hostnames not distinct')

    initial = {name: secrets.token_urlsafe(32).encode() for name in
               ('key-one', 'key-two', 'other-key', 'protected-key')}
    deployed = False
    try:
        cli('stage', initial)
        h.cli('deploy', '--server', 'local', cwd=directory)
        deployed = True
        before = snapshot()
        verify(before, initial)
        cli('stage', {'other-key': secrets.token_urlsafe(32).encode()})
        other = next(s for s in h.status(app)['secrets'] if s['name'] == 'other-key')
        (directory / 'Dockerfile').write_text('THIS IS NOT A DOCKERFILE\n')
        for service in services.values():
            service['env']['MARKER'] = 'pending'
        (directory / 'vamos.yaml').write_text(json.dumps(manifest))
        infra = infrastructure()
        updated = {name: secrets.token_urlsafe(32).encode() for name in ('key-one', 'key-two')}
        cli('set', updated)
        journal = json.loads((Path('/var/lib/vamos/apps') / app / 'state.json').read_text())
        require(journal.get('version') == 8 and journal.get('SecretUpdate'),
                'missing generated v8 secret update journal')
        after = snapshot()
        compare(before, after, {'web', 'worker'})
        verify(after, updated)
        require(next(s for s in h.status(app)['secrets'] if s['name'] == 'other-key') == other,
                'unrelated secret pins changed')
        now = infrastructure()
        owned_before = {c['id'] for rows in before.values() for c in rows}
        owned_after = {c['id'] for rows in after.values() for c in rows}
        owned = owned_before | owned_after
        require({k: v for k, v in infra.items() if k not in owned} ==
                {k: v for k, v in now.items() if k not in owned},
                'builder/release or unrelated Docker state changed')
        summary['checks'].append('batch replacement, immutable images, deployed config, runtime digests and unrelated pins')
        # Deliberately public fixture strings, not credentials: inline arguments
        # are visible in local command logs/process listings by design. Keep all
        # randomly generated secret values on the private stdin path above.
        inline = {'key-one': b'public-integration-example=one',
                  'key-two': b'public integration example two'}
        result = h.cli('secrets', 'set', '--server', 'local', '--app', app, '--json',
                       *(name + '=' + value.decode() for name, value in inline.items()), cwd=h.work)
        response(json.loads(result.stdout), inline, True)
        require(all(value not in result.stdout + result.stderr for value in inline.values()),
                'CLI echoed inline fixture values')
        inline_after = snapshot()
        compare(after, inline_after, {'web', 'worker'})
        verify(inline_after, inline)
        after = inline_after
        summary['checks'].append('inline multiple NAME=VALUE inputs preserve literal values without CLI echo')
        cli('set', {'unused-key': secrets.token_urlsafe(32).encode()})
        compare(after, snapshot(), set())
        summary['checks'].append('unused single set succeeds without replacement')
        versions = h.status(app)['secrets']
        cli('set', {name: secrets.token_urlsafe(32).encode() for name in ('key-one', 'protected-key')},
            'protected by update: manual')
        require(h.status(app)['secrets'] == versions, 'refused batch staged secrets')
        compare(after, snapshot(), set())
        summary['checks'].append('manual batch refusal before staging')
        summary['passed'] = True
    finally:
        try:
            if deployed and summary['passed']:
                h.cli('destroy', '--server', 'local', '--app', app, '--yes', cwd=h.work)
                require(h.status(app).get('secrets'), 'destroy lost retained secrets')
        except Exception:
            summary['passed'] = False
            raise
        finally:
            (h.artifacts / 'secrets-set.json').write_text(json.dumps(summary, indent=2) + '\n')
