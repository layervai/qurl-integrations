#!/usr/bin/env bash
set -euo pipefail

shopt -s nullglob

status=0
sha_re='^[0-9a-f]{40}$'
tag_re='^v[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$'
workflow_files=(.github/workflows/*.yml .github/workflows/*.yaml)

fail() {
  printf '%s:%s: %s\n' "$1" "$2" "$3" >&2
  status=1
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

for file in "${workflow_files[@]}"; do
  line_no=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line_no=$((line_no + 1))

    if [[ ! "$line" =~ ^[[:space:]]*(-[[:space:]]*)?uses:[[:space:]]*([^[:space:]#]+)([[:space:]]*#(.*))?$ ]]; then
      continue
    fi

    uses_ref="${BASH_REMATCH[2]}"
    comment="$(trim "${BASH_REMATCH[4]:-}")"

    if [[ "$uses_ref" == ./* ]]; then
      continue
    fi

    if [[ "$uses_ref" != *@* ]]; then
      fail "$file" "$line_no" "external action is missing an @ ref: $uses_ref"
      continue
    fi

    pin="${uses_ref##*@}"
    if [[ ! "$pin" =~ $sha_re ]]; then
      fail "$file" "$line_no" "external action must be pinned to a 40-character commit SHA: $uses_ref"
    fi

    if [[ ! "$comment" =~ $tag_re ]]; then
      fail "$file" "$line_no" "SHA pin must include an exact version tag comment like '# v1.2.3': $uses_ref"
    fi
  done < "$file"
done

if (( status == 0 )); then
  printf 'GitHub Actions pins are SHA-pinned with exact version comments.\n'
fi

exit "$status"
