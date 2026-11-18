import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import unittest


PROVIDER = Path(__file__).parent / "share-providers" / "qurl.sh"
CRID = "a" * 58
ADDRESS = "https://qurl.link/" + CRID
STUB = r'''#!/usr/bin/env python3
import json, os, signal, sys, time
with open(os.environ['QURL_STUB_LOG'], 'a') as f:
    f.write(json.dumps(sys.argv[1:]) + '\n')
if sys.argv[1] == 'publish':
    print(os.environ['QURL_STUB_DOCUMENT'], flush=True)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    while True: time.sleep(.1)
elif sys.argv[1] == 'delete':
    if os.environ.get('QURL_STUB_HANG_DELETE'): time.sleep(60)
'''


class ProviderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        stub = root / "qurl"
        stub.write_text(STUB)
        stub.chmod(0o700)
        self.log = root / "calls.jsonl"
        self.document = {"crid": CRID, "resource_url": ADDRESS, "private": True,
                         "found_existing": False, "access_requests": True}
        self.env = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                    "DDEV_LOCAL_URL": "http://127.0.0.1:8080", "DDEV_SHARE_ARGS": "",
                    "QURL_STUB_LOG": str(self.log)}

    def start(self):
        self.env["QURL_STUB_DOCUMENT"] = json.dumps(self.document)
        proc = subprocess.Popen(["bash", str(PROVIDER)], env=self.env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        def stop_group():
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.communicate(timeout=5)
        self.addCleanup(stop_group)
        return proc

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_private_request_url_and_sigterm_cleanup(self):
        proc = self.start()
        self.assertEqual(proc.stdout.readline().strip(), ADDRESS)
        proc.send_signal(signal.SIGTERM)
        out, err = proc.communicate(timeout=5)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertIn("qurl approve " + CRID, err)
        self.assertEqual(self.calls()[-1], ["delete", CRID, "--yes"])
        self.assertIn("--foreground", self.calls()[0])
        self.assertIn("--allow-requests", self.calls()[0])

    def test_existing_resource_is_not_advertised_or_deleted(self):
        self.document["found_existing"] = True
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(len(self.calls()), 1)

    def test_unexpected_public_resource_is_removed_without_advertising(self):
        self.document["private"] = False
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(self.calls()[-1], ["delete", CRID, "--yes"])

    def test_unexpected_address_is_removed_without_advertising(self):
        self.document["resource_url"] = "https://example.com/"
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(self.calls()[-1], ["delete", CRID, "--yes"])

    def test_non_object_output_fails_closed(self):
        self.document = []
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(len(self.calls()), 1)

    def test_remote_origin_is_refused_before_publishing(self):
        self.env["DDEV_LOCAL_URL"] = "https://example.com"
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(self.calls(), [])

    def test_extra_flags_cannot_make_the_share_public(self):
        self.env["DDEV_SHARE_ARGS"] = "--public"
        proc = self.start()
        out, _ = proc.communicate(timeout=5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(out, "")
        self.assertEqual(self.calls(), [])

    def test_slow_deletion_stays_within_ddev_shutdown_window(self):
        self.env["QURL_STUB_HANG_DELETE"] = "1"
        proc = self.start()
        self.assertEqual(proc.stdout.readline().strip(), ADDRESS)
        started = time.monotonic()
        proc.send_signal(signal.SIGTERM)
        _, err = proc.communicate(timeout=14)
        self.assertLess(time.monotonic() - started, 14)
        self.assertIn("Cleanup timed out; run: qurl delete " + CRID, err)


if __name__ == "__main__":
    unittest.main()
