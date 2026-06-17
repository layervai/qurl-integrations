#!/usr/bin/env bash
# Unit-style coverage for qurl-origin-cachectl. The Docker behavior test proves
# nginx and cachectl agree in a live container; this suite pins the path
# normalization, alias expansion, safe-dir rails, and idempotence directly.
set -uo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$DIR/cachectl.sh"
TMPROOT="${TMPDIR:-/tmp}"
case "$TMPROOT" in /tmp|/tmp/*) ;; *) TMPROOT=/tmp ;; esac
ROOT="$(mktemp -d "$TMPROOT/qurl-cachectl.XXXXXX")"
CACHE_DIR="$ROOT/cache"
CACHE_KEY_SCHEME="http"
CACHE_KEY_PROXY_HOST="envoy_upstream"

cleanup() { rm -rf "$ROOT"; }
trap cleanup EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf 'FAIL  %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else no "$1 (got '$2', want '$3')"; fi; }
expect_file() { if [ -f "$1" ]; then ok "$2"; else no "$2 (missing $1)"; fi; }
expect_no_file() { if [ ! -f "$1" ]; then ok "$2"; else no "$2 (still exists $1)"; fi; }
expect_contains() {
  case "$2" in
    *"$3"*) ok "$1" ;;
    *) no "$1 (missing '$3' in '$2')" ;;
  esac
}

json_num() {
  sed -n "s/.*\"$1\":\([0-9][0-9]*\).*/\1/p"
}

json_str() {
  sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
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
  echo "md5sum or md5 is required for this test" >&2
  exit 2
}

cache_file_for_key() {
  digest="$(printf '%s' "$1" | md5_hex)"
  leaf="$(printf '%s' "$digest" | sed 's/^.*\(.\)$/\1/')"
  branch="$(printf '%s' "$digest" | sed 's/^.*\(..\).$/\1/')"
  printf '%s/%s/%s/%s\n' "$CACHE_DIR" "$leaf" "$branch" "$digest"
}

cache_key() {
  printf '%s%s%s%s' "$1" "$CACHE_KEY_SCHEME" "$CACHE_KEY_PROXY_HOST" "$2"
}

seed_entry() {
  file="$(cache_file_for_key "$(cache_key "$1" "$2")")"
  mkdir -p "$(dirname "$file")"
  printf '%s:%s\n' "$1" "$2" > "$file"
}

entry_file() {
  cache_file_for_key "$(cache_key "$1" "$2")"
}

reset_cache() {
  rm -rf "$CACHE_DIR"
  mkdir -p "$CACHE_DIR"
}

run_ctl() {
  env CACHE_DIR="$CACHE_DIR" \
    CACHE_KEY_SCHEME="$CACHE_KEY_SCHEME" \
    CACHE_KEY_PROXY_HOST="$CACHE_KEY_PROXY_HOST" \
    sh "$SCRIPT" "$@"
}

run_ctl_prefix() {
  env CACHE_DIR="$CACHE_DIR" \
    CACHE_KEY_SCHEME="$CACHE_KEY_SCHEME" \
    CACHE_KEY_PROXY_HOST="$CACHE_KEY_PROXY_HOST" \
    S3_PREFIX="$1" \
    sh "$SCRIPT" "${@:2}"
}

run_ctl_connector() {
  env CACHE_DIR="$CACHE_DIR" \
    CACHE_KEY_SCHEME="$CACHE_KEY_SCHEME" \
    CACHE_KEY_PROXY_HOST="$CACHE_KEY_PROXY_HOST" \
    CACHE_CONNECTOR_ID="$1" \
    CACHE_REPLICA_ID="$2" \
    sh "$SCRIPT" "${@:3}"
}

run_ctl_qurl_connector() {
  env CACHE_DIR="$CACHE_DIR" \
    CACHE_KEY_SCHEME="$CACHE_KEY_SCHEME" \
    CACHE_KEY_PROXY_HOST="$CACHE_KEY_PROXY_HOST" \
    QURL_CONNECTOR_ID="$1" \
    CACHE_REPLICA_ID="$2" \
    sh "$SCRIPT" "${@:3}"
}

run_ctl_at_connector() {
  env CACHE_DIR="$1" \
    CACHE_KEY_SCHEME="$CACHE_KEY_SCHEME" \
    CACHE_KEY_PROXY_HOST="$CACHE_KEY_PROXY_HOST" \
    CACHE_CONNECTOR_ID="$2" \
    CACHE_REPLICA_ID="$3" \
    sh "$SCRIPT" "${@:4}"
}

expect_purge_rejected() {
  label="$1"
  unsafe_dir="$2"
  out="$(env CACHE_DIR="$unsafe_dir" sh "$SCRIPT" purge 2>&1)"
  code=$?
  expect_eq "$label exit" "$code" 2
  case "$out" in
    *"Refusing unsafe CACHE_DIR"*) ok "$label message" ;;
    *) no "$label message (got '$out')" ;;
  esac
}

# status is read-only and reports zero for a missing cache directory.
rm -rf "$CACHE_DIR"
out="$(run_ctl status)"
expect_eq "status missing cache dir" "$(printf '%s\n' "$out" | json_num entries)" 0

out="$(run_ctl_connector stats-connector origin-a status)"
expect_eq "status includes connector id" "$(printf '%s\n' "$out" | json_str connector_id)" stats-connector
expect_eq "status includes replica id" "$(printf '%s\n' "$out" | json_str replica_id)" origin-a

out="$(run_ctl_qurl_connector fallback-connector origin-b status)"
expect_eq "status falls back to QURL_CONNECTOR_ID" "$(printf '%s\n' "$out" | json_str connector_id)" fallback-connector

special_connector='stats"quoted\slash'
special_replica="$(printf 'origin-a\tline\nnext\bback\fpage\abel\rcr')"
out="$(run_ctl_connector "$special_connector" "$special_replica" status)"
expect_contains "status escapes connector metadata" "$out" '"connector_id":"stats\"quoted\\slash"'
expect_contains "status escapes replica metadata" "$out" '"replica_id":"origin-a\tline\nnext\bback\fpage\u0007bel\rcr"'

# Targeted purge removes only the GET/HEAD entries for the normalized viewer
# path. Query strings and fragments are ignored because they are not in the
# nginx cache key.
reset_cache
seed_entry GET /cacheprobe.json
seed_entry HEAD /cacheprobe.json
seed_entry GET /metrics.json
out="$(run_ctl purge '/cacheprobe.json?_t=1#frag')"
expect_eq "targeted purge removes GET+HEAD" "$(printf '%s\n' "$out" | json_num entries_removed)" 2
expect_no_file "$(entry_file GET /cacheprobe.json)" "targeted purge removed GET cacheprobe"
expect_no_file "$(entry_file HEAD /cacheprobe.json)" "targeted purge removed HEAD cacheprobe"
expect_file "$(entry_file GET /metrics.json)" "targeted purge preserves unrelated metrics"
out="$(run_ctl purge /cacheprobe.json)"
expect_eq "targeted purge is idempotent" "$(printf '%s\n' "$out" | json_num entries_removed)" 0

# Connector-scoped purge is still a local replica operation, but it fails closed
# unless deploy automation addresses the connector this replica declares. This is
# the contract that lets an orchestrator fan the same purge out across active
# replicas without treating them as separate connector IDs.
reset_cache
seed_entry GET /cacheprobe.json
seed_entry HEAD /cacheprobe.json
seed_entry GET /metrics.json
out="$(run_ctl_connector stats-connector origin-a purge-connector stats-connector /cacheprobe.json)"
expect_eq "connector purge reports connector scope" "$(printf '%s\n' "$out" | json_str scope)" connector
expect_eq "connector purge reports connector id" "$(printf '%s\n' "$out" | json_str connector_id)" stats-connector
expect_eq "connector purge reports replica id" "$(printf '%s\n' "$out" | json_str replica_id)" origin-a
expect_eq "connector purge removes GET+HEAD" "$(printf '%s\n' "$out" | json_num entries_removed)" 2
expect_no_file "$(entry_file GET /cacheprobe.json)" "connector purge removed GET cacheprobe"
expect_no_file "$(entry_file HEAD /cacheprobe.json)" "connector purge removed HEAD cacheprobe"
expect_file "$(entry_file GET /metrics.json)" "connector purge preserves unrelated path"

reset_cache
seed_entry GET /cacheprobe.json
out="$(run_ctl_connector stats-connector origin-a purge-connector other-connector /cacheprobe.json 2>&1)"
code=$?
expect_eq "connector purge rejects mismatched connector exit" "$code" 3
case "$out" in
  *"Refusing connector-scoped purge for other-connector"*) ok "connector purge rejects mismatched connector message" ;;
  *) no "connector purge rejects mismatched connector message (got '$out')" ;;
esac
expect_file "$(entry_file GET /cacheprobe.json)" "connector mismatch preserves cache entry"

reset_cache
seed_entry GET /cacheprobe.json
out="$(run_ctl purge-connector stats-connector /cacheprobe.json 2>&1)"
code=$?
expect_eq "connector purge rejects unlabeled replica exit" "$code" 3
case "$out" in
  *"CACHE_CONNECTOR_ID is not set"*) ok "connector purge rejects unlabeled replica message" ;;
  *) no "connector purge rejects unlabeled replica message (got '$out')" ;;
esac
expect_file "$(entry_file GET /cacheprobe.json)" "unlabeled replica preserves cache entry"

replica_a="$ROOT/cache-replica-a"
replica_b="$ROOT/cache-replica-b"
CACHE_DIR="$replica_a"; reset_cache; seed_entry GET /metrics.json
CACHE_DIR="$replica_b"; reset_cache; seed_entry GET /metrics.json
out="$(run_ctl_at_connector "$replica_a" stats-connector origin-a purge-connector stats-connector /metrics.json)"
expect_eq "connector fanout result names first replica" "$(printf '%s\n' "$out" | json_str replica_id)" origin-a
CACHE_DIR="$replica_a"; expect_no_file "$(entry_file GET /metrics.json)" "connector purge removed first replica entry"
CACHE_DIR="$replica_b"; expect_file "$(entry_file GET /metrics.json)" "connector purge does not mutate peer replica cache"
CACHE_DIR="$ROOT/cache"; reset_cache

# Object-style root index invalidation covers both possible viewer cache keys.
reset_cache
seed_entry GET /
seed_entry HEAD /
seed_entry GET /index.html
seed_entry HEAD /index.html
out="$(run_ctl purge /index.html)"
expect_eq "root index alias purge removes four entries" "$(printf '%s\n' "$out" | json_num entries_removed)" 4
expect_no_file "$(entry_file GET /)" "root alias removed GET /"
expect_no_file "$(entry_file GET /index.html)" "root alias removed GET /index.html"

# Object-style nested index invalidation covers clean URL and trailing-slash
# aliases without touching sibling assets.
reset_cache
for path in /website/index.html /website /website/; do
  seed_entry GET "$path"
  seed_entry HEAD "$path"
done
seed_entry GET /website/app.js
out="$(run_ctl purge /website/index.html)"
expect_eq "nested index alias purge removes clean URL variants" "$(printf '%s\n' "$out" | json_num entries_removed)" 6
for path in /website/index.html /website /website/; do
  expect_no_file "$(entry_file GET "$path")" "nested alias removed GET $path"
  expect_no_file "$(entry_file HEAD "$path")" "nested alias removed HEAD $path"
done
expect_file "$(entry_file GET /website/app.js)" "nested alias preserves sibling asset"

# If deploy automation passes an S3 object path while S3_PREFIX is set, cachectl
# strips the prefix and still purges the viewer-path cache keys nginx uses.
reset_cache
for path in /website/index.html /website /website/; do
  seed_entry GET "$path"
  seed_entry HEAD "$path"
done
seed_entry GET /site/other/index.html
out="$(run_ctl_prefix site purge /site/website/index.html)"
expect_eq "S3_PREFIX object path purges viewer aliases" "$(printf '%s\n' "$out" | json_num entries_removed)" 6
for path in /website/index.html /website /website/; do
  expect_no_file "$(entry_file GET "$path")" "S3_PREFIX removed GET $path"
  expect_no_file "$(entry_file HEAD "$path")" "S3_PREFIX removed HEAD $path"
done
expect_file "$(entry_file GET /site/other/index.html)" "S3_PREFIX preserves unrelated object"

# Full purge removes all files below the cache directory but leaves the cache
# directory itself available for nginx to reuse.
reset_cache
seed_entry GET /a.json
seed_entry GET /b.json
mkdir -p "$CACHE_DIR/empty/nested"
out="$(run_ctl purge)"
expect_eq "full purge reports removed files" "$(printf '%s\n' "$out" | json_num entries_removed)" 2
out="$(run_ctl status)"
expect_eq "full purge leaves zero files" "$(printf '%s\n' "$out" | json_num entries)" 0
if [ -d "$CACHE_DIR" ]; then ok "full purge keeps cache dir"; else no "full purge removed cache dir"; fi

# Rails for accidental destructive settings mirror the sibling repo purge
# pattern: unsafe or ambiguous mutation targets fail closed.
expect_purge_rejected "reject root tmp" /tmp
expect_purge_rejected "reject non-tmp path" /var/tmp/qurl-cachectl
expect_purge_rejected "reject traversal" /tmp/qurl-cachectl/../escape
ln -s /etc "$ROOT/link-to-etc"
expect_purge_rejected "reject symlink component" "$ROOT/link-to-etc/cache"

echo "-------------------------------------------"
echo "cachectl: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
