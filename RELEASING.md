# Releasing and verifying releases

How qURL™ CLI releases are produced, what a release ships, and how to
verify a download before trusting it. Release *process* mechanics
(per-component tracks, tag shapes, recovery) live in the
[`.github/workflows/release-please.yml`](.github/workflows/release-please.yml)
header and [README → Releases](README.md#releases); this document is the
consumer-facing contract.

## Required CLI customer-journey gate

Pull-request CI builds and verifies the packaged CLI artifacts without access
to live credentials. After the exact change reaches `main`, a trusted
customer-journey workflow accepts only the matching same-repository `push`
SHA. It runs the exact packaged artifacts on Linux, macOS, and Windows. A CLI
release stays blocked until that exact `main` result passes. The smoke must
begin with a fresh local state directory and an ordinary account key piped to
`qurl login`. Every later command must use only the stored device identity.

The required journey is: login, warm `whoami`, remote-URL publish with status
and inspect, loopback publish, status, inspect, list, public-route response,
stop, route fencing, start, restart, background-daemon restart and resume on a
supported desktop platform, delete, and credential/resource cleanup. The
Windows CI matrix must also build and run
the released command surface and its real Task Scheduler integration. Unit
tests, compiled tagged tests, source receipts, and artifact attestations support
this gate; they do not replace the live journey.

Live inputs belong only in a protected, `main`-only environment. Each platform
lane must use an isolated test owner and a disposable minimum-scope credential.
Candidate code must never receive the authority that creates those
credentials. Pull-request jobs must not use the live environment. Lane-local
cleanup and an independent `always()` cleanup job must delete every created
resource and revoke every disposable credential on success, failure, or
cancellation.

CLI releases have two reviewed Hub-trust postures. A production-enabled
release must contain the exact production Hub trust root, and every packaged
artifact must verify it. A dark release must contain no production Hub trust
root. Native local sharing uses the qURL native UDP protocol and does not embed
or require an HTTPS session-relay origin. It stops with the fixed, redacted
missing-settings error if the required built-in Hub trust root is absent or
invalid, unless a qURL administrator supplies a complete custom deployment
configuration. The release process rejects partial trust data and must never
embed development or test trust data. The GitHub Release stays draft and the
Homebrew tap stays on its prior version until all customer-journey, artifact,
image, trust-posture, and signature checks pass.

## Required production proof

After building the draft release, the workflow runs its exact packaged binary
against `https://api.layerv.ai` on Linux, macOS, and Windows. It creates a fresh
device identity and new local resources. Public and private publication, exact
backend content, private-access denial, resource reuse, stop/start/restart,
deletion, and independent credential/resource cleanup must pass. The candidate
uses its built-in production trust settings. Tests cannot skip or substitute a
sandbox endpoint or a rebuilt CI binary.

Publication requires all three platform results and cleanup to succeed. It
then checks SHA-256 receipts against the current draft archives and checksums.
Receipts must name this source commit, release tag, and workflow run, and must
come from the same attempt and be at most one hour old. Missing, stale, failed, or changed evidence blocks
both public image promotion and GitHub publication. Homebrew depends on that
publication. Recovery dispatches use the same gates. Cancellation also starts
an independent trusted cleanup workflow.

If a publication fails or its proof expires, rerun the whole release
continuation dispatch, or dispatch it again with the same exact tag and source
inputs. A failed-jobs-only retry cannot reuse evidence from an earlier attempt.
All continuations for one tag are serialized, including operator recovery, so
one build cannot replace the release assets while another run tests them.
GitHub retains only one pending continuation per tag; a newer queued dispatch
replaces an older pending dispatch, while the active run continues.

The `cli-production` environment admits only `main` and has no approval rule.
It needs its own production CI owner and the `QURL_JOURNEY_API_KEY` and
`QURL_JOURNEY_OWNER_ID` environment secrets, with the scopes and isolation in
[CLI automation credentials](docs/ci-api-keys.md). Missing configuration blocks
release; sandbox credentials must never be copied into it. The parent key is
used only by trusted setup and cleanup. Customer processes receive disposable
keys with no key-management authority.

This is proof of the listed production journeys on each native runner, not
proof of every CLI command or every platform architecture. API-dependent
features need their own production assertions before they can be treated as
verified. All six release archives are bound to the proven checksum set;
the existing package checks also remain required.

## Test an unreleased CLI without releasing it

When a CLI change needs platform features that are in sandbox but not yet in
production, keep the release PR open and test a candidate instead. Dispatch
`cli: Candidate Build` on a branch; it builds that branch's latest commit,
which the run summary names:

```bash
gh workflow run cli-candidate.yml -R layervai/qurl-integrations --ref <branch>
```

The branch must contain `.github/workflows/cli-candidate.yml`; merge `main`
into an older branch first. A dispatch runs the branch's own copy of that
file. Runs on one branch queue one at a time, and a third dispatch while one
runs and one waits replaces the waiting one.

It runs GoReleaser in snapshot mode and keeps the archives and
`checksums.txt` as a run artifact for 7 days. It creates no tag and no GitHub
Release, publishes no Homebrew cask, and holds only `contents: read`, so
`brew upgrade` and `install.sh` never see it. The candidate is unsigned and
its version is `<last release reachable from the branch>-SNAPSHOT-<commit>`. The run summary has the
download command. Run the unpacked `qurl` by its path, and point it at sandbox
with `QURL_ENDPOINT`, `QURL_DEPLOYMENT` and the all-or-none
`QURL_CONNECTOR_HUB_HOST`, `QURL_CONNECTOR_HUB_PORT` and
`QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64` triple. Without them it uses
production. Release once production has the platform the change needs.

## Production default activation after v2.5.0

v2.5.0 was intentionally published without a production Hub pin. Do not replace
its signed assets or move its tag. Future public releases require the production
pin; missing inputs block publication. Local/nightly snapshots can remain dark.

Before cutting the next CLI patch release:

1. Provision the production Hub through the governed Control rollout. Read only
   `/prod/nhp/control/hub/identity/public-key` in account235500187906,
   regionus-east-2. Verify its canonical32-byte X25519 public key and independently
   approve its SHA256 fingerprint against the production Hub identity. Never use
   the private key secret or a sandbox key.
2. Set the existing repository secrets `QURL_PROD_NHP_HUB_PUBLIC_KEY_B64` and
   `QURL_PROD_NHP_HUB_PUBLIC_KEY_SHA256` to those reviewed public values. Do not
   disable `QURL_REQUIRE_RELEASE_HUB_PIN` to pass a release.
3. Merge the normal CLI release-please patch PR for a new version after2.5.0.
   Its exact source must pass the existing CLI customer journey. The workflow
   creates a draft, injects the pin through GoReleaser, verifies all archives and
   image platforms, and updates Homebrew only after publication checks pass.
4. If that new draft needs continuation, dispatch `release-please.yml` on `main`
   with `cli_tag` set to the new tag, `source_sha` to its exact source, and
   `cli_run_id`/`cli_run_attempt` to the successful exact-source CLI workflow.
   These are evidence from the completed run, not placeholders to guess. Never
   dispatch repair for the already-public v2.5.0 assets to change their trust root.
5. Verify the new downloaded binary with `version --verify-release-native-trust`;
   require the independently reviewed fingerprint with all Hub overrides unset.
   Then run the real production login/publish/share/get journey in separate
   temporary native state. Registration and fixture writes wait for the approved
   production identity cutover; no existing user state needs to be reset.

## What a CLI release ships

Merging the CLI's release-please PR tags `vX.Y.Z` and creates a draft GitHub
Release. The `release-cli` job runs GoReleaser, verifies the result, publishes
the GitHub Release, and then updates Homebrew. Before GoReleaser, the job
checks that the qURL agent skill served to users is at least the version the
tagged CLI asks for. If that step fails, nothing has been uploaded: serve the
newer skill, then use **Re-run failed jobs**. A cache in front of the skill
may keep the old copy for a while, and the step fails until it does not. The
`cli: Served Skill` workflow runs the same check against `main` every day and
reports a failure without blocking anything. To see the answer on demand
before the tag exists, run `go run ./apps/cli/scripts/check-served-skill` on
`main` before merging the CLI's release PR. The release attaches:

| Asset | What it is |
|---|---|
| `qurl_<version>_<os>_<arch>.tar.gz` | Binary + LICENSE + man pages + completions for macOS and Linux |
| `qurl_<version>_windows_<arch>.zip` | `qurl.exe` + LICENSE + man pages + completions for Windows |
| `qurl_<version>_linux_<arch>.deb` / `.rpm` | Linux packages |
| `<release-archive>.sbom.json` | Per-archive SPDX 2.3 SBOM (syft), including Windows `.zip` archives |
| `checksums.txt` | SHA-256 manifest of every archive, package, and SBOM |
| `checksums.txt.sigstore.json` | Keyless Sigstore signature bundle over `checksums.txt` |
| `qurl-image.txt` | Exact tested multi-architecture `ghcr.io/layervai/qurl@sha256:...` image reference |
| `qurl-image-buildkit-manifest.json` | Signed binding from that index to both platforms' BuildKit provenance and SPDX statement digests |

The downloadable binary/package trust chain is deliberately single-rooted:
the signature covers `checksums.txt`, and `checksums.txt` enumerates every
archive, package, and archive SBOM. The OCI image is published after
GoReleaser, so `qurl-image.txt` is intentionally not in that manifest. Its
trust root is the independent keyless signature on the exact recorded digest
plus a signed manifest that binds that index to both platforms' BuildKit
provenance and SPDX statements by content digest.

## Verify a release

Requires [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
**v3.0+** (`brew install cosign`, or the official `.deb`/`.rpm`/binary
from the [releases page](https://github.com/sigstore/cosign/releases/latest)).
Releases are signed with cosign v3.0.6; v2.x verification of v3-emitted
bundles is not an exercised path, so keep the same major version.

Download `checksums.txt`, `checksums.txt.sigstore.json`, and your
artifact from the [release page](https://github.com/layervai/qurl-integrations/releases),
then:

```bash
# 1. Verify the checksum manifest's signature and signer identity.
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity 'https://github.com/layervai/qurl-integrations/.github/workflows/release-please.yml@refs/heads/main' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# 2. Verify your download against the now-trusted manifest.
sha256sum --check --ignore-missing checksums.txt   # Linux
shasum -a 256 --check --ignore-missing checksums.txt  # macOS
```

Step 1 prints `Verified OK` on success. A non-zero exit means the
manifest either isn't signed, wasn't signed by this repo's release
workflow running on `main`, or the Rekor transparency-log proof doesn't
check out — treat any of those as "do not install".

> **What the identity pins.** The signature is keyless (Sigstore): the
> release job exchanges its GitHub OIDC token for a short-lived Fulcio
> certificate, and the signing event is appended to the public Rekor
> transparency log, so a malicious re-sign cannot retroactively
> disappear from the audit trail. The certificate's identity binds the
> signature to (1) this exact repository, (2) the exact workflow file
> `release-please.yml`, and (3) a run from `refs/heads/main` — a fork,
> a different workflow that gains `id-token: write`, or a non-`main`
> run cannot mint a signature satisfying this recipe. The identity is
> an exact string rather than a regexp because the release job always
> runs from `main` (release tags here never self-trigger workflows —
> see the workflow header). The `release-cli` job re-runs this exact
> command as a post-release self-test, so the recipe and reality
> cannot silently drift; if the workflow file is ever renamed, this
> recipe and that self-test move in lockstep with the rename PR.

The bundle embeds the signature, certificate, and Rekor inclusion
proof, so step 1 also works offline (`--offline`).

## Verify the qurl container image

Download `qurl-image.txt` from the same CLI release. It contains one immutable
reference such as `ghcr.io/layervai/qurl@sha256:...`; guided Docker, Compose,
Kubernetes, ECS, and S3-origin installs render this digest rather than a tag.
Verify the exact reference before deploying it:

```bash
image="$(cat qurl-image.txt)"
printf '%s\n' "$image" | grep -Eq '^ghcr\.io/layervai/qurl@sha256:[0-9a-f]{64}$' || {
  echo "unexpected qurl image reference" >&2
  exit 1
}

identity='https://github.com/layervai/qurl-integrations/.github/workflows/release-please.yml@refs/heads/main'
issuer='https://token.actions.githubusercontent.com'

cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  "$image"
cosign verify-attestation \
  --type https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1 \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  "$image"
```

Both commands must succeed. The release workflow first smokes both
`linux/amd64` and `linux/arm64` manifests, promotes that same digest, attaches
BuildKit SLSA/SPDX statements to each platform, validates the exact OCI
manifest subjects and each statement's type and nonempty predicate, signs a
manifest of their exact blob digests against the
index, signs the index itself, and then runs these verification commands. The
release's `qurl-image-buildkit-manifest.json` is the human-readable predicate
for that signed binding. Do not replace the digest from `qurl-image.txt` with
the mutable release tag.

Recovery reruns never rebuild over an existing `vX.Y.Z` image tag. They resolve
the already-published digest, re-run both platform smokes, require both
platform binaries to carry the reviewed Hub-trust posture, require its BuildKit
provenance to name the checked-out release commit and canonical repository, and
finish signing/uploading that same digest. Recovery cannot overwrite an
existing versioned image. An ambiguous registry lookup fails closed instead of
treating the tag as absent.

## SBOMs

Every archive has a sibling `<archive>.sbom.json` on the release page:
an [SPDX 2.3](https://spdx.dev/) JSON document generated by
[syft](https://github.com/anchore/syft) enumerating every Go module in
that archive's binary. SBOM documents are listed in `checksums.txt`, so
the signature verification above covers them too. Inspect one with
`jq '.packages[] | .name + " " + .versionInfo'`, or feed it to any
SPDX-aware scanner (e.g. `grype sbom:./qurl_<version>_linux_amd64.tar.gz.sbom.json`).

## Homebrew, install.sh, Windows zip, deb/rpm

`brew install layervai/tap/qurl`, `scripts/install.sh`, and the Linux
packages all consume the same signed release: the cask pins the archive
SHA-256 published at release time, and `install.sh` verifies its
download against `checksums.txt` before installing. The manual recipe
above is for verifying those checksums' provenance — or any directly
downloaded artifact — yourself. Windows users download the architecture-
specific `.zip`, verify it against the same signed `checksums.txt`, extract
`qurl.exe`, and add its directory to their user `PATH`.
