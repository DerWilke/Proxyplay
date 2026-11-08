#!/usr/bin/env python3
"""Spiele-Stream web page: serves index.html, /health and downloads (/dl/<file> from DATA_DIR/dl).
The /api/ path is routed by the reverse proxy straight to the agent on the gaming PC.
For local tests, API_UPSTREAM=http://127.0.0.1:8771 makes this server proxy /api/ itself."""
import os, shutil, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, unquote

DATA = os.environ.get('DATA_DIR', '/data')
INDEX = os.environ.get('INDEX_FILE', '/app/index.html')
PORT = int(os.environ.get('PORT', '8080'))
UP = os.environ.get('API_UPSTREAM', '')


class H(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *a):
        pass

    def send_bytes(self, b, ctype, code=200):
        self.send_response(code)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(b)))
        self.send_header('Cache-Control', 'no-store')
        self.end_headers()
        self.wfile.write(b)

    def proxy(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n) if n else None
        req = urllib.request.Request(UP + self.path, data=body, method=self.command,
                                     headers={'Content-Type': self.headers.get('Content-Type', 'application/octet-stream')})
        try:
            r = urllib.request.urlopen(req, timeout=3600)
        except urllib.error.HTTPError as e:
            r = e
        self.send_response(r.status)
        cl = r.headers.get('Content-Length')
        for k in ('Content-Type', 'Cache-Control'):
            if r.headers.get(k):
                self.send_header(k, r.headers[k])
        if cl:
            self.send_header('Content-Length', cl)
            self.end_headers()
            self.wfile.write(r.read())
            return
        self.send_header('Transfer-Encoding', 'chunked')
        self.end_headers()
        try:
            while True:
                c = r.read1(65536)
                if not c:
                    break
                self.wfile.write(b'%x\r\n%s\r\n' % (len(c), c))
                self.wfile.flush()
            self.wfile.write(b'0\r\n\r\n')
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        p = urlparse(self.path).path
        if UP and p.startswith('/api/'):
            return self.proxy()
        if p in ('/', '/index.html'):
            return self.send_bytes(open(INDEX, 'rb').read(), 'text/html; charset=utf-8')
        if p == '/health':
            return self.send_bytes(b'{"ok":true}', 'application/json')
        if p.startswith('/dl/'):
            name = os.path.basename(unquote(p[4:]))
            f = os.path.join(DATA, 'dl', name)
            if name and os.path.isfile(f):
                self.send_response(200)
                self.send_header('Content-Type', 'application/zip' if name.endswith('.zip') else 'application/octet-stream')
                self.send_header('Content-Length', str(os.path.getsize(f)))
                self.send_header('Content-Disposition', 'attachment; filename="%s"' % name)
                self.end_headers()
                with open(f, 'rb') as fh:
                    try:
                        shutil.copyfileobj(fh, self.wfile, 1 << 20)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                return
        self.send_bytes(b'not found', 'text/plain', 404)

    def do_POST(self):
        if UP and self.path.startswith('/api/'):
            return self.proxy()
        self.send_bytes(b'not found', 'text/plain', 404)


if __name__ == '__main__':
    os.makedirs(os.path.join(DATA, 'dl'), exist_ok=True)
    ThreadingHTTPServer(('', PORT), H).serve_forever()
