#!/usr/bin/env python3
"""Exercise the public archive trust boundary without network or credentials."""
import hashlib
import importlib.util
import json
import os
import pathlib
import tempfile
import sys
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("public_proof", pathlib.Path(__file__).with_name("stage-public-cli-proof.py"))
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)


class PublicProofTest(unittest.TestCase):
    def execute(self, *, draft=False, target=None, tag_source=None, missing=False, corrupt=False, signature=False):
        source = "a" * 40
        release = {"tagName": "v3.6.0", "targetCommitish": target or source, "isDraft": draft}
        verified = False

        def run(args, check):
            nonlocal verified
            if args[0] == "cosign":
                self.assertEqual(args[args.index("--certificate-identity") + 1], proof.IDENTITY)
                self.assertIn("https://token.actions.githubusercontent.com", args)
                if signature:
                    raise RuntimeError("bad signature")
                verified = True
            elif args[:3] == ["gh", "release", "download"]:
                directory = pathlib.Path(args[args.index("--dir") + 1])
                names = [args[index + 1] for index, arg in enumerate(args) if arg == "--pattern"]
                archives = [name for name in names if name.startswith("qurl_")]
                self.assertEqual(len(archives), 6)
                rows = []
                for name in archives:
                    rows.append(f"{hashlib.sha256(name.encode()).hexdigest()}  {name}")
                    if not (missing and "windows_arm64" in name):
                        (directory / name).write_bytes(b"wrong" if corrupt else name.encode())
                (directory / "checksums.txt").write_text("\n".join(rows))
                (directory / "checksums.txt.sigstore.json").write_text("bundle")
            else:
                self.fail(f"unexpected mutation: {args[0]}")

        with tempfile.TemporaryDirectory() as root, patch.dict(os.environ, {
            "GITHUB_REPOSITORY": proof.REPOSITORY, "GITHUB_REF": "refs/heads/main"
        }), patch.object(proof.subprocess, "check_output", side_effect=[(tag_source or source) + "\n", json.dumps(release)]), patch.object(proof.subprocess, "run", side_effect=run):
            proof.stage("v3.6.0", source, pathlib.Path(root) / "candidate")
        self.assertTrue(verified)

    def test_signed_public_archives(self):
        self.execute()

    def test_refuses_nonpublic_or_mismatched_source(self):
        for change in ({"draft": True}, {"target": "b" * 40}, {"tag_source": "b" * 40}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.execute(**change)

    def test_refuses_missing_corrupt_or_unsigned_archive(self):
        for change, error in (({"missing": True}, FileNotFoundError), ({"corrupt": True}, ValueError), ({"signature": True}, RuntimeError)):
            with self.subTest(change=change), self.assertRaises(error):
                self.execute(**change)

    def test_refuses_untrusted_workflow(self):
        with patch.dict(os.environ, {"GITHUB_REPOSITORY": "other/repo", "GITHUB_REF": "refs/heads/main"}), self.assertRaises(ValueError):
            proof.stage("v3.6.0", "a" * 40, pathlib.Path("unused"))


if __name__ == "__main__":
    unittest.main()
