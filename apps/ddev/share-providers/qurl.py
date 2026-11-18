#!/usr/bin/env python3
#ddev-generated
"""Private qURL request links for DDEV's share-provider contract."""

import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from urllib.parse import urlsplit


def new_crid(document):
    if not isinstance(document, dict):
        return None
    crid = document.get("crid")
    if document.get("found_existing") is False and isinstance(crid, str):
        if re.fullmatch(r"[a-z2-7]{32,256}", crid):
            return crid
    return None


def interrupted(_signum, _frame):
    # DDEV treats a nonzero provider exit as a failure, including after Ctrl+C.
    raise SystemExit(0)


def main():
    origin = os.environ.get("DDEV_LOCAL_URL", "")
    parsed = urlsplit(origin)
    if parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise ValueError("DDEV_LOCAL_URL must be a loopback HTTP address")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("DDEV_LOCAL_URL must not contain credentials, a query or a fragment")
    if os.environ.get("DDEV_SHARE_ARGS", "").strip():
        raise ValueError("This private provider does not accept --provider-args")
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, interrupted)

    with tempfile.TemporaryFile() as output:
        publisher = subprocess.Popen([
            "qurl", "publish", origin, "--id", "ddev-" + uuid.uuid4().hex,
            "--name", "DDEV private review", "--allow-requests", "--foreground", "-o", "json",
        ], stdout=output)
        document = {}
        try:
            deadline = time.monotonic() + 45
            while time.monotonic() < deadline:
                raw = os.pread(output.fileno(), 65537, 0)
                if len(raw) > 65536:
                    raise ValueError("qURL startup output exceeded its limit")
                try:
                    document = json.loads(raw)
                    if not isinstance(document, dict):
                        raise ValueError("qURL startup output must be an object")
                    break
                except json.JSONDecodeError:
                    if publisher.poll() is not None:
                        raise RuntimeError("qURL stopped before publishing a share")
                    time.sleep(0.1)
            else:
                raise TimeoutError("qURL did not publish within 45 seconds")
            crid = new_crid(document)
            address = document.get("resource_url")
            if not crid or document.get("private") is not True or document.get("access_requests") is not True:
                raise ValueError("qURL did not confirm a new private request-enabled resource")
            if address != "https://qurl.link/" + crid:
                raise ValueError("qURL did not return the expected private request address")
            print(address, flush=True)
            print("Ask your reviewer for their code, then run: qurl approve " + crid + " <code>", file=sys.stderr)
            return publisher.wait()
        finally:
            for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                signal.signal(sig, signal.SIG_IGN)
            # Recover a complete startup record if cancellation arrived before the next poll.
            try:
                document = json.loads(os.pread(output.fileno(), 65536, 0))
            except ValueError:
                document = {}
            crid = new_crid(document)
            try:
                if crid:
                    try:
                        deleted = subprocess.run(["qurl", "delete", crid, "--yes"], timeout=10, stdout=sys.stderr)
                        if deleted.returncode:
                            print("Cleanup needs a retry: qurl delete " + crid, file=sys.stderr)
                    except subprocess.TimeoutExpired:
                        print("Cleanup timed out; run: qurl delete " + crid, file=sys.stderr)
            finally:
                if publisher.poll() is None:
                    publisher.terminate()
                    try:
                        publisher.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        publisher.kill()
                        publisher.wait()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, TimeoutError) as error:
        print("qURL provider: " + str(error), file=sys.stderr)
        sys.exit(1)
