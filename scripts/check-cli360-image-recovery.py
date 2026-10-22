#!/usr/bin/env python3
"""Temporary owner-scoped recovery of the already-built CLI 3.6 image."""
import datetime
import hashlib
import json
import os
import pathlib
import subprocess
import sys

SOURCE = "dab9399a9297ac80ac9302dd8c9814d5f5ece4f2"
DIGEST = "sha256:1ba93f7401cac8aa7ab16b3afbc3e79d51fab518e5b76b6bd5754ec8c9674a30"
CHECKSUMS = "4d6279191cdbd4538a1d0ee351f299694d74f4d85ed3e8a9fbb5ef14827aba83"
BUNDLE = "e02589b9bcb20e000c9e6c411f5e3fe15ee26c456174a56148d0bae9145055f3"
NATIVE_ARCHIVES = {
    "qurl_3.6.0_darwin_amd64.tar.gz": "1fe10d9723da73c050d3ccbacb0a4049326f518b7dbb6011f2a4b1490d601dc2",
    "qurl_3.6.0_darwin_arm64.tar.gz": "71bf510485396f63f2ab6789343d4ea2e37f98ce20c6bd14f3a473e89ff45642",
    "qurl_3.6.0_linux_amd64.tar.gz": "b28e995228d9e3fdc8944b590587d3e9a06b41a43dfe9797d424576ef5c77967",
    "qurl_3.6.0_linux_arm64.tar.gz": "19521dde36007363281c8c6f36fd7303d46bf9243d1c3ba6ce504193889f7932",
    "qurl_3.6.0_windows_amd64.zip": "0421bd7390321d7ecd6839eabbf051b84748334ea85e6891805793f3c1d654ec",
    "qurl_3.6.0_windows_arm64.zip": "81e37f61b438e04409ab4233ce72d80be5cff4d0f562b1428cdb7550303610bc"
}
REPOSITORY = "layervai/qurl-integrations"
RUN = "39854001197"
# shortcut: remove this image-only owner exception after the 3.6 image is signed and promoted.
EXPIRES = datetime.datetime(2026, 10, 23, 2, tzinfo=datetime.timezone.utc)
JOBS = {
    120087338345: ("Verify the exact CLI release gate", "success"),
    120087338761: ("release-please", "skipped"),
    120087340021: ("Start the exact CLI customer journey", "skipped"),
    120087437271: ("release-cli", "success"),
    120094541518: ("Validate exact Windows release archive", "success"),
    120094541559: ("Validate exact Homebrew cask before publication", "success"),
    120094541659: ("Prove the packaged CLI against production (macos, 2, macos-latest, darwin_*.tar.gz)", "success"),
    120094541695: ("Prove the packaged CLI against production (windows, 3, windows-latest, windows_*.zip)", "failure"),
    120094541795: ("Prove the packaged CLI against production (linux, 1, ubuntu-latest, linux_*.tar.gz)", "failure"),
    120095632756: ("Verify production fixture cleanup", "success"),
    120095732959: ("Publish the verified CLI release", "skipped"),
    120095733122: ("Publish the audited Homebrew cask", "skipped"),
    120095733979: ("Remove superseded CLI drafts", "skipped"),
}
FAILED_STEPS = {
    120094541695: {"Mint disposable production credentials", "Remove local disposable credentials"},
    120094541795: {"Require the real production journey to pass without skips"},
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_inputs(source, tag, run, attempt, digest, environment, now):
    require((source, tag, run, attempt, digest) == (SOURCE, "v3.6.0", RUN, "1", DIGEST), "recovery candidate is not the exact built CLI 3.6 image")
    require(environment.get("GITHUB_REPOSITORY") == REPOSITORY and environment.get("GITHUB_REF") == "refs/heads/main", "image recovery requires canonical main")
    require(environment.get("GITHUB_ACTOR") == "justin-layerv" and environment.get("GITHUB_EVENT_NAME") == "workflow_dispatch", "image recovery requires the authorized owner dispatch")
    require(environment.get("GITHUB_TRIGGERING_ACTOR", environment.get("GITHUB_ACTOR")) == "justin-layerv", "image recovery rerun requires the authorized owner")
    require(now < EXPIRES, "owner image recovery authorization expired")


def validate_evidence(run, jobs, release):
    expected_run = {
        "id": int(RUN), "run_attempt": 1,
        "head_sha": "4646b9e6f4ca7dc97d5f9517564b591149e59e46",
        "head_branch": "main", "path": ".github/workflows/release-please.yml",
        "event": "workflow_dispatch", "status": "completed", "conclusion": "failure",
    }
    require(all(run.get(key) == value for key, value in expected_run.items()),
            "original build run identity or terminal state differs")
    require(run.get("repository", {}).get("full_name") == REPOSITORY and run.get("head_repository", {}).get("full_name") == REPOSITORY, "original build repository differs")
    require(len(jobs) == len(JOBS) and {job.get("id") for job in jobs} == set(JOBS), "original build job set differs")
    for job in jobs:
        expected = JOBS[job["id"]]
        require((job.get("name"), job.get("conclusion")) == expected and job.get("status") == "completed", "original build verification job differs")
        failed = {step.get("name") for step in job.get("steps", []) if step.get("conclusion") == "failure"}
        require(failed == FAILED_STEPS.get(job["id"], set()), "production failure is outside the owner-deferred steps")
    require(release.get("tag_name") == "v3.6.0" and release.get("target_commitish") == SOURCE and release.get("draft") is False, "public native release binding differs")
    assets = {asset["name"]: asset for asset in release.get("assets", [])}
    for name, digest in (("checksums.txt", CHECKSUMS), ("checksums.txt.sigstore.json", BUNDLE)):
        require(assets.get(name, {}).get("digest") == "sha256:" + digest, "public native signed manifest differs")
    for name, digest in NATIVE_ARCHIVES.items():
        require(assets.get(name, {}).get("digest") == "sha256:" + digest,
                "public native archive digest differs from the verified manifest")


def api(path):
    result = subprocess.run(["gh", "api", "--method", "GET", "-H", "Cache-Control: no-cache", f"repos/{REPOSITORY}/{path}"], check=True, capture_output=True, text=True, timeout=45)
    return json.loads(result.stdout)


def main():
    require(len(sys.argv) == 7, "expected source, tag, build run, attempt, image digest, and manifest directory")
    validate_inputs(*sys.argv[1:6], os.environ, datetime.datetime.now(datetime.timezone.utc))
    run = api(f"actions/runs/{RUN}")
    jobs = api(f"actions/runs/{RUN}/attempts/1/jobs?per_page=100")
    require(jobs.get("total_count") == len(JOBS), "original job pagination changed")
    validate_evidence(run, jobs["jobs"], api("releases/tags/v3.6.0"))
    directory = pathlib.Path(sys.argv[6])
    for name, digest in (("checksums.txt", CHECKSUMS), ("checksums.txt.sigstore.json", BUNDLE)):
        require(hashlib.sha256((directory / name).read_bytes()).hexdigest() == digest, "downloaded signed manifest differs")
    print("Owner deferred only the recorded Linux private-denial and Windows setup proof failures; native validation and cleanup passed. Exact existing image only; no rebuild or native asset changes.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
