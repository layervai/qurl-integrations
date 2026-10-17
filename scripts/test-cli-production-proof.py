#!/usr/bin/env python3
"""Offline regression tests for candidate binding and stale/broken evidence."""

import argparse
import importlib.util
import io
import json
import os
import pathlib
import shlex
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
import zipfile
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("proof", pathlib.Path(__file__).with_name("cli-production-proof.py"))
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)


class ProductionEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = pathlib.Path(self.temporary.name)
        self.receipts = self.directory / "receipts"
        self.receipts.mkdir()
        self.args = argparse.Namespace(directory=self.directory, receipts=self.receipts, tag="v3.5.0", source_sha="a" * 40)
        self.patch = patch.dict(os.environ, GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="2")
        self.patch.start()
        self.addCleanup(self.patch.stop)
        rows = []
        payload = b"exact candidate bytes"
        for system in ("linux", "darwin", "windows"):
            for arch in ("amd64", "arm64"):
                extension = "zip" if system == "windows" else "tar.gz"
                name = f"qurl_3.5.0_{system}_{arch}.{extension}"
                path = self.directory / name
                if system == "windows":
                    with zipfile.ZipFile(path, "w") as archive:
                        archive.writestr("qurl.exe", payload)
                else:
                    with tarfile.open(path, "w:gz") as archive:
                        member = tarfile.TarInfo("qurl")
                        member.size = len(payload)
                        archive.addfile(member, io.BytesIO(payload))
                rows.append(f"{proof.digest(path.read_bytes())}  {name}\n")
        (self.directory / "checksums.txt").write_text("".join(rows))
        checksum_digest, checksums = proof.checksums(self.directory)
        for lane, suffix in (("linux", "linux_amd64.tar.gz"), ("macos", "darwin_arm64.tar.gz"), ("windows", "windows_amd64.zip")):
            name = f"qurl_3.5.0_{suffix}"
            receipt = {
                "endpoint": proof.ENDPOINT, "source_sha": self.args.source_sha, "tag": self.args.tag,
                "run_id": "123", "run_attempt": "2", "lane": lane, "archive": name,
                "archive_sha256": checksums[name], "binary_sha256": proof.digest(payload),
                "checksums_sha256": checksum_digest, "passed_at": int(time.time()),
            }
            (self.receipts / f"{lane}.json").write_text(json.dumps(receipt))

    def test_exact_production_candidate_passes(self):
        proof.verify(self.args)

    def test_invalid_evidence_blocks(self):
        path = self.receipts / "linux.json"
        original = json.loads(path.read_text())
        for field, value in (
            ("endpoint", "https://sandbox.invalid"), ("source_sha", "b" * 40),
            ("tag", "v3.4.1"), ("run_id", "124"), ("run_attempt", "3"), ("run_attempt", "1"),
            ("lane", "windows"), ("archive", "qurl_3.5.0_darwin_arm64.tar.gz"),
            ("passed_at", int(time.time()) - 3601), ("passed_at", int(time.time()) + 120),
            ("passed_at", True), ("checksums_sha256", "0" * 64),
            ("archive_sha256", "0" * 64), ("binary_sha256", "0" * 64),
        ):
            with self.subTest(field=field, value=value):
                path.write_text(json.dumps(dict(original, **{field: value})))
                with self.assertRaises(ValueError):
                    proof.verify(self.args)
        path.write_text(json.dumps(original))

    def test_missing_platform_blocks(self):
        (self.receipts / "windows.json").unlink()
        with self.assertRaises(ValueError): proof.verify(self.args)

    def test_changed_untested_architecture_blocks(self):
        (self.directory / "qurl_3.5.0_linux_arm64.tar.gz").write_bytes(b"replacement")
        with self.assertRaises(ValueError): proof.verify(self.args)

    def test_changed_checksums_block(self):
        path = self.directory / "checksums.txt"
        path.write_text("".join(reversed(path.read_text().splitlines(keepends=True))))
        proof.checksums(self.directory)
        with self.assertRaises(ValueError): proof.verify(self.args)

    def test_record_rejects_changed_executable(self):
        receipt = json.loads((self.receipts / "linux.json").read_text())
        (self.directory / "candidate.json").write_text(json.dumps(receipt))
        (self.directory / "qurl").write_bytes(b"changed after staging")
        self.args.output = self.directory / "recorded"
        with self.assertRaises(ValueError): proof.record(self.args)

    def test_wrong_native_platform_blocks_staging(self):
        self.args.lane = "windows"
        with patch.object(proof.platform, "system", return_value="Linux"):
            with self.assertRaises(ValueError): proof.stage(self.args)

    def test_staging_uses_the_archive_and_checks_version(self):
        self.args.lane = "linux"
        with patch.object(proof.platform, "system", return_value="Linux"), \
                patch.object(proof.platform, "machine", return_value="x86_64"), \
                patch.dict(os.environ, GITHUB_ENV=str(self.directory / "environment"), GH_TOKEN="must-not-reach-candidate", QURL_API_KEY="must-not-reach-candidate"), \
                patch.object(proof.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"qurl version 3.5.0 (linux/amd64)\n")) as run:
            proof.stage(self.args)
            self.assertNotIn("GH_TOKEN", run.call_args.kwargs["env"])
            self.assertNotIn("QURL_API_KEY", run.call_args.kwargs["env"])
            self.assertLessEqual(set(run.call_args.kwargs["env"]), {"PATH", "SystemRoot", "TEMP", "TMP", "HOME"})
            self.assertEqual((self.directory / "qurl").read_bytes(), b"exact candidate bytes")
            self.args.output = self.directory / "recorded"
            proof.record(self.args)
            self.assertTrue((self.args.output / "linux.json").is_file())
        with patch.object(proof.platform, "system", return_value="Linux"), \
                patch.object(proof.platform, "machine", return_value="x86_64"), \
                patch.object(proof.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"qurl version 3.4.1 (linux/amd64)\n")):
            with self.assertRaises(ValueError): proof.stage(self.args)

    def test_checksum_paths_cannot_escape_on_either_platform(self):
        for name in ("../escape.tar.gz", "C:\\escape.tar.gz", "C:escape.tar.gz"):
            with self.subTest(name=name):
                (self.directory / "checksums.txt").write_text("0" * 64 + "  " + name + "\n")
                with self.assertRaises(ValueError): proof.checksums(self.directory)

    def test_duplicate_checksum_blocks(self):
        path = self.directory / "checksums.txt"
        path.write_text(path.read_text() * 2)
        with self.assertRaises(ValueError): proof.verify(self.args)


class ProductionCleanupTest(unittest.TestCase):
    def test_all_lanes_and_attempts_are_cleaned_without_parent_inheritance(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            helper = root / "mock.py"
            helper.write_text('''import json, os, pathlib, sys
assert "AUTOMATION_API_KEY" not in os.environ
args = sys.argv[1:]
key = pathlib.Path(args[args.index("--api-key-file")+1])
assert key.read_text() == "fixture-parent-key"
with open(os.environ["MOCK_CALLS"], "a") as output:
    output.write(json.dumps(args)+"\\n")
sys.exit(int(os.environ.get("MOCK_STATUS", "0")))
''')
            fake = root / "python3"
            fake.write_text(f'#!/bin/sh\nexec {shlex.quote(sys.executable)} {shlex.quote(str(helper))} "$@"\n')
            fake.chmod(0o700)
            calls_path = root / "calls"
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ["PATH"],
                       AUTOMATION_API_KEY="fixture-parent-key", QURL_PRODUCTION_OWNER_ID="fixture-owner",
                       MOCK_CALLS=str(calls_path))
            script = pathlib.Path(__file__).with_name("cleanup-cli-production-proof.sh")
            for status in ("0", "1"):
                with self.subTest(status=status):
                    calls_path.unlink(missing_ok=True)
                    result = subprocess.run(["bash", str(script), "123", "5"], env=dict(env, MOCK_STATUS=status), capture_output=True)
                    self.assertEqual(result.returncode, int(status))
                    calls = [json.loads(line) for line in calls_path.read_text().splitlines()]
                    self.assertEqual(len(calls), 2)
                    specs = [args[i+1] for args in calls for i, value in enumerate(args) if value == "--run-spec"]
                    self.assertEqual(set(specs), {
                        f"123{index}:{attempt}:{lane}:host:full"
                        for index, lane in enumerate(("linux", "macos", "windows"), 1)
                        for attempt in range(1, 6)
                    })
                    for args in calls:
                        self.assertEqual(args[args.index("--qurl-endpoint")+1], proof.ENDPOINT)
                        self.assertFalse(pathlib.Path(args[args.index("--api-key-file")+1]).exists())


if __name__ == "__main__":
    unittest.main()
