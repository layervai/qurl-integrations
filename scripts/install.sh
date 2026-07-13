#!/bin/sh
# qURL CLI installer
# Usage: curl -fsSL https://raw.githubusercontent.com/layervai/qurl-integrations/main/scripts/install.sh | sh
#
# Release contract (see .github/workflows/release-please.yml): this monorepo
# tags most components with a prefix (slack-v*, discord-v*, ...), but CLI
# releases are the bare `v<semver>` tags, and GoReleaser publishes assets
# named qurl_<semver>_<os>_<arch>.tar.gz plus checksums.txt on them.
#
# The whole script is a main() invoked on the last line so a truncated
# `curl | sh` stream parses to completion or runs nothing.
set -eu

main() {
    REPO="layervai/qurl-integrations"
    BINARY="qurl"
    INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

    # Detect OS and architecture
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m)"

    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) echo "Error: Unsupported architecture: $ARCH" >&2; exit 1 ;;
    esac

    case "$OS" in
        linux|darwin) ;;
        *) echo "Error: Unsupported OS: $OS" >&2; exit 1 ;;
    esac

    # Determine version. VERSION may be provided by the caller ("0.2.0" or
    # "v0.2.0"); otherwise pick the highest-versioned non-prerelease bare
    # `v<digit>` tag, which component-prefixed tags like slack-v0.4.0 can
    # never match. `releases/latest` is useless here — it returns the newest
    # release of ANY component. Candidates are max-selected by numeric
    # x.y.z sort rather than API order, so a backport released after a
    # newer version can never downgrade an install (sort -V is not on stock
    # macOS; per-field numeric sort is POSIX). Drafts never appear to
    # unauthenticated callers.
    #
    # Parsing with tr/grep/sed instead of jq is deliberate — the installer
    # must not depend on anything beyond curl and a POSIX shell. The payload
    # is collapsed to a single line first (the API pretty-prints one field
    # per line), then split at '}' so each release's tag_name and prerelease
    # flag share a chunk (they sit between the author object's closing brace
    # and the next brace in the API's stable field order). A release whose
    # *name* contains a literal '}' splits its own chunk and is skipped —
    # harmless here since release names are the bare tags. Prereleases are
    # excluded twice over: by the prerelease flag, and by rejecting
    # hyphenated tags (semver spells prereleases v1.2.3-rc.1).
    # scripts/test-install.sh pins all of this against fixture payloads in
    # both the API's pretty-printed shape and compact JSON. Only the newest
    # 100 releases are scanned; anything older needs an explicit VERSION=...
    VERSION="${VERSION:-}"
    if [ -z "$VERSION" ]; then
        RELEASES_JSON=$(curl -fsSL --retry 3 --connect-timeout 15 "https://api.github.com/repos/${REPO}/releases?per_page=100") || {
            echo "Error: Failed to query GitHub releases for ${REPO} (network or rate limit); set VERSION=<x.y.z> to skip the release query" >&2
            exit 1
        }
        VERSION=$(printf '%s\n' "$RELEASES_JSON" \
            | tr -d '\n\r' \
            | tr '}' '\n' \
            | grep '"prerelease": *false' \
            | grep -o '"tag_name": *"v[0-9][^"-]*"' \
            | sed -E 's/.*"v([^"]+)".*/\1/' \
            | sort -t. -k1,1n -k2,2n -k3,3n \
            | tail -n 1)
        if [ -z "$VERSION" ]; then
            echo "Error: Could not find a CLI release (bare v* tag) for ${REPO}" >&2
            exit 1
        fi
    fi
    VERSION="${VERSION#v}"

    ARCHIVE="qurl_${VERSION}_${OS}_${ARCH}.tar.gz"
    RELEASE_URL="https://github.com/${REPO}/releases/download/v${VERSION}"

    echo "Installing qurl v${VERSION} (${OS}/${ARCH})..."

    # Download and extract
    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT

    curl -fsSL --retry 3 --connect-timeout 15 "${RELEASE_URL}/${ARCHIVE}" -o "${TMP_DIR}/${ARCHIVE}" || {
        echo "Error: Failed to download ${ARCHIVE} — on a just-published release, assets may still be uploading; retry in a few minutes" >&2
        exit 1
    }

    # Verify checksum. No tool, no install: silently skipping verification
    # would defeat the point of shipping checksums. awk field-equality avoids
    # treating the archive name as a regex (its dots would be wildcards).
    curl -fsSL --retry 3 --connect-timeout 15 "${RELEASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt" || {
        echo "Error: Failed to download checksums.txt — on a just-published release, assets may still be uploading; retry in a few minutes" >&2
        exit 1
    }
    awk -v f="$ARCHIVE" '$2 == f' "${TMP_DIR}/checksums.txt" > "${TMP_DIR}/verify.txt"
    if ! [ -s "${TMP_DIR}/verify.txt" ]; then
        echo "Error: ${ARCHIVE} not found in checksums.txt" >&2
        exit 1
    fi
    # Quiet via redirect rather than --status/-s: BusyBox and GNU spell the
    # flag differently, and options after operands break strict-POSIX getopt.
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$TMP_DIR" && sha256sum -c verify.txt) >/dev/null 2>&1 || {
            echo "Error: Checksum verification failed" >&2
            exit 1
        }
    elif command -v shasum >/dev/null 2>&1; then
        (cd "$TMP_DIR" && shasum -a 256 -c verify.txt) >/dev/null 2>&1 || {
            echo "Error: Checksum verification failed" >&2
            exit 1
        }
    else
        echo "Error: Neither sha256sum nor shasum is available; refusing to install unverified binaries" >&2
        echo "  Install coreutils (sha256sum) or perl (shasum) and retry." >&2
        exit 1
    fi

    tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR"

    # Install binary. chmod before the move: the target may end up root-owned.
    chmod +x "${TMP_DIR}/${BINARY}"
    if [ -w "$INSTALL_DIR" ]; then
        mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
    else
        echo "Installing to ${INSTALL_DIR} (requires sudo)..."
        sudo mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
    fi

    echo "Installed qurl v${VERSION} to ${INSTALL_DIR}/${BINARY}"
    echo ""
    echo "Get started:"
    echo "  qurl config set api_key <your-api-key>"
    echo "  qurl create https://example.com"
    echo "  qurl --help"
}

main "$@"
