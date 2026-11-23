"""Exercise an actual scheduled tick in the owned Linux guest, not the workstation."""
import json
import time
from pathlib import Path

from build_network import IMAGE
from linux_suite import require_guest


def run(h, owner):
    require_guest(owner)
    h.command(['go', 'run', './cmd/build'])
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost', 'local', 'root@127.0.0.1')
    app = h.work / 'jobcheck'
    app.mkdir()
    literal = 'two words $HOME %n'
    worker = {
        'image': IMAGE, 'replicas': 2,
        'command': ['python', '-c', "import os,time; os.mkfifo('/tmp/tick'); time.sleep(3600)"],
        'jobs': {'marker': {'schedule': {'every': '1m'}, 'command': [
            'python', '-c', "import sys; open('/tmp/tick','w').write(sys.argv[1])", literal]}},
    }
    worker['jobs']['failure'] = {'schedule': {'every': '1m'}, 'command': ['python', '-c',
        "import sys; print('x'*40000,flush=True); print('deliberate error',file=sys.stderr); sys.exit(7)"]}
    manifest = {'app': 'jobcheck', 'services': {'worker': worker}}
    (app / 'vamos.yaml').write_text(json.dumps(manifest))
    h.command(['chown', '-R', 'vamostest:vamostest', app], user='root')
    h.cli('deploy', '--server', 'local', cwd=app)
    definitions = h.status('jobcheck')['jobs']
    if {job['name'] for job in definitions if job['next']} != {'marker', 'failure'}:
        raise AssertionError(definitions)
    result = h.cli('exec', '--server', 'local', '--service', 'worker', '--replica', '1', '--',
                   'timeout', '85', 'cat', '/tmp/tick', cwd=app)
    if result.stdout.decode() != literal:
        raise AssertionError('scheduled command did not deliver the literal argv to replica one')
    # FIFO delivery precedes the worker's durable completion record.
    deadline = time.monotonic() + 15
    while True:
        runs = json.loads(h.cli('jobs', '--service', 'worker', '--json', cwd=app).stdout)
        completed = {r['job']: r for r in runs if r['outcome'] in ('succeeded', 'failed')}
        if set(completed) == {'marker', 'failure'}:
            break
        if time.monotonic() >= deadline:
            raise AssertionError(f'missing completed attempts: {runs}')
        time.sleep(0.25)
    if completed['marker']['outcome'] != 'succeeded' or any('output' in r for r in runs):
        raise AssertionError('unexpected inbox summary')
    failed = json.loads(h.cli('jobs', 'show', completed['failure']['id'], '--json', cwd=app).stdout)
    if failed['exit_code'] != 7 or not failed['truncated'] or not failed['output'].endswith('deliberate error\n'):
        raise AssertionError('missing exit code, truncation marker, or final stderr')
    del worker['jobs']
    (app / 'vamos.yaml').write_text(json.dumps(manifest))
    h.cli('deploy', '--server', 'local', cwd=app)
    if h.status('jobcheck').get('jobs'):
        raise AssertionError('removed schedule still present')
    if json.loads(h.cli('jobs', 'show', failed['id'], '--json', cwd=app).stdout)['exit_code'] != 7:
        raise AssertionError('removing the schedule erased service history')
    h.cli('destroy', '--server', 'local', '--app', 'jobcheck', '--yes')
    if json.loads(h.cli('jobs', '--json', cwd=app).stdout):
        raise AssertionError('destroyed app retained readable job history')
    root = Path('/var/lib/vamos/job-results')
    if list(root.iterdir()) != [root / '.lock']:
        raise AssertionError('destroy did not purge job output')
