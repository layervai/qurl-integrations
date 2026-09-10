#!/usr/bin/env bash
set -euo pipefail

if (( $# == 0 )); then
  echo "usage: $0 ABSOLUTE_DIRECTORY [...]" >&2
  exit 64
fi

for directory in "$@"; do
  [[ "$directory" == /* && "$directory" != / ]] || {
    echo "sensitive cleanup target must be an absolute non-root path" >&2
    exit 65
  }

  if [[ -L "$directory" ]]; then
    rm -f -- "$directory"
  elif [[ -e "$directory" && ! -d "$directory" ]]; then
    echo "sensitive cleanup target is not a directory" >&2
    exit 65
  elif [[ -d "$directory" ]]; then
    # GNU shred is not available on hosted macOS, and overwrite semantics are
    # not reliable on every runner filesystem. Use it when present, then fall
    # back to portable removal. The absence check below is the hard contract.
    if command -v shred >/dev/null 2>&1; then
      find "$directory" -type f -exec shred -u -- {} + 2>/dev/null ||
        find "$directory" -type f -exec rm -f -- {} +
    else
      find "$directory" -type f -exec rm -f -- {} +
    fi
    find "$directory" -type l -exec rm -f -- {} +
    find "$directory" -depth -type d -exec rmdir -- {} +
  fi

  if [[ -e "$directory" || -L "$directory" ]]; then
    echo "sensitive cleanup target still exists" >&2
    exit 1
  fi
done
