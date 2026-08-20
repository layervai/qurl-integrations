# qURL CLI

Publish an app running on your machine with one command:

```bash
qurl publish http://127.0.0.1:3000
```

qURL™ gives the app a permanent **CRID** you can safely paste into chat,
documentation, or an agent prompt. A CRID identifies the protected resource;
it does not grant access. Authorized users turn it into a short-lived access
link only when they need one.

[Publish localhost in 60 seconds](#publish-localhost-in-60-seconds) ·
[Command reference](#commands) · [Scripting](#scripting-contract)

## Publish localhost in 60 seconds

### 1. Install the CLI

On macOS or Linux with Homebrew:

```bash
brew install layervai/tap/qurl
qurl version
```

Local publishing requires qURL CLI 1.6.0 or newer. If the version is older,
update the tap and upgrade the CLI before continuing:

```bash
brew update
brew upgrade qurl
```

Using another package format? See [Install](#install).

### 2. Sign in

[Create or copy an API key in the qURL dashboard](https://layerv.ai/qurl/dashboard/keys/).
Select `qurl:agent` to publish the local app and `qurl:resolve` to open it in
step 4. Then run:

```bash
qurl login
```

Paste the key at the hidden prompt. The CLI validates it before saving it in a
secure credential store (your OS keyring when one is available).

### 3. Start your app, then publish it

Keep your app running in one terminal. If you only want to try the flow, start
Python's built-in web server:

```bash
python3 -m http.server 3000 --bind 127.0.0.1
```

In a second terminal:

```bash
qurl publish http://127.0.0.1:3000
```

When the route is ready, qURL prints:

```text
Published

  Target:  http://127.0.0.1:3000
  Status:  serving

CRID: <CRID>
```

Leave that terminal open while you want the app available. Press Ctrl-C to
stop serving it. Running the same command later reuses the same CRID.

### 4. Open or share it

The CRID is safe to share. An authorized user can open the app with:

```bash
qurl get <CRID>
```

Your app still listens only on your machine. The CLI connects outward to qURL;
you do not need public DNS, a public IP, or custom Connector configuration.

Publishing a remote URL instead? `qurl publish https://api.example.com/reports`
prints its CRID and exits immediately.

### If the first run fails

| What you see | What to do |
|--------------|------------|
| `only HTTPS URLs are allowed` | You have qURL CLI 1.5.0 or older. Run `brew update`, `brew upgrade qurl`, and confirm `qurl version` reports 1.6.0 or newer. |
| No API key is configured | Run `qurl login` |
| The key lacks `qurl:agent` | Add that scope in the dashboard, then log in with the updated key |
| The local app cannot be reached | Check it with `curl http://127.0.0.1:3000` and use the same URL with `qurl publish` |
| `This Connector needs its qURL platform assignment refreshed` | Update to qURL CLI 1.6.1 or newer. If the message remains because a saved assignment genuinely needs a refresh, review why the previous connection stopped, then rerun the same publish command once with `--refresh-mode auto`. |
| The route is rejected or times out | Run the command once more; if it repeats, contact LayerV support |

## Install

**Homebrew** (macOS / Linux):

```bash
brew install layervai/tap/qurl
```

Homebrew also installs the man pages and the bash/zsh/fish completions
shipped in the release archive.

**Debian / RPM** — download the `.deb` or `.rpm` for your architecture from
the [latest release](https://github.com/layervai/qurl-integrations/releases)
and install it with `dpkg -i` / `rpm -i`.

**Prebuilt binaries** — download the archive for your OS and architecture
(`linux`, `darwin`, `windows` × `amd64`, `arm64`) from the
[releases page](https://github.com/layervai/qurl-integrations/releases),
extract it, and put the `qurl` binary on your `PATH`. The archive carries
the man pages and completion files alongside the binary.

Confirm the install:

```bash
qurl version
```

## Authentication

Commands that contact qURL use an API key (`lv_live_…` for production,
`lv_test_…` for test). Create one in the
[qURL dashboard](https://layerv.ai/qurl/dashboard/keys/) with the scopes for
the commands you plan to use:

| What you want to do | Scope |
|---------------------|-------|
| Publish a local app | `qurl:agent` |
| Publish or delete a remote URL | `qurl:write` |
| Resolve or open a CRID | `qurl:resolve` |
| List resources | `qurl:read` |

There is deliberately no `--api-key` flag — command-line arguments leak into
shell history and process lists. The CLI looks for the key in this order:

1. `QURL_API_KEY` environment variable — recommended for scripts and CI.
   When set, nothing on disk is read or written.
2. The key `qurl login` stored: in your OS keyring (Keychain on macOS,
   Credential Manager on Windows, the freedesktop Secret Service on Linux),
   or — only where no keyring is available — in `~/.config/qurl/token`, a
   file readable by your user alone. Commands warn when the key is served
   from that file. Other LayerV tools (the SDK, the Connector) read only
   that file, never the keyring — so on keyring machines, give them the
   key via `QURL_API_KEY` rather than expecting them to see `qurl login`'s.

`qurl login` checks the key before storing it, so a mistyped key fails
immediately. `qurl logout` removes the stored key from every place it may sit,
and `qurl whoami` shows which account and key identity the configured
credential maps to.

The first local publish needs a login key with `qurl:agent`. The CLI uses that
authority only to mint a Connector-bound, one-shot enrollment credential; it
keeps the credential in memory and the native enrollment exchanges it for a
restricted device identity. A warm start reuses that device identity and does
not read the login key or mint another enrollment credential. Remote publish,
resolve, and list continue to work with their existing narrower scopes.

## Configuration

Every setting resolves through the same precedence chain:

```
command-line flag > environment variable > profile/config file > built-in default
```

| Setting | Flag | Environment | Config key | Default |
|---------|------|-------------|------------|---------|
| API endpoint | `--endpoint` | `QURL_ENDPOINT` | `endpoint` | `https://api.layerv.ai` |
| Output format | `-o, --output` | `QURL_OUTPUT` | `output` | `text` |
| Color | `--color` | `QURL_COLOR` | `color` | `auto` |
| Connector ID | `--id` | `QURL_CONNECTOR_ID` | `connector_id` | Stable opaque ID for local `publish`; required by `connector run` |

Config files are YAML. The default file is `~/.config/qurl/config.yaml`; a
named profile lives at `~/.config/qurl/profiles/<name>.yaml` and is
selected with `--profile` or `QURL_PROFILE`. A missing file simply means
defaults apply. **Config files never hold secrets** — a file carrying an
`api_key` entry is rejected outright rather than silently honored.

Also honored: `NO_COLOR` (disables color while `--color` is `auto`), and
`QURL_BROWSER` / `BROWSER` (which browser `qurl get` opens). Pointing the
CLI at a plain-`http` endpoint on a non-local address warns that the key
would travel unencrypted; loopback endpoints are exempt.

## Commands

| Command | Description |
|---------|-------------|
| `qurl publish <target-url>` | Publish a remote URL or serve a loopback HTTP app, and get its CRID |
| `qurl resolve <CRID>` | Turn a CRID into a short-lived access link |
| `qurl get <CRID>` | Fetch what a CRID points to: browser on a terminal, or download with `--file` |
| `qurl list` | List your published resources |
| `qurl delete <CRID>` | Delete a published resource |
| `qurl connector run` | Serve a local app through the qURL platform, outbound-only |
| `qurl login` / `qurl logout` | Store your API key (validated first, OS keyring preferred) / remove it everywhere |
| `qurl whoami` | Show which account and key identity your credential maps to |
| `qurl completion <shell>` | Generate shell completions (`bash`, `zsh`, `fish`, `powershell`) |
| `qurl version` | Print version information |

Run `qurl <command> --help` for the full help text; installed man pages
cover the same surface (`man qurl`, `man qurl-publish`, …).

Commands that take a CRID assess it locally first: a likely typo (bad
checksum, wrong alphabet) is warned about and still forwarded — the server
is the only authoritative validator. Sending a **test-environment CRID to
the production endpoint** is refused unless `--yes` is given; a production
CRID aimed at a non-production endpoint warns and proceeds.

### qurl publish

`qurl publish` handles both local apps and remote URLs:

| Target | What happens |
|--------|--------------|
| `http://127.0.0.1:3000` | qURL serves the local app and keeps running until Ctrl-C |
| `https://api.example.com/reports` | qURL registers the remote URL, prints its CRID, and exits |

#### Local apps

```bash
qurl publish http://127.0.0.1:3000
```

The CRID appears only after the route is ready. If the initial route is
rejected or does not become ready within 30 seconds, the command exits without
printing a success CRID. Once serving, temporary connection failures use the
Connector's normal reconnect behavior.

Restarting the same app on the same machine reuses its resource and CRID. Use
`--id` only when you want to choose the Connector ID yourself.

Local publishing accepts `http://localhost:<port>` and IPv4 or IPv6 loopback
addresses. It intentionally rejects HTTPS, paths, queries, fragments,
credentials, wildcard listeners, and localhost subdomains. These restrictions
keep the one-command path unambiguous; use the advanced
[`qurl connector run`](#qurl-connector-run) command when you need custom state
or enrollment settings.

Local publish is a long-running command, so do not wrap it in command
substitution: the shell would wait until you stop serving. When a process
supervisor needs only the CRID, use `--quiet` and read the first stdout line.

#### Remote URLs

```bash
qurl publish https://api.example.com/reports
```

Remote targets must use HTTP or HTTPS, include a host and valid port, and must
not contain embedded credentials. The CLI validates them before reading your
credential or making a network request.

| Flag | Description |
|------|-------------|
| `--description <text>` | Human-readable description stored with the resource |
| `--tag <tag>` | Tag stored with the resource (repeatable) |
| `--alias <name>` | Memorable handle stored with the resource |
| `--id <id>` | Connector ID for a local publish; local-only |

Description, tags, and alias apply only to remote resources. `--id` applies
only to a local publish. Mixing those options fails loudly instead of being
silently ignored.

In either mode, the CRID is last and alone on its line; `--quiet` prints only
the CRID. Publishing the same target again does not create a duplicate while
its resource is active: the existing CRID is returned and the output says so.
JSON reports a known outcome as `found_existing: true` or `false`; if recovery
cannot prove which happened, it omits the field rather than guessing. Delete
the resource first if you intentionally want a new CRID.

### qurl resolve

`qurl resolve <CRID>` mints a temporary access link for the resource the
CRID names. The link expires on its own; resolve again whenever you need
a fresh one. When stdout is not a terminal the command prints the bare
link and nothing else, ready to share or open.

The link opens in a browser. Passing it to a tool like curl fetches the
page that opens the link, not the content itself — to download content
from a script, use `qurl get <CRID> --file <path>`.

| Flag | Description |
|------|-------------|
| `--ttl <duration>` | Requested link lifetime in whole seconds (e.g. `5m`, `1h`). The service may grant less; a shorter grant is reported on stderr, never silent. Sub-second or negative values are refused rather than rounded. |
| `--yes` | Proceed without confirmation, including sending a test CRID to production |

Before anything is printed, the CLI verifies the service's answer against
the CRID you asked for; a mismatched answer is discarded and the command
exits with code 12 without printing a link.

### qurl get

`qurl get <CRID>` resolves and verifies exactly like `qurl resolve`, then
acts on the verified link — nothing is ever acted on unverified:

- **On a terminal**, get prints the link, then opens it in your browser
  (set `QURL_BROWSER` or `BROWSER` to choose which one).
- **With `--file <path>`** it downloads to that path instead. For links
  that need a browser to open, get asks the qURL platform for direct
  access and downloads the granted content — it never saves the
  in-browser page in place of your file. The download is atomic: bytes
  arrive in `<path>.part`, which becomes `<path>` only when the download
  completes. Existing files are never replaced unless `--force` is given,
  and an access link that expires mid-download is refreshed and retried
  once automatically.
- **With `--file -`** the raw bytes stream to stdout, clean for piping —
  gate pipelines on the exit status, since a mid-stream failure leaves
  already-written bytes behind.

| Flag | Description |
|------|-------------|
| `--file <path>` | Download to this path instead of opening a browser (`-` = raw bytes to stdout) |
| `--force` | Allow `--file` to replace an existing file |
| `--yes` | Proceed without confirmation, including sending a test CRID to production |

When stdout is not a terminal, get never opens a browser: pass `--file`,
or use `qurl resolve` if you only need the link. With `-o json`, get is a
machine asking for data, so browser mode and `--file -` are refused
loudly; `--file <path> -o json` downloads and emits the outcome document.

Direct downloads use the deployment settings shipped with the CLI. On a
deployment they don't cover — the sandbox, or a self-hosted platform —
set `QURL_DEPLOYMENT` to the path of that deployment's settings file (ask
whoever runs the deployment for it). Without usable settings, `get
--file` fails loudly with exit code 3 rather than downloading the wrong
thing; browser mode needs no settings at all.

### qurl list

`qurl list` prints one row per resource published under your account. The
text table shortens each CRID from the middle so rows stay readable; JSON
output and `--quiet` always carry the full CRID.

| Flag | Description |
|------|-------------|
| `--limit <n>` | Maximum resources per page, 1–100 (default: service decides) |
| `--cursor <cursor>` | Continue from a previous page's cursor |
| `--status <status>` | Only resources with this status, e.g. `active` |
| `--type <kind>` | Only resources of this kind: `url` or `tunnel` |

When more results exist, text mode says so on stderr with the `--cursor`
value to pass next. See [JSON output](#json-output--o-json) for the
pagination contract scripts should follow.

`-o json` additionally carries each row's `type` and its publish-time
`description` and `tags` — the metadata `qurl publish --description` and
`--tag` set. The text table deliberately does not: its five columns
already run past an 80-column terminal, and it shortens the CRID and
truncates the target to get even that far. Scripts that recognize resources
by the label their publisher gave them read the JSON document:

```bash
cursor=""
while :; do
  if [ -n "$cursor" ]; then
    page=$(qurl list --status active -o json --cursor "$cursor")
  else
    page=$(qurl list --status active -o json)
  fi
  jq -r '.resources[] | select((.description // "") | test("safe to delete")) | .crid' <<<"$page"
  jq -e '.has_more' <<<"$page" >/dev/null || break
  cursor=$(jq -r '.next_cursor // empty' <<<"$page")
  [ -n "$cursor" ] || break
done
```

The loop is the point: a single `qurl list` call returns one page, so a
sweeper that reads only `.resources[]` from one invocation silently misses
everything behind `has_more`. `(.description // "")` matters too — the key is
absent on rows that carry no description, and `null | test(...)` is a jq
error, not a non-match.

`--status active` matters for a different reason: deleting a resource flips
its status rather than removing the row, so an unfiltered listing keeps
returning resources you have already deleted. Pass it on anything that walks
the whole listing.

### qurl delete

`qurl delete <CRID>` deletes a published resource. Deletion cannot be
undone: the CRID stops resolving, and republishing the same target later
mints a different CRID.

| Flag | Description |
|------|-------------|
| `--yes` | Skip the confirmation prompt (required when stdin is not a terminal) |

Interactive runs confirm first; scripts and pipelines must pass `--yes` —
without a terminal the command refuses rather than hanging. Deleting an
already-deleted resource succeeds idempotently and says so (JSON sets
`already_gone`).

### qurl connector run

`qurl connector run --id <id> --target <host:port>` is the advanced and
backward-compatible local serving surface. It serves an app
running on your machine through the qURL platform. Your app keeps
listening on localhost and the Connector connects outward — your machine
never opens a listening port to the internet — while the platform
verifies each caller and grants access before any request is forwarded.

`connector run` currently requires macOS or Linux. The Windows release can
still use management commands such as `qurl get`, `qurl list`, and `qurl
delete`, but it fails closed before creating a local Connector identity; the
pinned native identity store does not yet claim Windows filesystem semantics.

| Flag | Description |
|------|-------------|
| `--id` | Which Connector to run: its ID in qURL — the route name your app serves under (or `connector_id` in your profile) |
| `--target` | The local app, as `host:port`; `:8080` means `127.0.0.1:8080` |
| `--state-dir` | Where this machine's Connector identity lives (default: your user state directory) |
| `--refresh-mode` | Self-healing gate after sustained failures: `manual` (default), `auto`, or `disabled` |

The Connector ID is the same identity the standalone qurl-connector
configures as `QURL_CONNECTOR_ID` (YAML `id:`), so one setting covers a
machine that moves between the two tools. It must be 3–64 lowercase letters,
numbers, or hyphens, start with a letter, and end with a letter or number;
`connector run` validates this platform-owned grammar before opening state or
making a network request. The names v1.1.0 briefly shipped still work,
deprecated: `--slug` as a hidden alias of `--id`
(passing both with different values is refused), and
`QURL_CONNECTOR_SLUG` / `connector_slug` at lower precedence than their
`id`-named counterparts. All three will be removed in the next major
release.

The first start enrolls this machine and needs a one-time enrollment
token from the qURL console, supplied **only** via `QURL_CONNECTOR_TOKEN`
or `QURL_CONNECTOR_TOKEN_FILE` — there is deliberately no token flag,
because arguments leak into shell history and process lists. The token is
used once and never stored; later starts reuse the saved identity.

After enrollment, Connector resource setup and continuity checks stay on the
same native NHP path as admission: the CLI sends an authenticated encrypted
UDP request directly to its assigned LayerV cell, then knocks that cell for
the tunnel session. It does not call `api.layerv.ai` for those runtime steps
and does not fall back through a Hub, relay, or another cell. A logged-in
`qurl publish` may call the HTTPS API once to mint the one-time enrollment
token when the machine has no saved identity; explicit management commands
such as `qurl list` and `qurl delete` continue to use HTTPS.

The state directory also stores the authenticated Connector binding and an
exact pending request nonce in an owner-only file. The request is saved before
it is sent, so a restart after a lost UDP response safely replays the same
logical operation. On later starts the saved public resource identity is sent
as a continuity assertion; the CLI refuses an unexpected replacement instead
of silently adopting it. Do not copy one state directory into multiple active
Connector instances.

Every start prints the Connector's CRID, so the identity a consumer needs
is on screen rather than something to go look up:

```
Starting Connector "reports" for your local app at 127.0.0.1:8080. Press Ctrl-C to stop.

  Anyone authorized can reach it with `qurl get <CRID>`.

CRID: aea6x7mea52zcalolw7nis3g4iy3rcfr7nzyfukkuujsqufnxhmvhhtledfa
```

That note is prose for a person. An unattended runner should read the
structured event instead, emitted on stderr beside the command's other
operator events:

```
level=INFO msg="connector: starting to serve local app" event=connector_starting connector_id=reports target=127.0.0.1:8080 crid=aea6x7mea52z…
```

`event=connector_starting` is the stable name; it fires as the serve loop
starts, not once traffic flows. On this advanced surface, `event=proxy_allow`
preserves its admission-level meaning after an authenticated Login; `connector
run` does not opt into local publish's terminal 30-second exact-proxy readiness
gate, so FRP retains its existing registration and reconnect behavior. `crid`
is omitted entirely rather than logged empty when the platform returned none,
so its presence is meaningful. A CRID is base32 over `[a-z2-7]`, so the value
never needs quoting and always renders as a bare `crid=<value>`.

If the platform stays unreachable long enough, the command exits with
code 11 instead of retrying forever. The next start may then need its
platform assignment refreshed: with the default `--refresh-mode manual`
it stops and asks for approval (exit 2) — approve by running once with
`--refresh-mode auto`. Automatic restarts are deliberately not treated
as approval. Stop serving with Ctrl-C or SIGTERM; teardown gets a short
grace period and the command exits 130.

Once a tunnel has been admitted, losing the connection no longer fails
quietly. The Connector says so on stderr, keeps retrying for a bounded
window, and then starts a fresh connection cycle rather than retrying
invisibly:

```
level=WARN msg="connector: the tunnel connection keeps dropping and is not staying up; still retrying, and consumers will time out while it is down" event=reconnect_retrying dial_attempts=3 retrying_seconds=48.2 gives_up_after_seconds=240
```

The message states the observation and not a cause: at that layer the
Connector only sees transport errors with no reason attached, so naming
one would be a guess. The retry budget is finite either way, and a
Connector that never recovers exits 11 rather than looping forever.

### qurl login / logout / whoami

`qurl login` reads the key from piped stdin or a hidden interactive
prompt — never as an argument — validates its shape, checks it against
the qURL service, and only then stores it (OS keyring preferred, the
`~/.config/qurl/token` fallback where no keyring exists — see
[Authentication](#authentication)). `qurl logout` removes the stored key
from every backend that holds it; it does not touch `QURL_API_KEY` in
your environment. `qurl whoami` shows the account and the key's own
identity (id, kind, scopes, expiry) — identity only, no plan or usage
data, so it is cheap enough for scripts and shell prompts (`--quiet`
prints just the owner id).

```bash
op read op://team/qurl/key | qurl login
qurl whoami -o json
```

### qurl completion

`qurl completion <shell>` writes a completion script to stdout for
`bash`, `zsh`, `fish`, or `powershell`:

```bash
eval "$(qurl completion bash)"
qurl completion zsh > "${fpath[1]}/_qurl"
qurl completion fish > ~/.config/fish/completions/qurl.fish
```

Homebrew installs the bash/zsh/fish completions for you.

### qurl version

`qurl version` prints one line — `qurl version <version> (<os>/<arch>)` —
and its shape is a stable contract, so scripts may parse it
(`qurl version | awk '{print $3}'`).

`qurl version`, `qurl completion`, and `qurl help` deliberately read no
configuration, credentials, or network: a broken config file can never
brick shell startup or a version check.

There is also a hidden maintenance command, `qurl docs [man|markdown] -d
<dir>`, which generates the man pages and markdown docs from the command
tree itself; release packaging runs it to produce the man pages shipped
in every archive.

## Global flags

| Flag | Description |
|------|-------------|
| `-o, --output text\|json` | Output format (JSON shapes are a stable contract) |
| `-q, --quiet` | Print only the primary value, one per line — for scripts |
| `--color auto\|always\|never` | Colorize output (`NO_COLOR` is honored) |
| `--endpoint <url>` | qURL API endpoint |
| `--profile <name>` | Configuration profile |
| `-v, --verbose` | Request diagnostics on stderr (credentials always redacted) |

## Scripting contract

- **stdout carries data, stderr carries everything else.** `qurl resolve`
  piped into another command prints the bare link and nothing more;
  notes, warnings, and confirmation prompts go to stderr.
- **`--quiet` prints only the primary value**, one per line: the CRID for
  `publish`, the link for `resolve`, full CRIDs for `list`, the
  destination path for a `get --file` download, the owner id for
  `whoami` and `login`.
- **Verification is built in:** before printing anything, `qurl resolve`
  and `qurl get` check the service's answer against the CRID you asked
  for and discard mismatches (exit 12).

### Exit codes

Exit codes are stable. The meanings below mirror the CLI's single
exit-code authority in code (`apps/cli/internal/exitcode`):

| Code | Name | Meaning |
|-----:|------|---------|
| 0 | success | The command did what was asked. |
| 1 | general | An unclassified failure, including features not yet available in this build. |
| 2 | usage | The command line itself was wrong: flags, arguments, or missing confirmation. |
| 3 | configuration | Configuration files or profiles are invalid. |
| 4 | authentication | No credential, an implausible credential, or the service rejected the credential. |
| 5 | not found | The resource does not exist or is retired — revoked and tombstoned resources included; the stderr message distinguishes them. |
| 6 | permission | The credential lacks permission for this operation. |
| 7 | conflict | The request conflicts with current state — including `--file` refusing to replace an existing destination without `--force`. |
| 8 | invalid input | An operand or request rejected as invalid (by the service, or locally for inputs that can never be valid). |
| 9 | rate limited | Still rate limited after the CLI's bounded automatic retries. |
| 10 | server error | The service failed or answered outside its contract. |
| 11 | unavailable | The service cannot be reached or is not serving this surface: HTTP 503, network failures, timeouts. |
| 12 | verification failed | The response failed CRID-anchored verification. Nothing was printed — treat it as tampering, not transience. |
| 130 | interrupted | The run was canceled (Ctrl-C or SIGTERM), including a graceful `connector run` stop. |

### JSON output (`-o json`)

Every command's `-o json` document uses field names owned by this repo —
a stable contract independent of upstream renames. Fields that only
sometimes apply (`found_existing`, `already_gone`, a missing `crid` on
older deployments) are omitted rather than emitted empty.

For `qurl list`, **`has_more` — not `next_cursor` presence — is the
pagination terminator.** The service legitimately serves short and even
zero-item pages with `"has_more": true`, so consumers must keep following
`next_cursor` until `"has_more": false`; `has_more` is always emitted for
exactly that reason.

```bash
qurl list -o json | jq -r '.resources[].crid'
```
