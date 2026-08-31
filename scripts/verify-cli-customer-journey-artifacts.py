#!/usr/bin/env python3
"""Verify the exact CLI customer-journey artifact before execution."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import platform
import re
import subprocess
import sys


SHA = re.compile(r"[0-9a-f]{40}\Z")
DIGEST = re.compile(r"[0-9a-f]{64}\Z")
VERSION = re.compile(r"v0\.0\.0-ci\.[0-9a-f]{12}\Z")
MODULE_VERSION = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+\Z")
EXPECTED_PLATFORMS = {
    "qurl-linux-amd64": ("linux", "amd64"),
    "qurl-darwin-amd64": ("darwin", "amd64"),
    "qurl-darwin-arm64": ("darwin", "arm64"),
    "qurl-windows-amd64.exe": ("windows", "amd64"),
}


def native_binary_name() -> str:
    system = platform.system().lower()
    machine = platform.machine().lower()
    architecture = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(machine)
    if architecture is None:
        fail(f"unsupported verifier architecture {machine}")
    suffix = ".exe" if system == "windows" else ""
    name = f"qurl-{system}-{architecture}{suffix}"
    if name not in EXPECTED_PLATFORMS:
        fail(f"customer artifact set has no native verifier binary for {system}/{architecture}")
    return name


def fail(message: str) -> None:
    raise ValueError(message)


def verify_build_info(path: pathlib.Path, head_sha: str, modules: dict[str, str]) -> None:
    result = subprocess.run(
        ["go", "version", "-m", "-json", str(path)],
        check=True,
        capture_output=True,
        text=True,
    )
    info = json.loads(result.stdout)
    dependencies = info.get("Deps")
    settings = info.get("Settings")
    if not isinstance(dependencies, list) or not isinstance(settings, list):
        fail(f"{path.name} has malformed Go build metadata")

    selected: dict[str, str] = {}
    for dependency in dependencies:
        if not isinstance(dependency, dict) or dependency.get("Path") not in modules:
            continue
        module = dependency["Path"]
        if module in selected or dependency.get("Replace") is not None:
            fail(f"{path.name} has ambiguous metadata for {module}")
        selected[module] = dependency.get("Version")
    for module, version in modules.items():
        if selected.get(module) != version:
            fail(f"{path.name} does not contain exact {module} version {version}")

    selected_settings: dict[str, str] = {}
    for setting in settings:
        if not isinstance(setting, dict) or setting.get("Key") not in {"GOOS", "GOARCH", "vcs.revision"}:
            continue
        key = setting["Key"]
        if key in selected_settings or not isinstance(setting.get("Value"), str):
            fail(f"{path.name} has ambiguous {key} build metadata")
        selected_settings[key] = setting["Value"]
    expected_os, expected_arch = EXPECTED_PLATFORMS[path.name]
    if selected_settings != {"GOOS": expected_os, "GOARCH": expected_arch, "vcs.revision": head_sha}:
        fail(f"{path.name} source or platform metadata is not exact")


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
        set(manifest) != {"files", "head_sha", "modules", "release_hub_public_key_sha256", "repository", "run_attempt", "run_id", "schema", "version"}
        or manifest["schema"] != "layerv.qurl-cli-customer-journey.v1"
        or manifest["repository"] != args.repository
        or manifest["head_sha"] != args.head_sha
        or not SHA.fullmatch(manifest["head_sha"])
        or manifest["run_id"] != args.run_id
        or manifest["run_attempt"] != args.run_attempt
        or args.run_attempt < 1
        or not DIGEST.fullmatch(manifest["release_hub_public_key_sha256"])
        or not VERSION.fullmatch(manifest["version"])
        or manifest["version"].split(".")[-1] != args.head_sha[:12]
    ):
        fail("manifest authority does not match the triggering run")
    modules = manifest["modules"]
    if set(modules) != {"github.com/layervai/qurl-connector", "github.com/layervai/qurl-go"} or any(
        not isinstance(value, str) or not MODULE_VERSION.fullmatch(value) for value in modules.values()
    ):
        fail("manifest module authority is not the dependency lock")
    expected = set(EXPECTED_PLATFORMS)
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
        verify_build_info(args.artifact / name, args.head_sha, modules)
    native = args.artifact / native_binary_name()
    trust = subprocess.run(
        [str(native), "version", "--verify-release-native-trust"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if trust != manifest["release_hub_public_key_sha256"]:
        fail("native customer artifact does not carry the manifest Hub trust root")
    print("verified exact packaged customer artifacts")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (json.JSONDecodeError, OSError, subprocess.CalledProcessError, TypeError, ValueError) as exc:
        print(f"artifact verification failed: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
