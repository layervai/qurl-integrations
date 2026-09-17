#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
subject=$root/scripts/remove-sensitive-tree.sh
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

outside=$temporary/outside
mkdir "$outside"
printf 'retain\n' >"$outside/sentinel"

target=$temporary/target
mkdir -p "$target/nested"
printf 'delete\n' >"$target/nested/key"
ln -s "$outside/sentinel" "$target/link"

# A present but failing shred exercises the portable fallback on every host.
fake_bin=$temporary/bin
mkdir "$fake_bin"
cat >"$fake_bin/shred" <<'SH'
#!/usr/bin/env bash
exit 1
SH
chmod 0700 "$fake_bin/shred"
PATH="$fake_bin:$PATH" "$subject" "$target"
[[ ! -e "$target" && ! -L "$target" ]]
[[ $(<"$outside/sentinel") == retain ]]

link_target=$temporary/link-target
ln -s "$outside" "$link_target"
"$subject" "$link_target"
[[ ! -e "$link_target" && ! -L "$link_target" ]]
[[ $(<"$outside/sentinel") == retain ]]

if "$subject" relative/path >/dev/null 2>&1; then
  echo "relative cleanup target was accepted" >&2
  exit 1
fi
if "$subject" / >/dev/null 2>&1; then
  echo "root cleanup target was accepted" >&2
  exit 1
fi

regular=$temporary/regular
printf 'retain\n' >"$regular"
if "$subject" "$regular" >/dev/null 2>&1; then
  echo "regular-file cleanup target was accepted" >&2
  exit 1
fi
[[ $(<"$regular") == retain ]]

echo "portable sensitive-tree cleanup tests passed"
