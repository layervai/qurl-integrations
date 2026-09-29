#!/bin/sh
# qURL CLI installer
# Usage: curl -fsSL https://raw.githubusercontent.com/layervai/qurl-integrations/main/scripts/install.sh | sh
#
# Install location: $INSTALL_DIR when set (created if missing), otherwise
# /usr/local/bin when it is writable, otherwise ~/.local/bin. The installer
# never runs sudo, so it cannot stall on a password prompt: coding agents,
# CI jobs, and fresh machines without Homebrew all complete unattended. For a
# system-wide install, run the script itself with sudo.
#
# Release contract (see .github/workflows/release-please.yml): this monorepo
# tags most components with a prefix (slack-v*, discord-v*, ...), but CLI
# releases are the bare `v<semver>` tags, and GoReleaser publishes assets
# named qurl_<semver>_<os>_<arch>.tar.gz plus checksums.txt on them.
#
# The whole script is a main() invoked on the last line so a truncated
# `curl | sh` stream parses to completion or runs nothing.
set -eu

# download <asset-name> — fetch a release asset into TMP_DIR.
download() {
    curl -fsSL --retry 3 --connect-timeout 15 "${RELEASE_URL}/$1" -o "${TMP_DIR}/$1" || {
        echo "Error: Failed to download $1 from ${RELEASE_URL} — check that the version exists (on a just-published release, assets may still be uploading; retry in a few minutes)" >&2
        exit 1
    }
}

main() {
    REPO="layervai/qurl-integrations"
    BINARY="qurl"
    # An explicit INSTALL_DIR is honoured as given. The default prefers the
    # conventional system directory and falls back to the per-user one
    # instead of escalating (see the header).
    # _QURL_INSTALL_SYSTEM_DIR is a test seam (scripts/test-install.sh) so
    # the fallback can be exercised without touching the host's
    # /usr/local/bin; it is not a user-facing setting.
    SYSTEM_DIR="${_QURL_INSTALL_SYSTEM_DIR:-/usr/local/bin}"
    if [ -z "${INSTALL_DIR:-}" ]; then
        if [ -d "$SYSTEM_DIR" ] && [ -w "$SYSTEM_DIR" ]; then
            INSTALL_DIR="$SYSTEM_DIR"
        else
            INSTALL_DIR="${HOME:?HOME is not set; set INSTALL_DIR}/.local/bin"
        fi
    fi

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
    # must not depend on anything beyond curl and a POSIX shell. Each page's
    # payload is collapsed to a single line first (the API pretty-prints one
    # field per line), then split at '}' so each release's tag_name and
    # prerelease flag share a chunk (they sit between the author object's
    # closing brace and the next brace in the API's stable field order). A
    # release whose *name* contains a literal '}' splits its own chunk and
    # is skipped — harmless here since release names are the bare tags.
    # Prereleases are excluded twice over: by the prerelease flag, and by
    # rejecting hyphenated tags (semver spells prereleases v1.2.3-rc.1).
    #
    # Pagination matters in this monorepo: every component's releases share
    # the list, so a burst of other components' releases must not push the
    # CLI's tags out of a single page. Candidates accumulate across pages
    # (capped at 10 pages / 1000 releases; VERSION= is the escape hatch
    # beyond that) and the highest x.y.z wins, so neither release order nor
    # page boundaries can select a stale version. scripts/test-install.sh
    # pins all of this against fixture payloads in the API's pretty-printed
    # shape, compact JSON, and multi-page form.
    VERSION="${VERSION:-}"
    if [ -z "$VERSION" ]; then
        CANDIDATES=""
        PAGE=1
        while [ "$PAGE" -le 10 ]; do
            RELEASES_JSON=$(curl -fsSL --retry 3 --connect-timeout 15 "https://api.github.com/repos/${REPO}/releases?per_page=100&page=${PAGE}") || {
                echo "Error: Failed to query GitHub releases for ${REPO} (network or rate limit); set VERSION=<x.y.z> to skip the release query" >&2
                exit 1
            }
            PAGE_FLAT=$(printf '%s\n' "$RELEASES_JSON" | tr -d '\n\r')
            CANDIDATES="${CANDIDATES}
$(printf '%s\n' "$PAGE_FLAT" \
                | tr '}' '\n' \
                | grep '"prerelease": *false' \
                | grep -o '"tag_name": *"v[0-9][^"-]*"' \
                | sed -E 's/.*"v([^"]+)".*/\1/')"
            # A short page is the last page.
            RELEASE_COUNT=$(printf '%s\n' "$PAGE_FLAT" | tr '}' '\n' | grep -c '"tag_name"' || true)
            [ "$RELEASE_COUNT" -ge 100 ] || break
            PAGE=$((PAGE + 1))
        done
        if [ "$PAGE" -gt 10 ]; then
            echo "warning: stopped scanning after 1000 releases; set VERSION=<x.y.z> if the expected version is missing" >&2
        fi
        # The anchored grep defensively drops any tag that is not plain
        # x.y.z (e.g. build metadata) rather than letting the numeric
        # field-sort mis-rank it; empty input yields an empty VERSION.
        VERSION=$(printf '%s\n' "$CANDIDATES" \
            | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' \
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

    download "$ARCHIVE"

    # Verify checksum. No tool, no install: silently skipping verification
    # would defeat the point of shipping checksums. Trust model: checksums.txt
    # ships from the same release as the archive, so this verifies integrity
    # (corruption, truncation), not publisher provenance — real signing is
    # tracked in #943. awk field-equality avoids treating the archive name as
    # a regex (its dots would be wildcards).
    download checksums.txt
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

    # Install binary. Never escalate: an unwritable directory is an error
    # that names the fix, not a password prompt nobody may be there to answer.
    # umask 022: directories we create must not be group/other-writable.
    # The CLI keeps its identity under ~/.local/state and refuses to run
    # beneath a writable ancestor, and distributions whose default umask is
    # 002 (Ubuntu, Fedora) would otherwise leave ~/.local at 0775.
    (umask 022 && mkdir -p "$INSTALL_DIR") 2>/dev/null || true
    if ! [ -d "$INSTALL_DIR" ] || ! [ -w "$INSTALL_DIR" ]; then
        echo "Error: ${INSTALL_DIR} is not writable." >&2
        echo "  Set INSTALL_DIR to a directory you own (for example INSTALL_DIR=\"\$HOME/.local/bin\")," >&2
        echo "  or run the installer with sudo for a system-wide install." >&2
        exit 1
    fi
    chmod +x "${TMP_DIR}/${BINARY}"
    mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"

    echo "Installed qurl v${VERSION} to ${INSTALL_DIR}/${BINARY}"
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            echo ""
            echo "${INSTALL_DIR} is not on your PATH. Add it for this shell with:"
            echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
            echo "and add the same line to your shell profile to keep it."
            ;;
    esac
    echo ""
    echo "Get started (no account needed):"
    echo "  qurl publish http://127.0.0.1:3000"
    echo "  qurl --help"
}

main "$@"
