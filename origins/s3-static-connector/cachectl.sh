#!/bin/sh
# Local cache-control helper for deploy automation. This is intentionally a
# container command, not an HTTP endpoint, so qURL viewers cannot purge cache.
set -eu

CACHE_DIR="${CACHE_DIR:-/tmp/s3cache}"
CACHE_KEY_METHODS="${CACHE_KEY_METHODS:-GET HEAD}"
# Must match templates/nginx.conf.template's proxy_cache_key:
# "$request_method$scheme$proxy_host$uri".
CACHE_KEY_SCHEME="${CACHE_KEY_SCHEME:-http}"
CACHE_KEY_PROXY_HOST="${CACHE_KEY_PROXY_HOST:-envoy_upstream}"
INDEX_DOCUMENT="${INDEX_DOCUMENT:-index.html}"
S3_PREFIX="${S3_PREFIX:-}"
CACHE_CONNECTOR_ID="${CACHE_CONNECTOR_ID:-${QURL_CONNECTOR_ID:-}}"
CACHE_REPLICA_ID="${CACHE_REPLICA_ID:-$(hostname 2>/dev/null || true)}"

usage() {
  cat >&2 <<'EOF'
usage: qurl-origin-cachectl <status|purge [path ...]|purge-connector <connector-id> [path ...]>

Commands:
  status   Print a small JSON summary of the nginx proxy cache directory.
  purge    Remove all cache entries, or only entries for the given viewer paths.
  purge-connector
           Remove entries for this local replica only after confirming the
           requested connector matches CACHE_CONNECTOR_ID.
EOF
}

json_escape() {
  printf '%s' "$1" | awk '
    BEGIN {
      ORS = ""
      for (i = 1; i < 32; i++) {
        control = sprintf("%c", i)
        escaped[control] = sprintf("\\u%04x", i)
      }
      escaped[sprintf("%c", 8)] = "\\b"
      escaped[sprintf("%c", 9)] = "\\t"
      escaped[sprintf("%c", 12)] = "\\f"
      escaped[sprintf("%c", 13)] = "\\r"
    }
    {
      if (NR > 1) {
        printf "\\n"
      }
      for (pos = 1; pos <= length($0); pos++) {
        c = substr($0, pos, 1)
        if (c == "\\") {
          printf "%s", "\\\\"
        } else if (c == "\"") {
          printf "%s", "\\\""
        } else if (c in escaped) {
          printf "%s", escaped[c]
        } else {
          printf "%s", c
        }
      }
    }
  '
}

json_common_fields() {
  printf ',"cache_dir":"%s"' "$(json_escape "$CACHE_DIR")"
  if [ -n "$CACHE_CONNECTOR_ID" ]; then
    printf ',"connector_id":"%s"' "$(json_escape "$CACHE_CONNECTOR_ID")"
  fi
  if [ -n "$CACHE_REPLICA_ID" ]; then
    printf ',"replica_id":"%s"' "$(json_escape "$CACHE_REPLICA_ID")"
  fi
}

assert_safe_cache_dir() {
  case "$CACHE_DIR" in
    /tmp/*) ;;
    *)
      echo "Refusing unsafe CACHE_DIR: $CACHE_DIR (must be under /tmp)" >&2
      exit 2
      ;;
  esac

  case "$CACHE_DIR" in
    /tmp/|/tmp/.|/tmp/..|*/../*|*/..|*/./*|*/.)
      echo "Refusing unsafe CACHE_DIR: $CACHE_DIR" >&2
      exit 2
      ;;
  esac

  probe="$CACHE_DIR"
  while [ "$probe" != "/tmp" ] && [ "$probe" != "/" ]; do
    if [ -L "$probe" ]; then
      echo "Refusing unsafe CACHE_DIR symlink: $probe" >&2
      exit 2
    fi
    probe="$(dirname "$probe")"
  done
}

assert_connector_scope() {
  requested="$1"
  if [ -z "$requested" ]; then
    usage
    exit 2
  fi
  if [ -z "$CACHE_CONNECTOR_ID" ]; then
    echo "Refusing connector-scoped purge for $requested: CACHE_CONNECTOR_ID is not set on this origin replica" >&2
    exit 3
  fi
  if [ "$CACHE_CONNECTOR_ID" != "$requested" ]; then
    echo "Refusing connector-scoped purge for $requested: this origin replica serves $CACHE_CONNECTOR_ID" >&2
    exit 3
  fi
}

entry_count() {
  if [ ! -d "$CACHE_DIR" ]; then
    printf '0'
    return
  fi
  find "$CACHE_DIR" -type f -print | wc -l | tr -d ' '
}

normalize_s3_prefix() {
  p="$S3_PREFIX"
  while [ "${p#/}" != "$p" ]; do p="${p#/}"; done
  while [ "${p%/}" != "$p" ]; do p="${p%/}"; done
  if [ -n "$p" ]; then
    printf '/%s' "$p"
  fi
}

normalize_path() {
  p="${1%%\#*}"
  p="${p%%\?*}"
  case "$p" in
    "") printf '/\n' ;;
    /*) printf '%s\n' "$p" ;;
    *) printf '/%s\n' "$p" ;;
  esac
}

emit_path_candidates() {
  raw="$(normalize_path "$1")"
  prefix="$(normalize_s3_prefix)"

  {
    printf '%s\n' "$raw"

    if [ -n "$prefix" ]; then
      case "$raw" in
        "$prefix") printf '/\n' ;;
        "$prefix"/*) printf '%s\n' "${raw#"$prefix"}" ;;
      esac
    fi
  } | while IFS= read -r p; do
    printf '%s\n' "$p"
    suffix="/$INDEX_DOCUMENT"
    case "$p" in
      "$suffix")
        printf '/\n'
        ;;
      *"$suffix")
        dir="${p%"$suffix"}"
        [ -n "$dir" ] || dir="/"
        printf '%s\n' "$dir"
        case "$dir" in
          */) ;;
          *) printf '%s/\n' "$dir" ;;
        esac
        ;;
    esac
  done
}

md5_hex() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum | awk '{print $1}'
    return
  fi
  if command -v md5 >/dev/null 2>&1; then
    md5 -q
    return
  fi
  echo "md5sum or md5 is required for targeted cache purge" >&2
  exit 2
}

cache_file_for_key() {
  # Mirrors nginx `proxy_cache_path levels=1:2` for targeted local purges.
  digest="$(printf '%s' "$1" | md5_hex)"
  leaf="$(printf '%s' "$digest" | sed 's/^.*\(.\)$/\1/')"
  branch="$(printf '%s' "$digest" | sed 's/^.*\(..\).$/\1/')"
  printf '%s/%s/%s/%s\n' "$CACHE_DIR" "$leaf" "$branch" "$digest"
}

purge_one_path() {
  path_removed=0
  candidates="$(emit_path_candidates "$1")"
  while IFS= read -r candidate; do
    for method in $CACHE_KEY_METHODS; do
      key="${method}${CACHE_KEY_SCHEME}${CACHE_KEY_PROXY_HOST}${candidate}"
      file="$(cache_file_for_key "$key")"
      if [ -f "$file" ]; then
        rm -f "$file"
        path_removed=$((path_removed + 1))
        rmdir "$(dirname "$file")" "$(dirname "$(dirname "$file")")" 2>/dev/null || true
      fi
    done
  done <<EOF
$candidates
EOF
  printf '%s\n' "$path_removed"
}

purge_cache() {
  removed=0
  mkdir -p "$CACHE_DIR"
  if [ "$#" -eq 0 ]; then
    removed="$(entry_count)"
    find "$CACHE_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  else
    for path in "$@"; do
      path_removed="$(purge_one_path "$path")"
      removed=$((removed + path_removed))
    done
  fi
}

case "${1:-}" in
  status)
    printf '{"layer":"origin","msg":"cache_status"'
    json_common_fields
    printf ',"entries":%s}\n' "$(entry_count)"
    ;;
  purge)
    assert_safe_cache_dir
    shift
    purge_cache "$@"
    printf '{"layer":"origin","msg":"cache_purged","scope":"local"'
    json_common_fields
    printf ',"entries_removed":%s}\n' "$removed"
    ;;
  purge-connector)
    assert_safe_cache_dir
    requested_connector="${2:-}"
    assert_connector_scope "$requested_connector"
    shift 2
    purge_cache "$@"
    printf '{"layer":"origin","msg":"cache_purged","scope":"connector"'
    json_common_fields
    printf ',"entries_removed":%s}\n' "$removed"
    ;;
  *)
    usage
    exit 2
    ;;
esac
