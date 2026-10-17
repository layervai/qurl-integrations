#!/usr/bin/env python3
"""Bind production journey evidence to the exact draft release archives."""

import argparse
import hashlib
import json
import os
import pathlib
import platform
import re
import subprocess
import tarfile
import time
import zipfile

ENDPOINT = "https://api.layerv.ai"
LANES = {"linux", "macos", "windows"}


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def checksums(directory):
    raw = (directory / "checksums.txt").read_bytes()
    rows = {}
    for line in raw.decode("ascii").splitlines():
        sha, name = line.split()
        if (not re.fullmatch(r"[0-9a-f]{64}", sha) or pathlib.PurePosixPath(name).name != name
                or pathlib.PureWindowsPath(name).name != name or pathlib.PureWindowsPath(name).drive or name in rows):
            raise ValueError("malformed or duplicate release checksum")
        rows[name] = sha
    return digest(raw), rows


def binary_bytes(archive):
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as bundle:
            return bundle.read("qurl.exe")
    with tarfile.open(archive) as bundle:
        member = bundle.getmember("qurl")
        if not member.isfile():
            raise ValueError("release binary is not a regular file")
        return bundle.extractfile(member).read()


def stage(args):
    sha, rows = checksums(args.directory)
    if platform.system() != {"linux": "Linux", "macos": "Darwin", "windows": "Windows"}[args.lane]:
        raise ValueError("production lane does not match the native runner")
    machine = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    system = {"linux": "linux", "macos": "darwin", "windows": "windows"}[args.lane]
    extension = "zip" if system == "windows" else "tar.gz"
    name = f"qurl_{args.tag[1:]}_{system}_{machine}.{extension}"
    archive = args.directory / name
    if rows.get(name) != digest(archive.read_bytes()):
        raise ValueError("candidate archive differs from its release checksum")
    payload = binary_bytes(archive)
    binary = args.directory / ("qurl.exe" if system == "windows" else "qurl")
    binary.write_bytes(payload)
    binary.chmod(0o700)
    # No rebuild, deployment override, or trust-root injection is permitted.
    result = subprocess.run([str(binary), "version"], capture_output=True, check=True, timeout=30)
    if result.stdout.decode().split()[2] != args.tag:
        raise ValueError("candidate binary has the wrong release version")
    receipt = {
        "endpoint": ENDPOINT, "source_sha": args.source_sha, "tag": args.tag,
        "run_id": os.environ["GITHUB_RUN_ID"], "run_attempt": os.environ["GITHUB_RUN_ATTEMPT"],
        "lane": args.lane, "archive": name, "archive_sha256": rows[name],
        "binary_sha256": digest(payload), "checksums_sha256": sha,
    }
    (args.directory / "candidate.json").write_text(json.dumps(receipt))
    with open(os.environ["GITHUB_ENV"], "a") as output:
        output.write(f"QURL_PRODUCTION_BINARY={binary}\n")


def record(args):
    receipt = json.loads((args.directory / "candidate.json").read_text())
    binary = args.directory / ("qurl.exe" if receipt["lane"] == "windows" else "qurl")
    if digest(binary.read_bytes()) != receipt["binary_sha256"]:
        raise ValueError("candidate binary changed during the production journey")
    receipt["passed_at"] = int(time.time())
    args.output.mkdir(parents=True, exist_ok=True)
    (args.output / f"{receipt['lane']}.json").write_text(json.dumps(receipt, sort_keys=True) + "\n")


def verify(args):
    sha, rows = checksums(args.directory)
    if {p.name for p in args.receipts.iterdir()} != {f"{lane}.json" for lane in LANES}:
        raise ValueError("production evidence requires exactly three platform receipts")
    now = int(time.time())
    for lane in sorted(LANES):
        receipt = json.loads((args.receipts / f"{lane}.json").read_text())
        if (
            receipt["endpoint"] != ENDPOINT or receipt["source_sha"] != args.source_sha
            or receipt["tag"] != args.tag or receipt["lane"] != lane
            or receipt["run_id"] != os.environ["GITHUB_RUN_ID"]
            or receipt["run_attempt"] != os.environ["GITHUB_RUN_ATTEMPT"]
            or type(receipt.get("passed_at")) is not int
            or not -60 <= now - receipt["passed_at"] <= 3600
            or receipt["checksums_sha256"] != sha
        ):
            raise ValueError("production evidence is stale or belongs to another candidate")
        name = receipt["archive"]
        system = {"linux": "linux", "macos": "darwin", "windows": "windows"}[lane]
        extension = "zip" if lane == "windows" else "tar.gz"
        if name not in rows or name not in {f"qurl_{args.tag[1:]}_{system}_{arch}.{extension}" for arch in ("amd64", "arm64")}:
            raise ValueError("production evidence names an unknown archive")
        archive = args.directory / name
        if (digest(archive.read_bytes()) != rows[name] or rows[name] != receipt["archive_sha256"]
                or digest(binary_bytes(archive)) != receipt["binary_sha256"]):
            raise ValueError("release bytes changed after the production proof")
    # Check every archive, including the architectures whose packaging was
    # validated by the existing native/archive jobs rather than this journey.
    archives = {name: expected for name, expected in rows.items() if name.endswith((".tar.gz", ".zip"))}
    if len(archives) != 6:
        raise ValueError("release must have exactly six platform archives")
    for name, expected in archives.items():
        if digest((args.directory / name).read_bytes()) != expected:
            raise ValueError("a release archive changed after the production proof")
    print("Fresh production proof matches the exact release bytes on Linux, macOS, and Windows.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for command in ("stage", "record", "verify"):
        current = sub.add_parser(command)
        current.add_argument("--directory", type=pathlib.Path, required=True)
        if command == "record":
            current.add_argument("--output", type=pathlib.Path, required=True)
        else:
            current.add_argument("--tag", required=True)
            current.add_argument("--source-sha", required=True)
            if command == "stage":
                current.add_argument("--lane", choices=sorted(LANES), required=True)
            else:
                current.add_argument("--receipts", type=pathlib.Path, required=True)
    args = parser.parse_args()
    if args.command != "record" and (not re.fullmatch(r"v\d+\.\d+\.\d+(?:[.-][\w.-]+)?", args.tag)
            or not re.fullmatch(r"[0-9a-f]{40}", args.source_sha)):
        parser.error("exact release tag and source commit are required")
    {"stage": stage, "record": record, "verify": verify}[args.command](args)


if __name__ == "__main__":
    main()
