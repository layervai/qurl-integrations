#!/usr/bin/env python3
"""Minimal S3 stand-in for s3-static-connector behavior tests.

It deliberately does NOT verify SigV4 (there is no shared secret): it asserts
that the origin's Envoy signer attached an Authorization header and forwarded
the expected canonical path/Host, which is what proves nginx's rewrite + query
strip + signing-after-rewrite wiring. Cryptographic verification against real S3
happens during the staging soak.

Known keys return fixture bodies + headers. `badrequest*` -> 400, `forbidden*`
-> 403 (auth/signing failure), `boom*` -> 500 (upstream 5xx), unknown -> 404.
The received request line is echoed to stderr (docker logs) so the test can
assert cache behavior by counting upstream hits.
"""
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit, unquote

# key (no leading slash) -> (content_type, cache_control_or_None, body bytes)
FIXTURES = {
    "index.html": ("text/html", "public, max-age=300, must-revalidate", b"index"),
    "website/index.html": ("text/html", None, b"website"),
    "metrics.json": ("application/json", "max-age=300", b'{"ok":true}'),
    "cacheprobe.json": ("application/json", "max-age=300", b'{"probe":1}'),
    "v1.2/docs": ("text/plain", None, b"docs"),
    "about.": ("text/plain", None, b"trailing-dot"),
    "styles/app.css": ("text/css", None, b"body{}"),
    "deep/path/index.html": ("text/html", None, b"deep"),
    # octet-stream (not in nginx gzip_types): gzip would disable byte ranges.
    # Cache-Control so nginx caches it (default config caches only what S3 marks
    # cacheable); the range test serves the 206 from cache.
    "range.bin": ("application/octet-stream", "max-age=300", b"0123456789"),
    "home.htm": ("text/html", None, b"home"),
    "site/index.html": ("text/html", None, b"prefixed-index"),
    "site/website/index.html": ("text/html", None, b"prefixed"),
}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _key(self):
        return unquote(urlsplit(self.path).path).lstrip("/")

    def _echo(self):
        self.send_header("X-Stub-Path", self.path)
        self.send_header("X-Stub-Host", self.headers.get("Host", ""))
        self.send_header(
            "X-Stub-Authorization",
            "present" if self.headers.get("Authorization") else "absent",
        )
        self.send_header(
            "X-Stub-Amz-Content-Sha256",
            self.headers.get("x-amz-content-sha256", "absent"),
        )
        self.send_header(
            "X-Stub-Client-Amz-Meta",
            self.headers.get("x-amz-meta-client", "absent"),
        )

    def _send(self, status, body=b"", ctype=None, cache=None, head_only=False,
              extra=None):
        self.send_response(status)
        self._echo()
        if ctype:
            self.send_header("Content-Type", ctype)
        if cache:
            self.send_header("Cache-Control", cache)
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if not head_only and body:
            self.wfile.write(body)

    def _serve(self, head_only=False):
        key = self._key()
        if "badrequest" in key:
            return self._send(400, b"<Error><Code>InvalidRequest</Code></Error>",
                              ctype="application/xml", head_only=head_only)
        if "forbidden" in key:
            return self._send(403, b"", head_only=head_only)
        if "boom" in key:
            return self._send(500, b"", head_only=head_only)
        fx = FIXTURES.get(key)
        if fx is None:
            return self._send(404, b"<Error><Code>NoSuchKey</Code></Error>",
                              ctype="application/xml", head_only=head_only)
        ctype, cache, body = fx
        rng = self.headers.get("Range")
        if rng and rng.startswith("bytes="):
            spec = rng[len("bytes="):].split("-", 1)
            if not spec[0] and len(spec) > 1 and spec[1]:
                suffix_len = min(int(spec[1]), len(body))
                start = len(body) - suffix_len
                end = len(body) - 1
            else:
                start = int(spec[0]) if spec[0] else 0
                end = int(spec[1]) if len(spec) > 1 and spec[1] else len(body) - 1
            chunk = body[start:end + 1]
            return self._send(
                206, chunk, ctype=ctype, cache=cache, head_only=head_only,
                extra={"Content-Range": f"bytes {start}-{end}/{len(body)}",
                       "Accept-Ranges": "bytes"})
        self._send(200, body, ctype=ctype, cache=cache, head_only=head_only)

    def do_GET(self):
        self._serve(head_only=False)

    def do_HEAD(self):
        self._serve(head_only=True)

    def log_message(self, fmt, *args):
        sys.stderr.write("STUB %s\n" % (fmt % args))


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 9000), Handler).serve_forever()
