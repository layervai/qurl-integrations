#!/bin/sh
# QURL CLI installer
# Usage: curl -fsSL https://raw.githubusercontent.com/layervai/qurl-integrations/main/scripts/install.sh | sh
set -e

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

# Get latest version
if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"v?([^"]+)".*/\1/')
    if [ -z "$VERSION" ]; then
        echo "Error: Could not determine latest version" >&2
        exit 1
    fi
fi

ARCHIVE="qurl_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${VERSION}/${ARCHIVE}"

echo "Installing qurl v${VERSION} (${OS}/${ARCH})..."

# Download and extract
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "$URL" -o "${TMP_DIR}/${ARCHIVE}"

# Verify checksum
CHECKSUM_URL="https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"
curl -fsSL "$CHECKSUM_URL" -o "${TMP_DIR}/checksums.txt"
(cd "$TMP_DIR" && grep "${ARCHIVE}" checksums.txt > "${TMP_DIR}/verify.txt") || {
    echo "Error: Archive not found in checksums.txt" >&2
    exit 1
}
VERIFIED=0
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$TMP_DIR" && sha256sum -c verify.txt --status) && VERIFIED=1
elif command -v shasum >/dev/null 2>&1; then
    (cd "$TMP_DIR" && shasum -a 256 -c verify.txt --status) && VERIFIED=1
else
    echo "Warning: No sha256sum or shasum found, skipping checksum verification" >&2
    VERIFIED=1
fi
if [ "$VERIFIED" -ne 1 ]; then
    echo "Error: Checksum verification failed" >&2
    exit 1
fi

tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR"

# Install binary
if [ -w "$INSTALL_DIR" ]; then
    mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
else
    echo "Installing to ${INSTALL_DIR} (requires sudo)..."
    sudo mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
fi

chmod +x "${INSTALL_DIR}/${BINARY}"

echo "Installed qurl v${VERSION} to ${INSTALL_DIR}/${BINARY}"
echo ""
echo "Get started:"
echo "  qurl config set api_key <your-api-key>"
echo "  qurl create https://example.com"
echo "  qurl --help"
