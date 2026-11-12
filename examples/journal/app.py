"""Unauthenticated persistence demo; not a production HTTP server."""
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
from http.server import BaseHTTPRequestHandler, HTTPServer

UID = GID = 10001
DATA = Path('/data')


def initialize_permissions(root):
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode):
        raise RuntimeError('data mount must be a directory, not a symlink')
    if (info.st_uid, info.st_gid) == (0, 0) and not any(root.iterdir()):
        os.chown(root, UID, GID)  # Only the new empty mount root, never descendants.
        os.chmod(root, 0o700)
    for path in [root, *root.rglob('*')]:
        info = path.lstat()
        needed = 0o700 if stat.S_ISDIR(info.st_mode) else 0o600
        if (stat.S_ISLNK(info.st_mode)
                or not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode))
                or (info.st_uid, info.st_gid) != (UID, GID)
                or info.st_mode & needed != needed):
            raise RuntimeError(f'incompatible existing permissions/type: {path}')


def initialize(root):
    (root / 'uploads').mkdir(exist_ok=True)
    with sqlite3.connect(root / 'journal.sqlite') as db:
        db.execute('CREATE TABLE IF NOT EXISTS entries (id INTEGER PRIMARY KEY, text TEXT NOT NULL)')


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, value, content_type='application/json'):
        body = json.dumps(value).encode() if content_type == 'application/json' else value
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ('/readyz', '/entries'):
            with sqlite3.connect(DATA / 'journal.sqlite') as db:
                rows = db.execute('SELECT id, text FROM entries ORDER BY id').fetchall()
            self.reply(200, {'ready': True} if self.path == '/readyz' else
                       [{'id': i, 'text': text} for i, text in rows])
        elif re.fullmatch(r'/uploads/[A-Za-z0-9_-]{1,64}', self.path):
            path = DATA / self.path.lstrip('/')
            if path.is_file():
                self.reply(200, path.read_bytes(), 'application/octet-stream')
            else:
                self.reply(404, {'error': 'not found'})
        else:
            self.reply(404, {'error': 'not found'})

    def do_POST(self):
        self.write_request()

    def do_PUT(self):
        self.write_request()

    def write_request(self):
        try:
            size = int(self.headers.get('Content-Length', '-1'))
            if not 0 <= size <= 1024 * 1024 or self.headers.get('Transfer-Encoding'):
                raise ValueError('Content-Length required; maximum 1 MiB')
            body = self.rfile.read(size)
            if len(body) != size:
                raise ValueError('incomplete body')
            if self.command == 'POST' and self.path == '/entries':
                value = json.loads(body)
                if not isinstance(value, dict) or not isinstance(value.get('text'), str):
                    raise ValueError('expected JSON object with text string')
                with sqlite3.connect(DATA / 'journal.sqlite') as db:
                    cursor = db.execute('INSERT INTO entries(text) VALUES (?)', (value['text'],))
                self.reply(201, {'id': cursor.lastrowid, 'text': value['text']})
            elif self.command == 'PUT' and re.fullmatch(r'/uploads/[A-Za-z0-9_-]{1,64}', self.path):
                target = DATA / self.path.lstrip('/')
                temporary = target.with_suffix('.tmp')
                with temporary.open('wb') as file:
                    file.write(body)
                    file.flush()
                    os.fsync(file.fileno())
                temporary.replace(target)
                self.reply(201, {'name': target.name, 'bytes': size})
            else:
                self.reply(404, {'error': 'not found'})
        except (ValueError, UnicodeDecodeError) as error:
            self.reply(400, {'error': str(error)})


if __name__ == '__main__':
    if os.geteuid() != 0:
        raise RuntimeError('entrypoint must initialize the mount as root')
    os.umask(0o077)
    initialize_permissions(DATA)
    os.setgroups([])
    os.setgid(GID)
    os.setuid(UID)
    initialize(DATA)
    HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
