import http.server, socketserver, os, sys, hashlib, random
BLOCK = random.Random(42).randbytes(1 << 20)
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path.startswith("/det"):
            mb = int(self.path.split("mb=")[1].split("&")[0]) if "mb=" in self.path else 8
            self.send_response(200); self.send_header("Content-Length", str(mb << 20)); self.end_headers()
            for _ in range(mb): self.wfile.write(BLOCK)
        elif self.path.startswith("/big"):
            mb = int(self.path.split("mb=")[1].split("&")[0]) if "mb=" in self.path else 256
            self.send_response(200); self.send_header("Content-Length", str(mb << 20)); self.end_headers()
            chunk = os.urandom(1 << 20)
            for _ in range(mb): self.wfile.write(chunk)
        else:
            body = b"ok"
            self.send_response(200); self.send_header("Content-Length", "2"); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); left = n; h = hashlib.md5()
        while left > 0:
            b = self.rfile.read(min(1 << 20, left))
            if not b: break
            h.update(b); left -= len(b)
        body = h.hexdigest().encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
class T(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True; request_queue_size = 64
T(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
