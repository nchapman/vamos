"""Exercise an actual scheduled tick in the owned Linux guest, not the workstation."""
import json

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
    manifest = {'app': 'jobcheck', 'services': {'worker': worker}}
    (app / 'vamos.yaml').write_text(json.dumps(manifest))
    h.command(['chown', '-R', 'vamostest:vamostest', app], user='root')
    h.cli('deploy', '--server', 'local', cwd=app)
    job, = h.status('jobcheck')['jobs']
    if job['name'] != 'marker' or not job['next']:
        raise AssertionError(job)
    result = h.cli('exec', '--server', 'local', '--service', 'worker', '--replica', '1', '--',
                   'timeout', '85', 'cat', '/tmp/tick', cwd=app)
    if result.stdout.decode() != literal:
        raise AssertionError('scheduled command did not deliver the literal argv to replica one')
    del worker['jobs']
    (app / 'vamos.yaml').write_text(json.dumps(manifest))
    h.cli('deploy', '--server', 'local', cwd=app)
    if h.status('jobcheck').get('jobs'):
        raise AssertionError('removed schedule still present')
