"""Small unauthenticated demonstration API; startup never changes the schema."""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_BODY = 4096


def connect():
    # Lazy import lets validation/HTTP unit tests run without third-party packages.
    import psycopg
    return psycopg.connect(host=os.environ['HOST'], dbname=os.environ['DB'],
                           user=os.environ['USER'], password=os.environ['PASSWORD'],
                           connect_timeout=3, options='-c statement_timeout=5000')


def validate_entry(value):
    if not isinstance(value, dict) or set(value) != {'amount_cents', 'description'}:
        raise ValueError('expected amount_cents and description only')
    amount, description = value['amount_cents'], value['description']
    if type(amount) is not int or not -(2**63) <= amount < 2**63:
        raise ValueError('amount_cents must be a signed 64-bit integer')
    if not isinstance(description, str) or not description.strip() or len(description) > 500 or '\x00' in description:
        raise ValueError('description must contain 1–500 characters, without NUL')
    return amount, description


def query(path, entry=None):
    with connect() as conn:
        if path == '/readyz':
            conn.execute('SELECT id, amount_cents, description FROM entries LIMIT 0')
            return {'status': 'ok'}
        if entry is not None:
            row = conn.execute(
                'INSERT INTO entries (amount_cents, description) VALUES (%s, %s) RETURNING id, amount_cents, description',
                entry).fetchone()
            return dict(zip(('id', 'amount_cents', 'description'), row))
        rows = conn.execute('SELECT id, amount_cents, description FROM entries ORDER BY id DESC LIMIT 100').fetchall()
        return [dict(zip(('id', 'amount_cents', 'description'), row)) for row in rows]


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *args):
        pass  # Do not log request bodies, paths, credentials, or DB exceptions.

    def reply(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Connection', 'close')
        self.end_headers()
        self.close_connection = True
        self.wfile.write(data)

    def do_GET(self):
        self.handle_request(False)

    def do_POST(self):
        self.handle_request(True)

    def handle_request(self, post):
        if self.path not in (('/entries',) if post else ('/entries', '/readyz')):
            return self.reply(404, {'error': 'not found'})
        entry = None
        if post:
            try:
                lengths = self.headers.get_all('Content-Length', [])
                if self.headers.get('Transfer-Encoding') or len(lengths) != 1:
                    raise ValueError('one Content-Length is required; chunked bodies unsupported')
                size = int(lengths[0])
                if not 0 < size <= MAX_BODY:
                    return self.reply(413, {'error': 'body must be 1–4096 bytes'})
                if self.headers.get_content_type() != 'application/json':
                    return self.reply(415, {'error': 'expected application/json'})
                body = self.rfile.read(size)
                if len(body) != size:
                    raise ValueError('incomplete body')
                entry = validate_entry(json.loads(body))
            except (ValueError, UnicodeError):
                return self.reply(400, {'error': 'invalid entry or request body'})
            except (TimeoutError, OSError):
                return self.reply(408, {'error': 'request timed out'})
        try:
            result = query(self.path, entry)
        except Exception:
            # Never expose driver diagnostics (which may include connection details).
            return self.reply(503, {'error': 'database unavailable or schema not initialized'})
        self.reply(201 if post else 200, result)


if __name__ == '__main__':
    ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
