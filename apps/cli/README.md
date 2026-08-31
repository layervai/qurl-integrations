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

On macOS with Homebrew:

```bash
brew install layervai/tap/qurl
qurl version
```

On Windows, download the Windows `.zip` from the
[latest release](https://github.com/layervai/qurl-integrations/releases),
extract `qurl.exe`, add its directory to your user `PATH`, and run
`qurl version` in PowerShell.

Local lifecycle commands require qURL CLI 2.0.0 or newer. If the version is older,
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

Paste the key at the hidden prompt. The CLI validates it, enrolls this machine,
and then discards it. qurl stores the restricted device identity in its
owner-only native state; it does not store the account API key.

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

The command exits after the route is serving. A per-user background daemon
keeps the share available and resumes it after login, sleep, wake, or a network
change. Run `qurl stop <CRID>` to turn it off and `qurl start <CRID>` to turn it
back on. Publishing the same target later reuses the same CRID.

Background lifecycle management is available on Linux, macOS, and Windows.
Linux uses the native systemd user manager. Use `--foreground` for CI,
debugging, or a process that another service manager owns.

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
| `only HTTPS URLs are allowed` or no `start`, `stop`, `restart`, and `status` commands | You have the legacy CLI. Run `brew update`, `brew upgrade qurl`, and confirm `qurl version` reports 2.0.0 or newer. |
| No API key is configured | Run `qurl login` |
| The key lacks `qurl:agent` | Add that scope in the dashboard, then log in with the updated key |
| The local app cannot be reached | Check it with `curl http://127.0.0.1:3000` and use the same URL with `qurl publish` |
| `This Connector needs its qURL platform assignment refreshed` | Upgrade qURL. Current releases refresh stale assignments automatically with bounded backoff; no approval flag is required. |
| The route is rejected or times out | Run the command once more; if it repeats, contact LayerV support |

## Install

**Homebrew** (macOS / Linux):

```bash
brew install layervai/tap/qurl
```

Homebrew also installs the man pages and the bash/zsh/fish completions
shipped in the release archive.

The CLI supports remote and local background qURL commands on macOS, Windows,
and Linux. Linux uses the native systemd user manager and reports a clear error
when that manager is unavailable.

**Debian / RPM** — download the `.deb` or `.rpm` for your architecture from
the [latest release](https://github.com/layervai/qurl-integrations/releases)
and install it with `dpkg -i` / `rpm -i`.

**Windows** — download the Windows `.zip` for your architecture from the
[latest release](https://github.com/layervai/qurl-integrations/releases),
extract `qurl.exe`, and put its directory on your user `PATH`. The first local
`publish` or `start` installs an owner-only per-user Task Scheduler job. It
does not require administrator access or store an account API key.

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

The CLI uses a registered device identity for ordinary commands. To enroll the
device, create an account API key (`lv_live_…` for production, `lv_test_…` for
test) in the [qURL dashboard](https://layerv.ai/qurl/dashboard/keys/) with the
`qurl:agent` scope, then run `qurl login`.

There is deliberately no `--api-key` flag — command-line arguments leak into
shell history and process lists. `qurl login` reads it from a hidden prompt or
piped standard input. Scripts and CI can set `QURL_API_KEY` or
`QURL_API_KEY_FILE` for the same first bootstrap or for an explicit recovery.

`QURL_API_KEY_FILE` must be an exact absolute path. Its contents must be the
key bytes followed by one LF or CRLF line ending, with no BOM, spaces, or
second newline.
On Unix, the file must be owned by you, have mode `0400` or `0600`, and have
exactly one hard link. Create it with
`(umask 077; printf '%s\n' "$QURL_API_KEY" > "$path")`.

On Windows, `QURL_API_KEY_FILE` must name a file that is owned by the current
user and has a protected, owner-only ACL. PowerShell's normal CRLF line ending
is accepted. To write UTF-8 without a BOM:

```powershell
[IO.File]::WriteAllText($path, $key + "`n", [Text.UTF8Encoding]::new($false))
```

Create the file inside an owner-only temporary directory, remove ACL
inheritance from the file, and remove the file immediately after `qurl login`.
The CLI rejects an inherited or broadly readable ACL with the exact failing
ACL condition. For most Windows CI jobs, use the one-command `QURL_API_KEY`
environment value instead; qurl consumes it only for enrollment and does not
store it.

The CLI validates the account key and uses it once to enroll a restricted
device identity. Only that device identity and its restricted credential enter
the owner-only local state directory. The account API key and one-time
enrollment credential remain in memory and are not stored by qurl. A warm
command reuses the device identity and does not read `QURL_API_KEY`.

`qurl whoami` checks the registered device and shows its account.

Authenticated commands need the owner-only local state directory to remain
writable. `QURL_API_KEY` can bootstrap missing or explicitly rejected device
credentials, but it is not a steady-state bypass for that durable identity.
Each authenticated command verifies the saved device identity with the qURL
platform before it calls the resource API. If the platform cannot verify that
identity, read-only commands such as `qurl list` are unavailable too. An
account API key does not bypass this boundary.

One native state directory belongs to one account. To switch accounts, first
revoke the registered device key in the qURL dashboard. Then move or remove the
complete state directory and run `qurl login` with the other account. Do not
edit or delete individual state files; qurl rejects cross-account reuse and
prints the exact directory and device-key ID needed for this recovery.

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
| Connector ID | `--id` | `QURL_CONNECTOR_ID` | `connector_id` | Stable opaque ID for local `publish` |

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
| `qurl start <CRID>` | Turn on a previously published local share |
| `qurl stop <CRID>` | Turn off a local share without deleting it |
| `qurl restart <CRID>` | Rotate and restart a local share |
| `qurl status <CRID>` | Show desired and platform-observed sharing state |
| `qurl inspect <CRID>` | Inspect the same authoritative resource or sharing state |
| `qurl daemon run` | Run the local sharing daemon directly for headless or supervised use |
| `qurl delete <CRID>` | Delete a published resource |
| `qurl login` | Enroll this device with a one-time account key |
| `qurl whoami` | Show which account this registered device belongs to |
| `qurl completion <shell>` | Generate shell completions (`bash`, `zsh`, `fish`, `powershell`) |
| `qurl version` | Print version information |

Run `qurl <command> --help` for the full help text; installed man pages
cover the same surface (`man qurl`, `man qurl-publish`, …).

### qurl daemon run

`qurl daemon run` lets another service manager own the long-running process.
With no headless flags, it serves the local shares already stored under
`--state-dir`.

Generated Docker, Kubernetes, and other headless deployment instructions can
also supply `--headless-config <share.yaml>`. This is a non-secret, read-only
version 2 YAML file for exactly one share. Use the generated file as-is; its
identifiers bind the deployment to that share.

The first start of a new state volume also requires
`--enrollment-token-file <path>`. This file contains a one-time enrollment
credential, not an account API key. It must be a read-only regular file or a
Kubernetes projected-secret link, and only its owner or the process's dedicated
group can read it. Keep the same file available until the first start finishes.
If bootstrap stops before registration finishes, retry with the same still-valid
one-time credential. In the next deployment revision, remove the flag but keep
the secret recoverable. Verify that the warm start connects, then remove the
secret mount and delete the one-time secret. A complete warm start does not read
or require that file.

Commands that take a CRID assess it locally first: a likely typo (bad
checksum, wrong alphabet) is warned about and still forwarded — the server
is the only authoritative validator. Sending a **test-environment CRID to
the production endpoint** is refused unless `--yes` is given; a production
CRID aimed at a non-production endpoint warns and proceeds.

### qurl publish

`qurl publish` handles both local apps and remote URLs:

| Target | What happens |
|--------|--------------|
| `http://127.0.0.1:3000` | qURL starts the background share, waits for serving, prints its CRID, and exits |
| `https://api.example.com/reports` | qURL registers the remote URL, prints its CRID, and exits |

#### Local apps

```bash
qurl publish http://127.0.0.1:3000
```

The CRID appears only after the route is ready. Once the daemon owns the
durable local share, temporary assignment, sleep/wake, and network failures
recover automatically without a customer approval step.

Restarting the same app on the same machine reuses its resource and CRID. Use
`--id` only when you want to choose the Connector ID yourself.

Local publishing accepts `http://localhost:<port>` and IPv4 or IPv6 loopback
addresses. It intentionally rejects HTTPS, paths, queries, fragments,
credentials, wildcard listeners, and localhost subdomains. These restrictions
keep the one-command path unambiguous. `--foreground` runs the same production
daemon engine in the current process for CI and debugging. Scripts can use
`--quiet` to read only the full CRID.

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
self-hosted or custom deployment, set `QURL_DEPLOYMENT` to the path of that
deployment's settings file (ask whoever runs the deployment for it). Without
usable settings, `get
--file` fails loudly with exit code 3 rather than downloading the wrong
thing; browser mode needs no settings at all.

### qurl list

`qurl list` prints one row per resource published under your account. Text,
JSON, and `--quiet` all carry the full CRID. The text table can be wide because
it does not shorten identifiers or local targets.

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
`--tag` set. Tunnel rows also carry `desired_state` and an explicit
`serving_epoch`, including epoch zero. The text table keeps publish metadata
out of its six operational columns. Scripts that recognize resources by the
label their publisher gave them read the JSON document:

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

### qurl start / stop / restart / status / inspect

Local shares are durable desired state, managed by their full CRID:

```bash
qurl stop <CRID>
qurl start <CRID>
qurl restart <CRID>
qurl status <CRID>
qurl inspect <CRID>
```

`stop` disables the cloud route first and then tells an already-running local
daemon to reconcile; it never starts the daemon. `start` is idempotent and
requires the saved local target to be reachable. `restart` always advances the
serving epoch so stale sessions cannot keep serving. `status` and
`inspect` use the same authoritative view. Both work for remote resources and
include the local target only when this machine owns one.

Custom deployments must support the current CLI resource-status API. The CLI
does not scan the full account inventory when one resource-status request
fails.

The daemon keeps each share independent. It recovers assignment, sleep/wake,
and network failures automatically with persisted bounded backoff. Customers
never need a refresh approval flag. On macOS the first local `publish` or
`start` installs an owner-only LaunchAgent. On Windows it installs a
least-privilege per-user Task Scheduler job. The installed `qurl` path survives
normal upgrades, and a binary-version change reloads the resident daemon
deliberately. Ordinary lifecycle commands reload desired state over an
owner-only local control channel without restarting healthy sibling shares.

`qurl list` prints every full CRID. For locally registered tunnel rows it also
prints the canonical loopback target and durable desired state. The paged list
does not make one live API request per row, so its observed column is
`unknown`; use `qurl status <CRID>` for the authoritative `stopped`,
`connecting`, or `serving` observation. If the owner-only local registry is
unavailable, list omits local targets and emits one warning.

### qurl login / whoami

`qurl login` reads the account key from piped stdin or a hidden interactive
prompt — never as an argument — validates it, enrolls the registered device,
checks that the device belongs to the same account, and discards the account
key. `qurl whoami` checks the registered device and shows its account identity
only, with no plan or usage data. `--quiet` prints just the owner id.

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
| 130 | interrupted | The foreground daemon or another command was canceled with Ctrl-C or SIGTERM. |

### JSON output (`-o json`)

Every command's `-o json` document uses field names owned by this repo —
a stable contract independent of upstream renames. Fields that only
sometimes apply (`found_existing`, `already_gone`) are omitted rather than
emitted empty. Every resource result requires a verified `crid`.

For `qurl list`, **`has_more` — not `next_cursor` presence — is the
pagination terminator.** The service legitimately serves short and even
zero-item pages with `"has_more": true`, so consumers must keep following
`next_cursor` until `"has_more": false`; `has_more` is always emitted for
exactly that reason.

```bash
qurl list -o json | jq -r '.resources[].crid'
```
