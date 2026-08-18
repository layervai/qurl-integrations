#!/bin/sh
# Generate shell completion files for the qurl CLI into completions/, for
# inclusion in release archives. Runs as a .goreleaser.yml before hook, after
# the hook that builds /tmp/qurl-gendocs. The Homebrew cask installs these by
# exact path (the old formula generated them at install time instead, which
# casks cannot do) — apps/cli/cmd/goreleaser_contract_test.go pins the file
# names, so change them here and there together.
set -eu

gendocs="${1:-/tmp/qurl-gendocs}"
outdir="${2:-completions}"

rm -rf "$outdir"
mkdir -p "$outdir"

for shell in bash zsh fish; do
  "$gendocs" completion "$shell" > "$outdir/qurl.$shell"
done
