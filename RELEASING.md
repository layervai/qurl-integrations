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
the GitHub Release, and then updates Homebrew. The release attaches:

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
