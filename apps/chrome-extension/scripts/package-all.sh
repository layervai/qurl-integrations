#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

MODE="${1:-patch}"

usage() {
  cat <<'EOF'
Usage:
  ./scripts/package-all.sh                # bump patch version and package
  ./scripts/package-all.sh patch          # bump patch version and package
  ./scripts/package-all.sh minor          # bump minor version and package
  ./scripts/package-all.sh major          # bump major version and package
  ./scripts/package-all.sh 1.2.3          # set explicit version and package

Output:
  - release/  Clean unpacked extension directory
  - dist/     Chrome Web Store upload ZIP
EOF
}

case "$MODE" in
  -h|--help|help)
    usage
    exit 0
    ;;
  patch|minor|major)
    npm run "publish:$MODE"
    ;;
  *)
    if [[ "$MODE" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      node scripts/bump-version.js "$MODE"
      npm run package:release
    else
      echo "Invalid mode: $MODE" >&2
      echo >&2
      usage >&2
      exit 1
    fi
    ;;
esac

# Derive both name and version from package.json so this stays in lockstep with
# package-release.js (which names the ZIP "${pkg.name}-v${pkg.version}.zip").
PKG_NAME="$(node -p "require('./package.json').name")"
VERSION="$(node -p "require('./package.json').version")"
ZIP_PATH="$PROJECT_ROOT/dist/${PKG_NAME}-v${VERSION}.zip"

echo
echo "Done."
echo "Version: $VERSION"
echo "Release directory: $PROJECT_ROOT/release"
echo "ZIP package: $ZIP_PATH"
