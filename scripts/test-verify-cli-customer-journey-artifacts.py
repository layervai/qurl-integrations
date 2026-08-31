#!/usr/bin/env python3
"""Negative tests for exact CLI customer-journey artifact verification."""

from __future__ import annotations

import hashlib
import base64
import json
import os
import pathlib
import shutil
import subprocess
import tempfile


ROOT = pathlib.Path(__file__).resolve().parent.parent
BUILDER = ROOT / "scripts" / "build-cli-customer-journey-artifacts.sh"
VERIFIER = ROOT / "scripts" / "verify-cli-customer-journey-artifacts.py"
REPOSITORY = "layervai/qurl-integrations"
RUN_ID = "701"
RUN_ATTEMPT = "2"
HUB_KEY = bytes(range(32))
BUILD_ENV = dict(
    os.environ,
    QURL_RELEASE_HUB_PUBLIC_KEY_B64=base64.b64encode(HUB_KEY).decode("ascii"),
    QURL_RELEASE_HUB_PUBLIC_KEY_SHA256=hashlib.sha256(HUB_KEY).hexdigest(),
)


def run(
    *arguments: str,
    expect_success: bool,
    error: str = "",
    environment: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(arguments, cwd=ROOT, capture_output=True, text=True, env=environment)
    if (result.returncode == 0) != expect_success:
        raise AssertionError(
            f"unexpected exit {result.returncode}: {' '.join(arguments)}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
        )
    if error and error not in result.stderr:
        raise AssertionError(f"missing error {error!r} in:\n{result.stderr}")
    return result


def write_manifest(path: pathlib.Path, document: dict[object, object]) -> None:
    path.chmod(0o600)
    path.write_text(json.dumps(document, separators=(",", ":"), sort_keys=True) + "\n", encoding="utf-8")


def verify(path: pathlib.Path, head: str, *, expect_success: bool, error: str = "") -> None:
    run(
        str(VERIFIER),
        str(path),
        REPOSITORY,
        head,
        RUN_ID,
        RUN_ATTEMPT,
        expect_success=expect_success,
        error=error,
    )


def main() -> None:
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    with tempfile.TemporaryDirectory(prefix="qurl-artifact-verifier-") as temporary:
        temporary_path = pathlib.Path(temporary)
        pristine = temporary_path / "pristine"
        run(
            str(BUILDER),
            str(pristine),
            REPOSITORY,
            head,
            RUN_ID,
            RUN_ATTEMPT,
            expect_success=True,
            environment=BUILD_ENV,
        )
        verify(pristine, head, expect_success=True)

        trust_case = temporary_path / "trust"
        shutil.copytree(pristine, trust_case)
        manifest_path = trust_case / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["release_hub_public_key_sha256"] = "0" * 64
        write_manifest(manifest_path, manifest)
        verify(trust_case, head, expect_success=False, error="does not carry the manifest Hub trust root")

        for module in ("github.com/layervai/qurl-go", "github.com/layervai/qurl-connector"):
            case = temporary_path / module.rsplit("/", 1)[-1]
            shutil.copytree(pristine, case)
            manifest_path = case / "manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            manifest["modules"][module] = "v9.9.9"
            write_manifest(manifest_path, manifest)
            verify(case, head, expect_success=False, error=f"does not contain exact {module} version v9.9.9")

        digest_case = temporary_path / "digest"
        shutil.copytree(pristine, digest_case)
        digest_binary = digest_case / "qurl-linux-amd64"
        digest_binary.chmod(0o700)
        with digest_binary.open("ab") as binary:
            binary.write(b"tamper")
        verify(digest_case, head, expect_success=False, error="artifact digest does not match")

        windows_case = temporary_path / "windows"
        shutil.copytree(pristine, windows_case)
        replacement_source = temporary_path / "replacement.go"
        replacement_source.write_text("package main\nfunc main() {}\n", encoding="utf-8")
        replacement = windows_case / "qurl-windows-amd64.exe"
        replacement.chmod(0o700)
        environment = dict(os.environ, CGO_ENABLED="0", GOOS="windows", GOARCH="amd64")
        subprocess.run(
            ["go", "build", "-trimpath", "-o", str(replacement), str(replacement_source)],
            cwd=ROOT,
            env=environment,
            check=True,
        )
        manifest_path = windows_case / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["files"][replacement.name] = hashlib.sha256(replacement.read_bytes()).hexdigest()
        write_manifest(manifest_path, manifest)
        verify(windows_case, head, expect_success=False, error="qurl-windows-amd64.exe")

    print("customer artifact verifier negative tests passed")


if __name__ == "__main__":
    main()
