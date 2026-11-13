#!/usr/bin/env python3
"""Stage signed public archives for the existing production journey matrix."""

import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys

REPOSITORY = "layervai/qurl-integrations"
IDENTITY = f"https://github.com/{REPOSITORY}/.github/workflows/release-please.yml@refs/heads/main"


def stage(tag, source, directory):
    if (os.environ.get("GITHUB_REPOSITORY") != REPOSITORY
            or os.environ.get("GITHUB_REF") != "refs/heads/main"
            or not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?", tag)
            or not re.fullmatch(r"[0-9a-f]{40}", source)):
        raise ValueError("public proof requires canonical main and an exact tag/source")
    actual = subprocess.check_output(["git", "rev-parse", "--verify", f"{tag}^{{commit}}"], text=True).strip()
    if actual != source:
        raise ValueError("published tag source mismatch")
    release = json.loads(subprocess.check_output([
        "gh", "release", "view", tag, "--repo", REPOSITORY,
        "--json", "tagName,targetCommitish,isDraft"], text=True))
    if release != {"tagName": tag, "targetCommitish": source, "isDraft": False}:
        raise ValueError("release must be public and bound to the exact source")
    directory.mkdir(parents=True)
    names = [f"qurl_{tag[1:]}_{system}_{arch}.{extension}"
             for system, extension in (("linux", "tar.gz"), ("darwin", "tar.gz"), ("windows", "zip"))
             for arch in ("amd64", "arm64")]
    command = ["gh", "release", "download", tag, "--repo", REPOSITORY, "--dir", str(directory)]
    for name in ["checksums.txt", "checksums.txt.sigstore.json", *names]:
        command += ["--pattern", name]
    subprocess.run(command, check=True)
    subprocess.run([
        "cosign", "verify-blob", "--bundle", str(directory / "checksums.txt.sigstore.json"),
        "--certificate-identity", IDENTITY,
        "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
        str(directory / "checksums.txt")], check=True)
    rows = {}
    for line in (directory / "checksums.txt").read_text().splitlines():
        sha, name = line.split()
        if not re.fullmatch(r"[0-9a-f]{64}", sha) or name in rows:
            raise ValueError("invalid or duplicate signed checksum")
        rows[name] = sha
    for name in names:
        if rows.get(name) != hashlib.sha256((directory / name).read_bytes()).hexdigest():
            raise ValueError("public archive checksum mismatch")


if __name__ == "__main__":
    stage(sys.argv[1], sys.argv[2], pathlib.Path(sys.argv[3]))
