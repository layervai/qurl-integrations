#!/bin/sh
# qURL CLI installer
# Usage: curl -fsSL https://raw.githubusercontent.com/layervai/qurl-integrations/main/scripts/install.sh | sh
#
# Release contract (see .github/workflows/release-please.yml): this monorepo
# tags most components with a prefix (slack-v*, discord-v*, ...), but CLI
# releases are the bare `v<semver>` tags, and GoReleaser publishes assets
# named qurl_<semver>_<os>_<arch>.tar.gz plus checksums.txt on them.
set -eu

REPO="layervai/qurl-integrations"
BINARY="qurl"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# Detect OS and architecture
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

case "$OS" in
    linux|darwin) ;;
    *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# Determine version. VERSION may be provided by the caller ("0.2.0" or
# "v0.2.0"); otherwise pick the newest CLI release. `releases/latest` is
# useless here — it returns the newest release of ANY component — so list
# releases (newest first) and take the first bare `v<digit>` tag, which
# component-prefixed tags like slack-v0.4.0 can never match. Parsing with
# grep/sed instead of jq is deliberate: the installer must not have
# dependencies beyond curl and a POSIX shell. Releases are capped at the
# last 100; a CLI release older than 100 releases would need VERSION=...
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=100" \
        | grep -o '"tag_name": *"v[0-9][^"]*"' \
        | head -n 1 \
        | sed -E 's/.*"v([^"]+)".*/\1/')
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

curl -fsSL "${RELEASE_URL}/${ARCHIVE}" -o "${TMP_DIR}/${ARCHIVE}"

# Verify checksum. No tool, no install: silently skipping verification would
# defeat the point of shipping checksums.
curl -fsSL "${RELEASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt"
(cd "$TMP_DIR" && grep "  ${ARCHIVE}\$" checksums.txt > verify.txt) || {
    echo "Error: ${ARCHIVE} not found in checksums.txt" >&2
    exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$TMP_DIR" && sha256sum -c verify.txt --status) || {
        echo "Error: Checksum verification failed" >&2
        exit 1
    }
elif command -v shasum >/dev/null 2>&1; then
    (cd "$TMP_DIR" && shasum -a 256 -c verify.txt --status) || {
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
