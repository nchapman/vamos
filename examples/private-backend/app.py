"""Small stateless example: process readiness is separate from dependency health."""

import json
import os
import signal
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

role = sys.argv[1]
if role not in {"web", "backend"}:
    raise SystemExit("expected web or backend")
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok\n")
            return

        result = {"service": role, "message": "Hello from Vamos"}
        if role == "web":
            try:
                # Resolve service DNS on each request; reconnect after replacement.
                url = os.environ["BACKEND_URL"]
                with urllib.request.urlopen(url, timeout=2) as response:
                    result["backend"] = json.loads(response.read(4096))
            except (OSError, ValueError, KeyError):
                self.send_response(503)
                self.end_headers()
                self.wfile.write(b"Backend unavailable; retry shortly.\n")
                return

        body = json.dumps(result).encode() + b"\n"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


HTTPServer(("0.0.0.0", 8080 if role == "web" else 9000), Handler).serve_forever()
