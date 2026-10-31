#!/usr/bin/env python3
"""Check the public origin pin before Slack release workflows build a new image."""
import hashlib
import json
from pathlib import Path
import re
import sys
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener

REPOSITORY = "layervai/qurl-integrations/s3-static-connector"
IMAGE = "ghcr.io/" + REPOSITORY
DIGEST = r"sha256:[0-9a-f]{64}"
INDEX_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("registry redirects are not allowed")


def fetch(url, token=None):
    headers = {"Accept": ", ".join(sorted(INDEX_TYPES))}
    if token is not None:
        headers["Authorization"] = "Bearer " + token
    with build_opener(NoRedirect).open(Request(url, headers=headers), timeout=15) as response:
        body = response.read(1024 * 1024 + 1)
    if len(body) > 1024 * 1024:
        raise ValueError("registry response exceeds 1 MiB")
    return body


def default_image(source):
    matches = re.findall(
        r'^\s*defaultS3StaticConnectorImage\s*=\s*"(' + re.escape(IMAGE) + "@" + DIGEST + r')"\s*$',
        source, re.MULTILINE,
    )
    if len(matches) != 1:
        raise ValueError("expected one canonical digest-pinned Slack S3 default")
    return matches[0]


def check_manifest(pinned, body):
    document = json.loads(body)
    if document.get("schemaVersion") != 2 or document.get("mediaType") not in INDEX_TYPES:
        raise ValueError("promoted origin must be a multi-architecture image index")
    runtimes = [entry for entry in document["manifests"]
                if entry.get("platform", {}).get("os") == "linux"
                and entry.get("platform", {}).get("architecture") in ("amd64", "arm64")]
    if (sorted(entry["platform"]["architecture"] for entry in runtimes) != ["amd64", "arm64"]
            or any(not re.fullmatch(DIGEST, entry.get("digest", "")) for entry in runtimes)):
        raise ValueError("promoted origin must have exactly one AMD64 and ARM64 runtime")
    promoted = IMAGE + "@sha256:" + hashlib.sha256(body).hexdigest()
    if pinned != promoted:
        raise ValueError(f"Slack default {pinned} differs from promoted {promoted}; rotate the tested pin")
    return promoted


def main():
    source = Path(__file__).resolve().parents[1] / "apps/slack/internal/handler_s3_website.go"
    pinned = default_image(source.read_text())
    query = urlencode({"service": "ghcr.io", "scope": "repository:" + REPOSITORY + ":pull"})
    # No registry credentials: a private package must fail this release check.
    token = json.loads(fetch("https://ghcr.io/token?" + query)).get("token")
    if not isinstance(token, str) or not re.fullmatch(r"[-A-Za-z0-9._~+/]+=*", token):
        raise ValueError("registry returned an invalid anonymous pull token")
    # shortcut: main is the current promotion channel; switch when #893 defines the customer channel.
    body = fetch("https://ghcr.io/v2/" + REPOSITORY + "/manifests/main", token)
    print("Slack S3 default matches public promoted image: " + check_manifest(pinned, body))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("S3 origin default check failed: " + str(error), file=sys.stderr)
        sys.exit(1)
