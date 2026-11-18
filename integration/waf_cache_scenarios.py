"""Real production-image HTTP middleware acceptance in a runner-owned guest."""
import json
import secrets
from pathlib import Path
from urllib.parse import quote

from linux_suite import CA
from firewall_scenarios import IMAGE

ORIGIN = '''from http.server import BaseHTTPRequestHandler, HTTPServer
import uuid
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        identity = str(uuid.uuid4())
        body = (identity + "\\n" + "origin response " * 200).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Cache-Control", "no-store" if self.path.startswith("/private") else "public, max-age=600")
        self.send_header("X-Origin-ID", identity)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'''


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def request(h, host, path, gzip=False, *, retry=True, forwarded_for=None):
    args = ['curl', '--silent', '--show-error', '--max-time', '30',
            '--noproxy', '*', '--cacert', str(CA), '--resolve',
            f'{host}:443:127.0.0.1', '--dump-header', '-',
            '--header', 'Accept-Encoding: ' + ('gzip' if gzip else 'identity')]
    if retry:
        args += ['--retry', '8', '--retry-connrefused', '--retry-delay', '1', '--retry-max-time', '30']
    if forwarded_for:
        args += ['--header', 'X-Forwarded-For: ' + forwarded_for]
    response = h.command(args + [f'https://{host}{path}'], user='root').stdout
    head, body = response.split(b'\r\n\r\n', 1)
    lines = head.decode().split('\r\n')
    status = int(lines[0].split()[1])
    headers = {k.lower(): v.strip() for line in lines[1:] for k, v in [line.split(':', 1)]}
    return status, headers, body


def cache_pair(h, host, path, enabled):
    first, second = request(h, host, path), request(h, host, path)
    require(first[0] == second[0] == 200, 'benign request failed')
    require((first[1]['x-origin-id'] == second[1]['x-origin-id']) == enabled,
            f'origin request identity does not match expected cache behavior: {first[1]} {second[1]}')
    if enabled:
        require('hit' in second[1].get('cache-status', '').lower(), f'cache did not report HIT: {second[1]}')
        require(first[2] == second[2], 'cache HIT body changed')
    return second


def ingress_rule_checks(h, host):
    require(request(h, host, '/', retry=False)[0] == 200, 'first request to exact root failed')
    require(request(h, host, '/', retry=False)[0] == 429, 'exact root limit did not activate')
    path = '/limited/' + secrets.token_hex(8)
    first, second = request(h, host, path, retry=False), request(h, host, path, retry=False)
    require(first[0] == second[0] == 200 and 'hit' in second[1].get('cache-status', '').lower(),
            f'rate-limit probe did not reach a warm cache: {first[:2]} {second[:2]}')
    denied = request(h, host, path, retry=False, forwarded_for='192.0.2.1')
    require(denied[0] == 429 and int(denied[1].get('retry-after', '0')) > 0,
            f'cache or forwarded header bypassed rate limit: {denied[:2]}')
    require(request(h, host, '/limited', retry=False)[0] == 200, 'wildcard matched its bare prefix')
    exact = request(h, host, '/old/special?original=private', retry=False)
    require(exact[0] == 301 and exact[1].get('location') == '/special' and 'x-origin-id' not in exact[1],
            f'exact redirect did not win or did not short-circuit the origin: {exact[:2]}')
    denied_redirect = request(h, host, '/OLD/special', retry=False)
    require(denied_redirect[0] == 429, f'redirect ran before rate limiting: {denied_redirect[:2]}')
    blocked = request(h, host, '/old/retired', retry=False)
    require(blocked[0] == 403 and 'location' not in blocked[1] and 'x-origin-id' not in blocked[1],
            f'block did not short-circuit the redirect and origin: {blocked[:2]}')
    prefix = request(h, host, '/old/page?original=private', retry=False)
    require(prefix[0] == 302 and prefix[1].get('location') == '/docs',
            f'prefix redirect copied path or query or used the wrong status: {prefix[:2]}')


def whole_site_budget_checks(h, hosts):
    # Probe distinct uncached paths from the same connection IP. Exhausting
    # the primary hostname must not spend any of its alias's budget.
    prefix = '/private/budget-' + secrets.token_hex(8)
    for host in hosts:
        for index in (1, 2):
            allowed = request(h, host, f'{prefix}/{index}', retry=False)
            require(allowed[0] == 200 and 'x-origin-id' in allowed[1],
                    f'{host}: request {index} did not reach the origin with a fresh budget: {allowed[:2]}')
        denied = request(h, host, f'{prefix}/3', retry=False)
        require(denied[0] == 429 and 'x-origin-id' not in denied[1],
                f'{host}: third distinct path did not hit the whole-site limit: {denied[:2]}')


def checks(h, state):
    hosts = state['hosts']
    for attack in ["1' OR '1'='1", '<script>alert(1)</script>']:
        path = '/attack?q=' + quote(attack, safe='')
        for service in ('default', 'cacheoff', 'compressionoff'):
            response = request(h, hosts[service], path)
            require(response[0] == 403, f'{service}: WAF failed to block {attack!r}: {response[0]}')
        require(request(h, hosts['wafoff'], path)[0] == 200, 'waf:false still blocked origin request')
    for service in hosts:
        cache_pair(h, hosts[service], '/cache/' + secrets.token_hex(8), service != 'cacheoff')
        cache_pair(h, hosts[service], '/private/' + secrets.token_hex(8), False)
        response = request(h, hosts[service], '/compression/' + secrets.token_hex(8), gzip=True)
        require(response[0] == 200, 'compression probe failed')
        require((response[1].get('content-encoding') == 'gzip') == (service != 'compressionoff'),
                f'{service}: compression toggle mismatch: {response[1]}')
    cache_files = [p for p in Path('/var/lib/caddy/data/cache').rglob('*') if p.is_file() and p.stat().st_size]
    require(cache_files, 'no nonempty cache files on persistent Caddy data volume')
    h.command(['docker', 'inspect', 'vamos-caddy'], user='root')
    h.command(['docker', 'logs', '--tail', '150', 'vamos-caddy'], user='root')
    h.log(f'WAF/cache/toggle probes passed; {len(cache_files)} nonempty disk-cache files')


def prepare(h):
    from build_network import configure_native
    from recovery import init_identity
    configure_native(h)
    h.cli('server', 'add', '--yes', '--firewall=false', '--domain', 'vamos.localhost',
          'local', 'root@127.0.0.1')
    app = 'http-' + secrets.token_hex(4)
    directory = h.work / app
    directory.mkdir()
    services = {}
    for name, feature in [('default', None), ('wafoff', 'waf'), ('cacheoff', 'cache'), ('compressionoff', 'compression')]:
        http = {'port': 8080}
        if feature:
            http[feature] = False
        else:
            http['rate_limit'] = [{'path': '/', 'requests': 1, 'window': '1m'},
                                  {'path': '/limited/*', 'requests': 2, 'window': '1m'},
                                  {'path': '/old/special', 'requests': 1, 'window': '1m'},
                                  {'path': '/old/retired', 'block': True}]
            http['redirects'] = [{'path': '/old/*', 'to': '/docs'},
                                 {'path': '/old/special', 'to': '/special', 'permanent': True}]
        services[name] = {'image': IMAGE, 'command': ['python', '-u', '-c', ORIGIN], 'http': http}
    hosts = {name: f'{app}-{name}.vamos.localhost' for name in services}
    alias = f'{app}-alias.vamos.localhost'
    services['budget'] = {
        'image': IMAGE, 'command': ['python', '-u', '-c', ORIGIN],
        'http': {'port': 8080, 'domains': [alias],
                 'rate_limit': [{'path': '/*', 'requests': 2, 'window': '5m'}]},
    }
    (directory / 'vamos.yaml').write_text(json.dumps({'app': app, 'services': services}))
    h.command(['chown', '-R', 'vamostest:vamostest', directory], user='root')
    h.cli('deploy', '--server', 'local', cwd=directory)
    state = {'app': app, 'hosts': hosts, 'budget_hosts': [f'{app}-budget.vamos.localhost', alias],
             'init': init_identity()}
    checks(h, state)
    ingress_rule_checks(h, hosts['default'])
    whole_site_budget_checks(h, state['budget_hosts'])
    state['persistent_path'] = '/persistent/' + secrets.token_hex(8)
    state['origin_id'] = cache_pair(h, state['hosts']['default'], state['persistent_path'], True)[1]['x-origin-id']
    (h.artifacts / 'waf-cache-checkpoint.json').write_text(json.dumps(state))


def verify_restart(h):
    from recovery import init_identity
    state = json.loads((h.artifacts / 'waf-cache-checkpoint.json').read_text())
    require(init_identity() != state['init'], 'machine restart did not occur')
    h.command(['systemctl', 'is-system-running', '--wait'], user='root', check=False, timeout=120)
    response = request(h, state['hosts']['default'], state['persistent_path'])
    require(response[0] == 200 and response[1].get('x-origin-id') == state['origin_id'],
            f'disk-cached response did not survive restart: {response[1]}')
    require('hit' in response[1].get('cache-status', '').lower(), 'persisted cache did not report HIT')
    checks(h, state)
    ingress_rule_checks(h, state['hosts']['default'])
    whole_site_budget_checks(h, state['budget_hosts'])
    (h.artifacts / 'waf-cache-summary.json').write_text(json.dumps({
        'passed': True,
        'verified': ['SQLi and XSS blocked', 'waf:false allows identical probes',
                     'cache HIT preserves origin identity and body', 'cache:false bypasses cache',
                     'no-store honored', 'gzip enabled by default and compression:false disables it',
                     'nonempty disk cache', 'cached response survives machine restart',
                     'rate limit counts cache hits and ignores spoofed forwarded IP',
                     'redirect precedence and fixed destinations', 'redirects are rate limited',
                     'exact root and prefix limits', 'blocked paths return 403 before redirects',
                     'whole-site budget spans paths and is independent for each hostname'],
    }, indent=2) + '\n')
