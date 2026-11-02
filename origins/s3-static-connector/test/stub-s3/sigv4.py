"""Test-only S3 GET/HEAD verifier for the stub's synthetic credentials.

This is not an authentication service. Query strings are forbidden because the
origin must strip them. No IAM, clock, credential-provider or TLS claims are made.
Canonicalization follows AWS's header-auth rules: preserve S3 path separators
and existing URI escapes, encode other bytes, and never normalize dot segments.
https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sig-v4-header-based-auth.html
"""
import hashlib
import hmac
import re
from urllib.parse import quote


def verify_sigv4(method, target, headers, access_key="test", secret_key="test"):
    """Reject a malformed or mismatched signature without returning key material."""
    if method not in ("GET", "HEAD") or "?" in target:
        return False
    if not target.startswith("/") or re.search(r"%(?![0-9a-fA-F]{2})", target):
        return False
    auth = re.fullmatch(
        r"AWS4-HMAC-SHA256 Credential=([^/]+)/([0-9]{8})/us-east-1/s3/aws4_request,\s*"
        r"SignedHeaders=([a-z0-9;-]+),\s*Signature=([0-9a-f]{64})",
        headers.get("Authorization", ""),
    )
    if not auth or auth[1] != access_key:
        return False
    names = auth[3].split(";")
    if names != sorted(set(names)) or not {"host", "x-amz-date"}.issubset(names):
        return False
    timestamp = headers.get("x-amz-date", "")
    if not re.fullmatch(auth[2] + r"T[0-9]{6}Z", timestamp):
        return False
    payload = headers.get("x-amz-content-sha256", "")
    if payload not in ("UNSIGNED-PAYLOAD", hashlib.sha256(b"").hexdigest()):
        return False
    canonical_headers = []
    for name in names:
        values = headers.get_all(name)
        if not values:
            return False
        canonical_headers.append(name + ":" + ",".join(" ".join(v.split()) for v in values) + "\n")
    uri = quote(target, safe="/%~")
    canonical = "\n".join([method, uri, "", "".join(canonical_headers), auth[3], payload])
    scope = auth[2] + "/us-east-1/s3/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", timestamp, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ("AWS4" + secret_key).encode()
    for part in (auth[2], "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    expected = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, auth[4])
