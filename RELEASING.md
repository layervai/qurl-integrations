# Releasing and verifying releases

How qURL™ CLI releases are produced, what a release ships, and how to
verify a download before trusting it. Release *process* mechanics
(per-component tracks, tag shapes, recovery) live in the
[`.github/workflows/release-please.yml`](.github/workflows/release-please.yml)
header and [README → Releases](README.md#releases); this document is the
consumer-facing contract.

## What a CLI release ships

Merging the CLI's release-please PR tags `vX.Y.Z` and creates the GitHub
Release; the `release-cli` job then runs GoReleaser, which attaches:

| Asset | What it is |
|---|---|
| `qurl_<version>_<os>_<arch>.tar.gz` / `.zip` | Binary + LICENSE + man pages + completions (`windows` ships as `.zip`) |
| `qurl_<version>_linux_<arch>.deb` / `.rpm` | Linux packages |
| `qurl_<version>_<os>_<arch>.tar.gz.sbom.json` (`.zip.sbom.json` for windows) | Per-archive SPDX 2.3 SBOM (syft) |
| `checksums.txt` | SHA-256 manifest of every archive, package, and SBOM |
| `checksums.txt.sigstore.json` | Keyless Sigstore signature bundle over `checksums.txt` |

The trust chain is deliberately single-rooted, following
qurl-connector's sign-the-digest-once model: the signature covers
`checksums.txt`, and `checksums.txt` enumerates the SHA-256 of every
other asset. Verify the signature, then verify your download against the
manifest — nothing else needs its own signature.

## Production Hub trust pin

Official CLI releases embed the public X25519 identity of the production NHP
Hub. That pin is the only out-of-band trust root for native Connector
enrollment: DNS selects where to send packets, while the key proves the peer
is the production Hub before the Hub can assign a cell.

The release job reads the canonical padded-base64 key from the public
repository variable `QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64`. Before
GoReleaser starts, it decodes that value with the CLI's runtime decoder and
compares the SHA-256 of the decoded 32 bytes with
[`production-public-key.sha256`](apps/cli/internal/connector/hub/production-public-key.sha256).
The workflow fails closed if the variable or fingerprint is absent, malformed,
or mismatched; it never publishes an artifact that trusts an unreviewed key.

The production value originates at the NHP Control parameter
`/prod/nhp/control/hub/identity/public-key`. Provisioning or rotating it is a
two-repository review event: validate its canonical X25519 encoding, commit the
raw-key fingerprint here, set the repository variable to that exact public
value, and release only after both values match. Never substitute a cell server
key—the Hub is a separate trust root. Source, test, and snapshot builds remain
dark unless the complete `QURL_CONNECTOR_HUB_HOST`,
`QURL_CONNECTOR_HUB_PORT`, and `QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64`
custom-deployment triple is supplied.

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

## SBOMs

Every archive has a sibling `<archive>.sbom.json` on the release page:
an [SPDX 2.3](https://spdx.dev/) JSON document generated by
[syft](https://github.com/anchore/syft) enumerating every Go module in
that archive's binary. SBOM documents are listed in `checksums.txt`, so
the signature verification above covers them too. Inspect one with
`jq '.packages[] | .name + " " + .versionInfo'`, or feed it to any
SPDX-aware scanner (e.g. `grype sbom:./qurl_<version>_linux_amd64.tar.gz.sbom.json`).

## Homebrew, install.sh, deb/rpm

`brew install layervai/tap/qurl`, `scripts/install.sh`, and the Linux
packages all consume the same signed release: the cask pins the archive
SHA-256 published at release time, and `install.sh` verifies its
download against `checksums.txt` before installing. The manual recipe
above is for verifying those checksums' provenance — or any directly
downloaded artifact — yourself.
