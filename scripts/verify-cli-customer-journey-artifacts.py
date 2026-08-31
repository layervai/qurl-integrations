#!/usr/bin/env python3
"""Verify the exact CLI customer-journey artifact before execution."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys


SHA = re.compile(r"[0-9a-f]{40}\Z")
DIGEST = re.compile(r"[0-9a-f]{64}\Z")
VERSION = re.compile(r"v0\.0\.0-ci\.[0-9a-f]{12}\Z")
MODULE_VERSION = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+\Z")


def fail(message: str) -> None:
    raise ValueError(message)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("artifact", type=pathlib.Path)
    parser.add_argument("repository")
    parser.add_argument("head_sha")
    parser.add_argument("run_id", type=int)
    parser.add_argument("run_attempt", type=int)
    args = parser.parse_args()

    manifest_path = args.artifact / "manifest.json"
    try:
        raw = manifest_path.read_bytes()
        manifest = json.loads(raw)
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"manifest is unavailable or malformed: {exc}")
    if raw != (json.dumps(manifest, separators=(",", ":"), sort_keys=True) + "\n").encode():
        fail("manifest is not canonical JSON")
    if (
        set(manifest) != {"files", "head_sha", "modules", "repository", "run_attempt", "run_id", "schema", "version"}
        or manifest["schema"] != "layerv.qurl-cli-customer-journey.v1"
        or manifest["repository"] != args.repository
        or manifest["head_sha"] != args.head_sha
        or not SHA.fullmatch(manifest["head_sha"])
        or manifest["run_id"] != args.run_id
        or manifest["run_attempt"] != args.run_attempt
        or args.run_attempt < 1
        or not VERSION.fullmatch(manifest["version"])
        or manifest["version"].split(".")[-1] != args.head_sha[:12]
    ):
        fail("manifest authority does not match the triggering run")
    modules = manifest["modules"]
    if set(modules) != {"github.com/layervai/qurl-connector", "github.com/layervai/qurl-go"} or any(
        not isinstance(value, str) or not MODULE_VERSION.fullmatch(value) for value in modules.values()
    ):
        fail("manifest module authority is not the dependency lock")
    expected = {
        "qurl-linux-amd64",
        "qurl-darwin-amd64",
        "qurl-darwin-arm64",
        "qurl-windows-amd64.exe",
    }
    files = manifest["files"]
    if not isinstance(files, dict) or set(files) != expected:
        fail("manifest does not contain the exact customer artifact set")
    observed = {
        path.relative_to(args.artifact).as_posix()
        for path in args.artifact.rglob("*")
        if path.is_file() and path.name != "manifest.json"
    }
    if observed != expected:
        fail("downloaded customer artifact set is not exact")
    for name, digest in files.items():
        if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
            fail("manifest contains a malformed digest")
        if hashlib.sha256((args.artifact / name).read_bytes()).hexdigest() != digest:
            fail("downloaded customer artifact digest does not match")
    print("verified exact packaged customer artifacts")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, TypeError, ValueError) as exc:
        print(f"artifact verification failed: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
