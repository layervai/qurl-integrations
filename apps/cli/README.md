# qURL CLI

Publish an app running on your machine with one command:

```bash
qurl publish http://127.0.0.1:3000
```

qURL™ gives the app a permanent **CRID** you can give to recipients in chat,
documentation, or an agent prompt. `qurl share` and `qurl get` turn a CRID
into a short-lived access link using this device's identity. They work on the
resource owner's devices and, for a private resource, on the devices the
publisher allowed; any other device gets "not found".

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

### 2. Start your app, then publish it

No account, API key, or browser sign-in is required. The first command creates
and registers a device identity automatically. Keep its local state to keep
control of your resources. <!-- TODO(upstream-contract): qurl-service owns the anonymous resource and link caps. -->
Anonymous devices can publish up to three active
resources with links valid for at most 24 hours.

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
  Access:  private — only you and the people you allow can open it
  Status:  serving

CRID: <CRID>
```

The command exits after the route is serving. A per-user background daemon
keeps the share available and resumes it after login, sleep, wake, or a network
change. Run `qurl stop <CRID>` to turn it off and `qurl start <CRID>` to turn it
back on. Publishing the same target later reuses the same CRID.

Background lifecycle management is available on Linux, macOS, and Windows.
Linux uses the native systemd user manager. Use `--foreground` for CI or
debugging. When another program owns the daemon process, run it with
`qurl daemon run --supervision external` instead — see
[External supervision](#external-supervision).

### 3. Open or share it

`qurl get` uses this device's identity. On the machine that published the app,
open it with:

```bash
qurl get <CRID>
```

The app is private: your own devices can open it, and no other device can
until you allow it. To allow another device, or to publish a public resource
instead, see [Private CRIDs](#private-crids).

Your app still listens only on your machine. The CLI connects outward to qURL;
you do not need public DNS, a public IP, or custom Connector configuration.

Publishing a remote URL instead? `qurl publish https://api.example.com/reports`
prints its CRID and exits immediately.

### If the first run fails

| What you see | What to do |
|--------------|------------|
| `only HTTPS URLs are allowed` or no `start`, `stop`, `restart`, and `status` commands | You have the legacy CLI. Run `brew update`, `brew upgrade qurl`, and confirm `qurl version` reports 2.0.0 or newer. |
| Using a new device identity | Run `qurl account setup` to enable recovery |
| The key lacks `qurl:agent` | Add that scope in the dashboard, update your key file, and retry |
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

**Install script** (macOS / Linux, no Homebrew or `sudo` needed):

```bash
curl -fsSL https://raw.githubusercontent.com/layervai/qurl-integrations/main/scripts/install.sh | sh
```

The script verifies the download against the release's `checksums.txt`.
It installs to `/usr/local/bin` when that is writable and otherwise to
`~/.local/bin`, printing a `PATH` hint if needed. It never runs `sudo`, so it
also works unattended for coding agents and CI. Set `INSTALL_DIR` to choose
the directory, or run the script with `sudo` for a system-wide install.

The CLI supports remote and local background qURL commands on macOS, Windows,
and Linux. Linux uses the native systemd user manager. Containers and sandboxes
often run without one; there, `qurl publish` says so and points to
`qurl publish <url> --foreground` or `qurl daemon run --supervision external`.

qURL refuses to keep its state or run its background job under a directory
other users can write to. With `umask 002` (the Ubuntu and Fedora default), a
`~/.local` or `~/bin` created by another tool can end up mode `0775`; the error
names the directory. Run `chmod go-w <directory>`, or set
`QURL_CONNECTOR_STATE_DIR` (or `XDG_STATE_HOME`) to a directory you own that
other users can't write to.

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

The CLI creates a registered device identity automatically for ordinary commands.
Account access is optional:

```bash
qurl account setup
```

Commands that open device state, including `list` and `whoami`, create a device identity on first use.
`get <CRID>` manages access to your own resource; it is not a recipient link opener.

Sign in through the browser to link the current resources to your account.
Existing resource IDs and links stay unchanged. On a new device, run
`QURL_CONNECTOR_STATE_DIR=~/.qurl-recovered qurl account recover` to regain management access.
Use an unused directory and keep existing device state intact. If the account has several
resource owners, select the owner with `--owner`. Recovery does not copy local
files or restart apps from the previous device. Keep the device state until
account linking completes. Recovery requires a linked account or a saved copy of the device state.

Existing account API keys remain supported through `qurl login` and environment
variables. Use a key with `qurl:agent` for explicit account enrollment.

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

If enrollment stops before it completes, run `qurl login` again with a key for
the same account. The CLI resumes enrollment with the saved device identity.
Keep the local state directory unchanged for this retry.

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

This release requires CRID continuity and version 3 of the Connector resource
journal. It does not convert old journals or use the old protocol. Preserve
old state and finish unresolved operations with its matching binary before
replacing that environment. To start fresh, stop the daemon, revoke its device
key in the dashboard, move the complete state directory aside, then run
`qurl login` and publish again. Fresh publication creates new CRIDs. Do not
copy individual bindings or pending requests into the new state.

### Supervised installs

All commands that open device state, including `list`, `whoami`, and `get`, must
use the state's supervision mode. For an externally supervised namespace, set
`QURL_DAEMON_SUPERVISION=external` or pass `--supervision external`. A fresh
externally supervised namespace must first enroll with `login --anonymous` or
the enrollment-token login flow. Both require the sealed key-provider settings
below and preserve an existing external identity.

For account-free enrollment, invoke `qurl login --anonymous --supervision external
-o json` with the same sealed key provider and inherited descriptor. This form
refuses account API-key environment variables and `--enrollment-token-file`.
Its JSON result contains `owner_id`, `auth_type`, `device_key_id` when available,
and `device_enrolled: true`. Retain the complete state namespace and wrapping
key across launches; failure never authorizes deleting or replacing them.

Supervisors can use `qurl request METHOD /v1/... --supervision external -o json`
for the registered device's existing resource operations. It requires the same
sealed key provider as enrollment and refuses natively supervised namespaces and
account API-key configuration, so it never enrolls or recovers a device
implicitly. For POST, PUT and PATCH, pipe an optional JSON body through stdin;
when there is no body, redirect stdin from the null device (`/dev/null`, or
`NUL` on Windows) rather than closing it, because a closed stdin is a read
error. The CLI sets no timeout, so impose a deadline on the process; it covers
both the stdin read and the HTTP call. GET and DELETE never read stdin. Responses have
`{status, headers, body}`; headers contain only `content-type`, `retry-after`
and `x-request-id`. Empty responses use `body: null`; non-JSON responses use a
string. HTTP failures also return this envelope with exit zero; local and
transport failures exit nonzero. Requests make one attempt. When retrying a
mutation, pass the same `--idempotency-key` (32–256 ASCII letters, digits,
hyphens or underscores; never on GET); it protects only routes where the qURL
service implements idempotency. Bodies are limited to 1 MiB including
surrounding whitespace. Absolute URLs and routes outside the registered-device
allowlist (mirrored locally from the SDK) are refused. Queries are supported
only for `GET /v1/resources` and `GET /v1/resources/{id}/qurls`; the CLI and the
SDK both reject queries on other routes. Response bodies also have a 1 MiB cap;
exceeding it returns a nonzero exit. Responses can carry capability material
such as newly minted qURL links; do not log stdout durably.

The allowed routes are `GET /v1/me`, `POST /v1/account/link`, `POST /v1/qurls`,
`GET` and `POST /v1/resources`, and for one resource: `GET`, `PATCH` and
`DELETE /v1/resources/{id}`; `GET` and `PUT .../sharing`;
`POST .../sharing/restart`; `POST .../share`; `GET` and `POST .../qurls`;
`PATCH` and `DELETE .../qurls/{id}`; `GET` and `DELETE .../sessions`; and
`DELETE .../sessions/{id}`. These operations require matching service-side
device authorization. The bridge only forwards the HTTP call: it does not
update this machine's share registry or reload the daemon, so use `qurl start`,
`qurl stop`, `qurl restart` and `qurl delete` for resources shared from this
machine rather than the `sharing` and resource `DELETE` routes. Sessions are
unpaginated, so a session list larger than the 1 MiB response cap fails; terminate all sessions to recover. API key
creation, account owner enumeration, billing, quota and usage are refused.

For example, account linking uses `POST /v1/account/link` with
`{"account_token":"<account access token>"}` on stdin. Keep that token out of
argv, environment variables, logs and durable files. The device credential
stays inside the CLI. Before recording success, verify the returned `owner_id`
matches this device and `account_id` matches the intended account.

A program that runs the daemon itself (see
[External supervision](#external-supervision)) never hands qurl an account API
key. From its own signed-in session it mints a one-time enrollment token for
target `agent` through the qURL API — the same kind of token `qurl login`
mints for itself from an account key — writes it to a private file, and runs:

```bash
qurl login --enrollment-token-file /abs/path/to/token --supervision external
```

The command reads the file once, only while enrolling; a warm namespace never
opens it. It reads no account key from anywhere and refuses to run while
`QURL_API_KEY` or `QURL_API_KEY_FILE` holds a non-empty value (exit code 2).
Create the file in an owner-only directory and delete it on every exit path,
including failures. The path must be absolute and clean, and the file must be
a regular file — never a symlink — that you own, with exactly one hard link
and mode `0400` or `0600`, holding one non-empty token without whitespace, at
most 16 KiB, and at most one trailing line ending. This reader is stricter than the
projected-secret reader behind `qurl daemon run --enrollment-token-file` in
[headless deployments](#headless-deployments): links and group access are
never accepted.

A supervised install keeps its daemon state sealed rather than in plaintext.
The command requires `LAYERV_KEY_PROVIDER=local-key` and `LAYERV_LOCAL_KEY_FD`
naming an inherited descriptor (3 or higher): a pipe or connected local stream
socket that delivers exactly 32 key bytes and closes, so the wrapping key never
appears in arguments, the environment, or a file. Every `qurl` process the
supervisor runs against that state directory — `daemon run` and the lifecycle
commands included — inherits the same two settings, each with a descriptor of
its own. The sealed envelope (`agent_state.sealed.json`) and the plaintext one
(`agent_state.json`) never share a directory. With a sealing provider set
(anything but `file`), a directory that already holds plaintext state is
refused. Without it, a
directory sealed by `local-key` is refused, and the error names the
variable to set. Omitting the provider is an error for token-file login. Native
account-key enrollment uses the default [key storage](#key-storage): sealed to
the TPM where one is usable, otherwise plaintext. Switching providers
is therefore a fresh namespace, not an in-place migration. There is no flag for the provider; the supervisor that owns the key
sets the environment. The token-file reader and the inherited-descriptor key
transport are available on macOS and Linux.

An unusable token file also returns exit code 4. Correct the file and retry
with the same state directory; this error does not require a new namespace.
Use the error message to distinguish it from a wrong-kind device below.

The enrolled device must be owner-scoped. A token minted for target
`connector` enrolls a credential that native session operations refuse, so
`login` fails with exit code 4. By then the directory is already marked
externally supervised and holds a sealed envelope with the wrong-kind
credential, so it is left resumable but unusable: move it aside and enroll
into a fresh one. Retrying in place with a corrected token does not work. On
success the usual login document is printed; with `-o json` it carries
`device_key_id` next to `owner_id`, the two values a supervisor records.
`device_key_id` is omitted rather than empty when the account API reports no
key object.

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
| Session group mode | `--share-group-mode` (`daemon run`) | `QURL_SHARE_GROUP_MODE` | `share_group_mode` | `single` — see [Session group modes](#session-group-modes) |
| Daemon supervision | `--supervision` | `QURL_DAEMON_SUPERVISION` | `daemon_supervision` | `native` — see [External supervision](#external-supervision) |

Config files are YAML. The default file is `~/.config/qurl/config.yaml`; a
named profile lives at `~/.config/qurl/profiles/<name>.yaml` and is
selected with `--profile` or `QURL_PROFILE`. A missing file simply means
defaults apply. **Config files never hold secrets** — a file carrying an
`api_key` entry is rejected outright rather than silently honored.

`QURL_CONNECTOR_STATE_DIR` selects the durable state namespace for all
commands and `QURL_CONNECTOR_RUNTIME_DIR` (environment-only) selects the
directory that holds its control socket; see
[External supervision](#external-supervision).

Also honored: `QURL_DEPLOYMENT` (a sandbox or custom deployment settings file;
production settings are included in releases; environment-only, with no profile override), `NO_COLOR` (disables color while `--color` is `auto`), and
`QURL_BROWSER` / `BROWSER` (which browser `qurl get` opens). Pointing the
CLI at a plain-`http` endpoint on a non-local address warns that the key
would travel unencrypted; loopback endpoints are exempt.

### Key storage

The local agent state holds this device's credential and identity. When a new
state directory is created on a machine with a TPM 2.0 that `qurl` can use,
that state is sealed to the TPM (`agent_state.sealed.json`). Everywhere else,
the state is a plaintext owner-only file (`agent_state.json`). "Usable" means
the Linux kernel resource manager `/dev/tpmrm0`, which usually requires
membership in the `tss` group, or TPM Base Services on Windows. macOS has no
TPM. The check is made by the process that creates the directory, with that
process's groups: `qurl` run under `sudo -u`, in a container, or from a
session without `tss` creates plaintext state even where an interactive shell
would seal it. A TPM that is present but not responding (busy, starting, or
timing out) fails with exit code 11 instead of falling back to plaintext,
because the choice is permanent; retry, or set `LAYERV_KEY_PROVIDER=file`.

A TPM-sealed directory needs no environment to reopen, so the natively
supervised background job serves it as usual. It opens only on this machine's
TPM as it was when the state was sealed. A backup restored onto other
hardware, a VM clone or image, a TPM clear, or another system later taking
ownership of the TPM (most commonly Windows on a dual-boot machine) leaves it
unreadable, and the error says which. The recovery is to move the state
directory aside and enroll again, which creates a new device identity.
<!-- TODO(upstream-contract): "the error says which" relies on qurl-connector
pkg/agentstate's TPM unseal diagnostics. -->

Under native supervision `qurl` prints a one-time notice on stderr just
before it seals a new directory to the TPM. With `--quiet`, or under an
external supervisor (which normally sets the provider itself), the same
permanent choice is made without it; `qurl whoami` shows the result.

The background job must be able to reach the TPM too. On Linux a systemd user
manager keeps the groups it started with, so after adding yourself to `tss`,
log out fully (or reboot) before the first `qurl publish`; otherwise a
foreground command can seal state that the background job, still without
`tss`, cannot open. Its error then names the TPM.

Existing directories keep the
envelope they were created with and are never migrated in place. `qurl
whoami` shows which one this device uses (`Key storage:`, or `key_storage`
with `-o json`).

`LAYERV_KEY_PROVIDER` overrides that choice when a directory is created.
`file` keeps it plaintext even where a TPM is available, and `tpm` requires
the TPM: without a usable one it exits 3, and with one that is not responding
it exits 11. The other providers seal the state under a key held elsewhere. With
`local-key`, the 32-byte wrapping key arrives on the inherited descriptor
named by `LAYERV_LOCAL_KEY_FD`, on macOS and Linux. There is deliberately no
flag: the supervisor that owns the key (for example qURL Desktop) sets the
environment. A namespace sealed by such a provider requires `--supervision
external`, because the background job `qurl` installs under native
supervision carries no environment and cannot inherit a key descriptor. Every
command that checks the namespace's supervision policy (`publish`, `start`,
`stop`, `restart`, `delete`, `login` and `daemon run`) therefore refuses that
namespace under native supervision with exit code 3, not only the commands
that would install a job. Read-only commands do not run that check and open
the sealed envelope normally.

A state directory holds exactly one envelope. `qurl` refuses a directory
sealed by an environment provider when that provider's variables are missing.
It also refuses a directory whose envelope conflicts with
`LAYERV_KEY_PROVIDER`: a plaintext directory with a sealing provider set, or a
sealed directory with `file` set. `file` over a plaintext directory is the
ordinary case and opens normally. Both refusals exit with code 3. Use a different state directory rather
than switching in place.

## Commands

| Command | Description |
|---------|-------------|
| `qurl publish <target-url>` | Publish a remote URL or serve a loopback HTTP app, and get its CRID |
| `qurl share <CRID>` | Share a CRID as a short-lived access link |
| `qurl grants <CRID>` | Show or change the devices and people allowed to open a private resource |
| `qurl requests [<CRID>]` | List access requests, or turn them on or off for a private resource |
| `qurl approve <CRID> <code>` | Approve one person's request for access to a private resource |
| `qurl deny <CRID> <device id>` | Refuse one person's request for access to a private resource |
| `qurl get <CRID>` | Fetch what a CRID points to: browser on a terminal, or download with `--file` |
| `qurl list` | List your published resources |
| `qurl start <CRID>` | Turn on a previously published local share |
| `qurl stop <CRID>` | Turn off a local share without deleting it |
| `qurl restart <CRID>` | Rotate and restart a local share, or move it to a new loopback target with `--target` |
| `qurl status <CRID>` | Show desired and platform-observed sharing state |
| `qurl inspect <CRID>` | Inspect the same authoritative resource or sharing state |
| `qurl daemon run` | Run the local sharing daemon directly for headless or supervised use |
| `qurl delete <CRID>` | Delete a published resource |
| `qurl account setup` | Link this device to an account for recovery and other devices |
| `qurl account recover` | Restore account resource access on a new device |
| `qurl login` | Enroll this device with a one-time account key, from a supervisor's enrollment token file, or anonymously for a supervisor |
| `qurl whoami` | Show which account this registered device belongs to |
| `qurl publisher` | Show the publisher name shown with your CRIDs; `set <name>` and `clear` change it |
| `qurl request METHOD PATH` | Make a device-authorized JSON request for a supervising app (see [Supervised installs](#supervised-installs)) |
| `qurl completion <shell>` | Generate shell completions (`bash`, `zsh`, `fish`, `powershell`) |
| `qurl version` | Print version information |

Run `qurl <command> --help` for the full help text; installed man pages
cover the same surface (`man qurl`, `man qurl-publish`, …).

Commands that take a CRID assess it locally first: a likely typo (bad
checksum, wrong alphabet) is warned about and still forwarded — the server
is the only authoritative validator. Sending a **test-environment CRID to
the production endpoint** is refused unless `--yes` is given; a production
CRID aimed at a non-production endpoint warns and proceeds.

### qurl daemon run

`qurl daemon run` runs the long-running sharing process in the foreground for
a headless deployment or for a program that supervises the daemon itself.
With no headless flags, it serves the local shares already stored under
`--state-dir`.

#### Headless deployments

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

#### External supervision

When another program — a desktop app, a service manager — owns the daemon
process instead of qURL's per-user background job, start the daemon with
`--supervision external` and run every lifecycle command against that state
directory with the same setting (flag `--supervision`, environment
`QURL_DAEMON_SUPERVISION`, config key `daemon_supervision`):

Use a dedicated, fresh state directory rather than the native default. Enroll
without an account using `qurl login --anonymous --supervision external -o json`,
or use the one-shot token file described in [Supervised installs](#supervised-installs),
then start the daemon. For each command below, the supervising process must
attach a fresh inherited key descriptor and set `LAYERV_KEY_PROVIDER` and
`LAYERV_LOCAL_KEY_FD` as described above. Exporting a descriptor number alone
does not supply the wrapping key:

```bash
# Each command also needs the provider settings and a fresh inherited key fd.
export QURL_CONNECTOR_STATE_DIR="$STATE_DIR"
export QURL_DAEMON_SUPERVISION=external
qurl login --enrollment-token-file "$TOKEN_FILE"
qurl daemon run # keep running under the supervisor
```

Once the daemon is running, lifecycle commands in another process use the same
state, supervision, and key-provider settings. A dedicated state directory
avoids marking the native default namespace by accident; use a different
directory to return to native supervision instead of deleting a marker beside durable credentials.

External supervision changes three things:

- Anonymous or token-file login marks the
  state directory as externally supervised (`runtime_mode.json`). It accepts
  only a directory that holds no natively managed state, and the mark is
  permanent. Starting the daemon before enrollment exits nonzero with
  `no durable account owner`; enroll first rather than retrying that startup.
- `publish`, `start`, and `restart` reload the running daemon and never
  install or replace a background job. When the daemon is not running they
  fail with exit code 11 and roll their own cloud change back, so the
  supervisor can start the daemon and retry.
- `publish`, `start`, `restart`, `stop`, `delete`, `login`, and `daemon run`
  refuse a state directory whose mark does not match their `--supervision`
  setting (exit code 3). A plain `qurl` command therefore never installs a
  background job over a supervised daemon, and a supervised command never
  adopts a natively managed directory. This check applies to the namespace,
  including commands for remote resources. Read-only sharing commands work
  either way, but can still enroll a device and write authentication state.

Private file origins can use a canonical `http+unix:///absolute/socket/path`
target on macOS and Linux. The path must fit within 100 bytes; Windows has no
TCP fallback for this transport. Request-header overlays still require a
trusted TLS tunnel connection.

To convert existing supervised file shares, first stop and verify the daemon
has exited, bind the private origin, then run:

```sh
qurl daemon retarget-local --supervision external -o json <<'JSON'
{"owner_id":"<account-owner>","connector_id_prefix":"qurl-file-","target":"http+unix:///absolute/socket/path"}
JSON
```

Private Unix origins and external `--enrollment-token-file` handoff require
Unix. Windows rejects those paths; ordinary account-API-key login remains
available.

The command uses the normal profile/state-directory settings. It requires an
existing externally supervised namespace and matching durable owner, reserves
the daemon IPC endpoint and lifetime lock, and atomically retargets every saved
row matching the prefix, including stopped shares. It preserves resource IDs,
qURLs, desired state and serving epochs, makes no network requests, and returns
`{"changed":N}` (`0` on an unchanged retry or when the selected prefix has no rows).
Conversion durably reserves one private-origin prefix per namespace even when
`changed` is zero. Older CLI binaries then refuse this registry, and current
writers cannot publish or retarget that prefix back to TCP, even at a newer epoch.
The supervisor owns the exact prefix; it must use the same constant for publishing
and conversion. An owner-bound profile with no file shares is a valid empty set.
The origin must be bound inside an owner-only (0700) directory controlled by
the supervisor, with ancestors other users cannot replace; do not use a socket
directly under a shared temporary directory. The supervisor owns the ancestor
boundary; publish, restart, and the daemon itself (before handing each route
to the Connector) also refuse a socket whose parent is not an owned `0700`
directory. The daemon withholds only that share, reports it as retrying with a
`local_state` failure, and re-checks it on backoff. Conversion is offline and
may precede binding the origin, so it does not dial the target.
Set the same `QURL_CONNECTOR_RUNTIME_DIR` environment value for daemon startup
and conversion (including any value supplied through daemon run's hidden
`--runtime-dir` flag), so the IPC reservation also excludes older daemon binaries.
A live or ambiguous daemon at that endpoint blocks conversion. Start the daemon only after conversion succeeds. New daemon binaries
hold the lifetime lock before loading credentials, closing the startup race.

A supervisor enrolls the device once per state directory with the token-file
form of `qurl login` (see [Supervised installs](#supervised-installs)) and
then follows one lifecycle:

1. Start `qurl daemon run --state-dir "$STATE_DIR" --supervision external`
   and keep the process. On every process start, including warm and headless
   starts, the daemon holds its first reconcile for up to 30 seconds so that
   runtime state is restored before any route is served. After that bound
   it serves the stored shares on its own.
2. Release the first reconcile with `PUT /overlay` (below) or `POST /reload`
   on the control socket. Every lifecycle command sends a reload as well.
3. Poll `GET /status`. The document carries `job_version`, `pid`, `running`
   (resource ID to CRID for every route the session group manages, one
   waiting out a retry included), and `resources`, the same redacted
   per-share diagnostics `qurl inspect` shows; a route is serving only when
   its `resources` entry says so. `pid` is the daemon's
   own process ID, so a daemon the supervisor found running can be stopped
   like one it spawned. Older daemons can report `pid: 0`; treat zero as
   absent and never pass it to `kill`.
4. Stop the daemon with SIGTERM (or SIGINT). It stops its Connector session
   and exits with code 130. Stopping the daemon is a local act: it changes no
   sharing state, so the shares resume on the next start. `qurl stop <CRID>`
   is the opposite: it is cloud-first, turns the share off on the qURL
   platform, and the share stays off until `qurl start <CRID>`. Use it to turn
   a share off, never to pause the daemon.

The control socket is `<state dir>/daemon.sock` when that path fits the
platform's socket-address limit, otherwise an owner-only per-user directory
below `/tmp`.

The environment-only `QURL_CONNECTOR_RUNTIME_DIR` pins it: set to a short
absolute path — a relative path, the filesystem root, and a path whose socket
would exceed the platform limit are all rejected — and the daemon and every
`qurl` command resolve exactly `<dir>/daemon.sock`, which is what a host whose
state path is long, an app container for example, needs.

Name a directory qURL owns. Startup creates missing directories with mode
`0700`. An existing directory must be owned by the caller, must not be a
symlink, and must already have mode `0700`; startup rejects it otherwise
without changing its permissions. Use a separate
dedicated directory for each state namespace; the socket address is the
directory alone, so two namespaces sharing one runtime directory resolve to
the same socket and lifecycle commands for one will address the other's
daemon. Set the variable identically for the daemon and for every command that
addresses the same state directory — a command run without it resolves a
different address and reports the daemon absent.

Windows named pipes have no length limit, so the pipe address ignores this
variable entirely: a value left in the environment there has no effect and is
not an error.

Upgrading from a release before 2.6 moves the derived fallback socket
(`/tmp/layerv-qurl-<uid>/<hash>.sock` becomes
`/tmp/qurl-<uid>-<hash>/daemon.sock`) and adds `pid` to the status document.
Stop the running daemon across an upgrade or a downgrade over that boundary:
a native background job replaces itself, but an externally supervised daemon
keeps listening on the old address until its supervisor restarts it, and a
client released before external supervision cannot decode `pid`. The old
`/tmp/layerv-qurl-<uid>/` directory is left behind and can be removed by hand.

The socket speaks HTTP. When it is in the state directory:

```bash
curl --unix-socket "$STATE_DIR/daemon.sock" http://localhost/status
curl --unix-socket "$STATE_DIR/daemon.sock" -X POST http://localhost/reload
```

All daemon routes verify the tunnel server certificate with the system trust
store and the hostname from authenticated NHP admission. The hosted deployment
uses `connect.layerv.ai` in production and `connect.layerv.xyz` in sandbox.
Certificate and key renewal does not require client updates. Runtime origin
headers are now available on the default per-user daemon without a CA file.

Upgrade note: deployments that previously accepted a self-signed or private-CA
tunnel certificate must now provide `--tunnel-ca-file`. Container images must
include system CA certificates unless a custom CA file is supplied.

<!-- TODO(upstream-contract): NHP infra owns the hosted tunnel domain names. -->

For a private CA, use `qurl daemon run --tunnel-ca-file
/etc/qurl/tunnel-ca.pem`. The PEM file must use an absolute path and replaces
the system CA certificates for that daemon. Use
`--tunnel-server-name <name>` only when the deployment requires a different
certificate identity, such as a server reached by IP address. Invalid trust
configuration fails before enrollment. These options do not provision a server
certificate.

<!-- TODO(upstream-contract): qurl-connector MaxGroupRoutes, header validation
limits, route re-registration, and session rotation/drain semantics. -->

`PUT /overlay` attaches request headers to routes at runtime. The body is
`{"route_request_headers": {"<connector_id>": {"Header-Name": "value"}}}`,
keyed by each share's Connector ID — a supervisor should publish with an
explicit `--id` so it knows this key. The daemon sends the headers over verified TLS to the tunnel server, which
adds them to requests for that share's local origin. The tunnel operator must
therefore be trusted with these credentials. The origin must reject missing
or invalid credentials. Each request replaces
the whole overlay: a route the body does not name loses its headers, and `{"route_request_headers": {}}` clears
it. A valid body is answered with 204. A body over 64 KiB, with unknown
fields, with more than 2,000 routes, with more than 16 headers or 1,024
name-and-value bytes for one route, or with an invalid, reserved, or
case-variant duplicate header name or an invalid value is answered with 400
and a fixed message that never echoes a header. All limits apply together;
larger route entries reduce the number that fits within 64 KiB.

Send secret overlay values from the supervisor process; do not put them in
shell arguments or history.

The overlay lives in process memory only: it is never written to disk, never
reported by `/status` or `qurl inspect`, never logged, and a restarted daemon
starts with an empty one — which is why step 2 pushes it before the first
reconcile. Stopping a share retains its headers until the next overlay
replacement. Republishing the same Connector ID reuses them; replace or clear
the entry before reusing that ID with different credentials. Changing a route's headers re-registers only that route on the
live session; its siblings are untouched. Re-registration may interrupt
in-flight requests. During session rotation the retiring session can retain
old headers until replacement promotion and drain, so an update is not
immediate revocation. See
[docs/session-groups.md](../../docs/session-groups.md#runtime-request-headers).

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
After `qurl delete`, you can reuse that ID to create a new resource with a new CRID.
Old links remain invalid. This also applies to IDs set through `QURL_CONNECTOR_ID`
or the profile's `connector_id`.

If publishing reports an identity conflict after deletion, repeat
`qurl delete <CRID> --yes` with the deleted resource's CRID, then publish again.
This retries the service's name release.
If you no longer have the deleted resource's CRID, publish with a different `--id`.

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
| `--public` | Let anyone who has the CRID open the resource. Without it the resource is private |
| `--allow-device-key <public-key>` | Allow a recipient's device to open the private resource (repeatable) |
| `--allow-requests` | Let people ask you for access to the private resource, also when the target is already published; you approve each person by a code they give you |
| `--description <text>` | Human-readable description stored with the resource |
| `--tag <tag>` | Tag stored with the resource (repeatable) |
| `--alias <name>` | Memorable handle stored with the resource |
| `--id <id>` | Connector ID for a local publish; local-only |

Description, tags, and alias apply only to remote resources. `--id` applies
only to a local publish. Mixing those options fails loudly instead of being
silently ignored.

#### Private or public

A resource is private unless you publish it with `--public`, for a local app
and for a remote URL alike. The text output says which it is in one row, and
`-o json` says it as `private`:

```text
  Access:  private — only you and the people you allow can open it
  Access:  public — anyone who has the CRID can open it
```

Privacy is set when a resource is first published and cannot be changed
afterwards. Publishing a target again reuses its resource, with the privacy it
has. A flag that asks for what that resource is not is refused with exit code
7 and prints nothing on stdout: `--allow-device-key` or `--allow-requests` for
a target that is published as public, `--public` for one that is private, and
`--allow-device-key` with a list other than the one the resource has. The
message says what to do: publish without that flag, change the allowed devices
with `qurl grants`, or delete the resource with `qurl delete <CRID>` and
publish again to get a new resource and a new CRID. `--allow-device-key` and
`--allow-requests` cannot be combined with `--public`. See
[Private CRIDs](#private-crids) for who can open each kind.

With `--allow-requests`, the output also says what to send to people who have
no qURL CLI and what happens next. See
[qurl requests, approve and deny](#qurl-requests-approve-and-deny).

`--allow-requests` also works for a target that is already published as a
private resource: the command turns access requests on for the resource it
finds, and says so in one line ("This target was already published. Access
requests are now on for it."), in the document in text mode and on stderr with
`-o json` and `--quiet`. If they cannot be turned on, the resource stays as it
was, private, and the command fails with the reason and the command that tries
again, `qurl requests <CRID> --on`, with the resource's CRID in it. For a
target that is already published as public it is the exit code 7 above.

**If you published with an earlier release.** Releases from before private
became the default published a public resource unless you asked for a private
one, and those resources are still public. `qurl publish` for such a target
with no privacy flag keeps working: it returns the existing resource, shows
the public `Access:` row, and warns you.

```text
Warning: this target was published as public before, and it stays public: anyone who has the CRID can open it. To make it private, delete it with `qurl delete <CRID>` and publish again; the new resource gets a new CRID.
```

The warning is part of the text output, and goes to stderr with `-o json` and
`--quiet`; JSON says `private: false` and `kept_public: true`. The resource
stays as it is until you delete it. A publish never turns a public resource
private, and it never makes a new public resource unless you pass `--public`.

In either mode, the CRID is last and alone on its line; `--quiet` prints only
the CRID. Publishing the same target again does not create a duplicate while
its resource is active: the existing CRID is returned and the output says so.
JSON reports a known outcome as `found_existing: true` or `false`; if recovery
cannot prove which happened, it omits the field rather than guessing. Delete
the resource first if you intentionally want a new CRID.

### Move a local share

Use `qurl restart <CRID> --target http://127.0.0.1:4000` to move an existing
local share to a new loopback HTTP origin. The destination must be reachable;
the old origin can already be stopped. The CRID and Connector ID stay the same.

| Flag | Description |
|------|-------------|
| `--target <url>` | Move the share to this loopback HTTP origin, e.g. `http://127.0.0.1:4000` |

The destination follows the [local publish rules](#local-apps) — a loopback
HTTP origin without path, query, fragment, or credentials; anything else is a
usage error (exit code 2) before any request is made — and it is the
destination that is preflighted, not the stored origin, which may already be
gone. The platform restart runs as usual, then the new target is stored
together with the serving epoch it returned in one registry write, so no
durable state pairs the old target with the new epoch or the new target with
the old one. The text and JSON documents (`target_url`) report the new target,
and `list` and `status` show the new `Target:` against the unchanged CRID.
`--target` also turns the share back on if it was off, the same way a plain
`restart` does. Without `--target`, `restart` is unchanged.

If the command fails after saving the new target, it keeps that target.
A failed daemon handoff attempts to stop the share; a readiness timeout leaves
it trying to start. Use `qurl inspect <CRID>` to check its state and target
before taking further action. A later `qurl start <CRID>` uses the saved target.

For a share published without `--id`, the default Connector ID remains based
on its original origin. Publishing that original origin again reuses the same
resource. Publishing the new origin without an explicit ID uses a different
identity. Use the share's existing `--id` when publishing it again by ID.

### qurl share

`qurl share <CRID>` mints a short-lived share link for the resource the
CRID names. Treat the minted link as a secret: it is a bearer credential.
The command uses this device's identity. It works on the resource owner's
devices and, for a private resource, on the devices the publisher allowed;
any other device gets "not found".
The link expires on its own; share again whenever you need a fresh one.

`share` also tells you who published the resource and when it was created:

```text
https://qurl.link/#…

  Publisher: "Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)
  Created:   2026-03-01 (1d ago)
  Expires in 5m (single use)
```

The publisher name is whatever the publisher typed. LayerV has not confirmed
who they are, so every publisher is `UNVERIFIED` today: treat the name as a
claim, not as proof. A publisher that set no name shows `no name provided`.
The name is always printed in quotes with unusual characters escaped.

When stdout is not a terminal the command prints the bare link on stdout,
ready to hand out or open, and one notice on stderr:

```text
Warning: UNVERIFIED publisher "Acme Docs" (self-declared name, not confirmed by LayerV). Created 2026-03-01.
```

`--quiet` prints only the link and no notice. `-o json` puts
`resource_created_at` and `publisher` in the document and prints no notice.
That date is when the resource was created, not when the link was minted.

The link opens in a browser. Passing it to a tool like curl fetches the
page that opens the link, not the content itself — to download content
from a script, use `qurl get <CRID> --file <path>`.

| Flag | Description |
|------|-------------|
| `--ttl <duration>` | Requested link lifetime in whole seconds (e.g. `5m`, `1h`). The service may grant less; a shorter grant is reported on stderr, never silent. Sub-second or negative values are refused rather than rounded. |
| `--session-duration <duration>` | Lifetime of each admitted session, e.g. `5m` or `1h`. Zero or omission uses the service default. The service enforces resource limits. |
| `--yes` | Proceed without confirmation, including sending a test CRID to production |

Link expiry and session duration are separate: an expired link does not end an already admitted session.

Production share verification needs no extra settings. Sandbox and custom
deployments use the settings described under `qurl get`.
Before anything is printed, the CLI verifies the signed link against
the CRID you asked for; a mismatched answer is discarded and the command
exits with code 12 without printing a link.

### qurl grants

`qurl grants <CRID>` shows who can open a private resource besides its owner,
and changes that for the resource owner. There are two lists: the public keys
of the devices you allowed, and the people you approved after they asked for
access. With no flag it prints the resource with both lists and says whether
people can still ask; `-o json` has them as `allowed_device_keys`,
`approved_people` and `access_requests`.

```bash
qurl grants <CRID>
qurl grants <CRID> --add <public-key>
qurl grants <CRID> --add <public-key> --remove <other-public-key>
qurl grants <CRID> --remove <device id>
qurl grants <CRID> --clear
```

| Flag | Description |
|------|-------------|
| `--add <public-key>` | Allow a device (repeatable) |
| `--remove <public-key>` | Take a device off the list (repeatable) |
| `--remove <device id>` | Take an approved person's access away; the id has the form `xxxx-xxxx-xxxx-xxxx` (repeatable) |
| `--clear` | Take every public key off the list; approved people stay |
| `--yes` | Proceed without confirmation when a change is sent for a test CRID to production. Reading the list never needs it |

`--add` and `--remove` can be used in one command, which is applied as one
change. A public key that is already on the list, or already off it, is left
as it is, so a command can be repeated safely. Every change prints the
complete lists that result. The same public key cannot be given to both
flags, and `--clear` cannot be combined with either.

An approved person is shown with the name they typed, in quotes, the id of
their device, and when you approved them. The name proves nothing about who
they are. `--remove <device id>` takes that person's access away, for at most
256 people in one command.

The command reads the list first and checks every device id against it before
it takes any access away. A device id that is not on the list is an error
(exit code 5) and removes nothing, wherever it stands among the others, so a
mistyped id is never mistaken for access taken away. Then each person is
removed with a change of their own, before any change to public keys in the
same command.

From the first person removed on, every failure says exactly what happened:
from whom access was taken away, what failed, and who still has access as far
as the command knows. That holds for a removal that fails or finds nobody, for
a list that cannot be read again after the removals, and for a list that still
shows a person the service said it removed. Access that was taken away never
reads as "nothing was removed". A failure that says none of this came before
any access was taken away. `qurl grants <CRID>` shows who has access now.

The same holds when a command that also names public keys removes every person
and then fails to change the keys, or stops before it gets to them. The
command says from whom access was taken away, what failed and why, and the
command that makes the change to the public keys alone. Run that command to
finish: the first one, run again, would stop at the device ids that are
already off the list.

A list holds at most 256 devices. `--add` and `--remove` each take at most 256
public keys in one command, which is checked before anything is sent. The
limit on the list that results is the service's: it refuses a change that
would leave more than 256 devices, and the list stays as it was.

The command checks the service's answer before it reports a change: every
added key must be on the returned list, and no removed key may be. That check
covers the keys the command named, not the rest of the list. What keeps the
other grants in place is the request itself, which carries only the keys to
add and to remove and never the whole list, so it cannot replace it. An answer
that does not show the change fails the check with exit code 10 and nothing
on stdout. A service that cannot add or remove single grants yet answers that
way; `qurl grants <CRID>` then shows the list as it is.

When several changes to one resource arrive together, the service can answer
that this one lost to the others (exit code 11). Nothing was changed, and the
command does not send it again on its own: run it again.

Earlier releases replaced the complete list with
`qurl grants <CRID> --allow-device-key <public-key>`. That form is removed and
is now a usage error that names `--add`, so the command no longer sends a
list that replaces the grants it did not name. `qurl publish
--allow-device-key` still sets the first list when you publish.

### qurl requests, approve and deny

These commands share a private resource with people who have no qURL CLI.
`qurl request`, without the s, is another command: it makes one request for a
supervising app (see [Supervised installs](#supervised-installs)). Each of the
two names the other in its usage error.

1. Turn access requests on: publish with `--allow-requests`, whether or not
   the target is already published as a private resource, or run
   `qurl requests <CRID> --on`. The output says what to send to people: the
   resource's address, when this install knows the web address for its
   deployment, and otherwise the CRID.
2. A person opens it in a browser, sees who published the resource, and asks
   for access. They are shown a six-digit code and give it to you.
3. `qurl requests` lists who asked: the name each person typed and the id of
   their device. It never shows a code. Approve the person who gave you their
   code with `qurl approve <CRID> <code>`, or refuse a request with
   `qurl deny <CRID> <device id>`.

The address and the CRID are safe to send to anyone: a private resource opens
only for you and the people you allow.

**Approve a code only when the person gave it to you themselves.** The code is
shown only to the person who asked, so it is the only proof of who is asking.
No qURL command shows it, in any output mode. If a listing showed the codes,
you, or an agent that runs these commands for you, could approve straight from
the list, and all the list knows about a person is the name they typed. The
name on a request is typed by whoever asked and proves nothing: anyone can
type any name. An agent that runs these commands for you must approve only
codes you passed on to it.

```bash
qurl publish https://wiki.example.com/team --allow-requests
qurl requests                    # pending requests of all your resources
qurl requests <CRID>             # pending requests of one resource
qurl requests <CRID> --on        # let people ask; prints what to send them
qurl requests <CRID> --off       # stop new requests
qurl approve <CRID> 123456       # the code they gave you; also 123 456 and 123-456
qurl deny <CRID> <device id>     # the device id from the listing
qurl grants <CRID>               # who has access now
qurl grants <CRID> --remove <device id>
```

A listing shows, for each request, the name in quotes, the id of the person's
device, how long ago they asked, and when the request expires; the listing of
all resources also shows the CRID. It shows no code. It ends with one line
that says how a person is let in.

```text
NAME         DEVICE ID            REQUESTED  EXPIRES
"Ana Lopez"  abcd-efgh-2345-mnop  2m ago     in 58m

To let one of these people in, ask them for the six-digit code on their screen and run `qurl approve <CRID> <code>`; a name can be typed by anyone, so the code is the only proof of who is asking.
```

The listing of all your resources is bounded. When there may be more requests
than it shows, it says so after the rows, and `-o json` has `has_more: true`.
List one resource with `qurl requests <CRID>` to see all of its requests.

`qurl approve` prints who now has access and the command that takes it away
again. `qurl deny` removes the request and gives no access. It takes the
device id the listing shows, in the form `xxxx-xxxx-xxxx-xxxx`. It also takes
a six-digit code in the same place, for a request whose code you were given
and want to refuse. A request that is neither approved nor denied gives no
access and expires by itself. `--off` stops new requests and says how many
approved people still have access.

| Command | Flag | Description |
|---------|------|-------------|
| `requests` | `--on` | Let people ask for access to this private resource |
| `requests` | `--off` | Stop new requests for access to this resource |
| `requests --on`, `requests --off`, `approve`, `deny` | `--yes` | Proceed without confirmation when a change is sent for a test CRID to production. Listing requests never needs it |

A code that is not pending for the resource is exit code 5. The message says
that it may have expired, or been approved or denied already, and says nothing
about which codes are pending. A device id with no pending request gets the
same answer from `qurl deny`. A value that can never be a code, or for
`qurl deny` neither a device id nor a code, is refused before any request
(exit code 8). Access requests can be turned on only for a private resource.

A service that does not offer access requests yet answers every one of these
commands with "this service does not offer access requests yet" and exit code
11, never with a success, and prints nothing on stdout. That holds whichever
way such a service treats the setting it does not know: `requests <CRID> --on`
and `--off` get that message when it ignores the setting, and when it refuses
the request.

For `publish --allow-requests` the message also says what became of the
publish, and prints no CRID in either case. A service that ignores the setting
published the resource as private, without access requests: run the command
again without `--allow-requests` to see its CRID. A service that refuses the
setting published nothing: run the command again without `--allow-requests` to
publish the resource as private.

### qurl get

`qurl get <CRID>` mints a share link exactly like `qurl share`, with this
device's identity and the same access rule, verifies it, then opens or
downloads — nothing is ever acted on unverified:

- **On a terminal**, get prints the link with the publisher and creation
  date, exactly as `qurl share` does, then opens it in your browser (set
  `QURL_BROWSER` or `BROWSER` to choose which one).
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

Where the deployment offers it, a public resource can also be fetched on any
machine with only its CRID: no account and no setup. The deployment this
release ships does not offer it yet. Where it is offered, `get` asks for a
link with the CRID alone when this device cannot share the resource, and a
machine with no device identity creates none. The three most common answers
when no link is given:

| Answer | Exit code | What it means |
|--------|-----------|---------------|
| not found | 5 | The CRID is mistyped, the resource was removed, or it is not open to this machine. |
| can't give a link right now | 11 | Nothing is wrong with the CRID. Try again later. |
| too many requests | 9 | Wait, then try again. |

| Flag | Description |
|------|-------------|
| `--file <path>` | Download to this path instead of opening a browser (`-` = raw bytes to stdout) |
| `--force` | Allow `--file` to replace an existing file |
| `--session-duration <duration>` | Lifetime of each admitted session, e.g. `5m` or `1h`. Zero or omission uses the service default. The service enforces resource limits. |
| `--yes` | Proceed without confirmation, including sending a test CRID to production |

With `--file`, the publisher notice goes to stderr before the content is
fetched, so you see it before the download: ahead of the saved message for
`--file <path>`, and ahead of the bytes for `--file -`. `--quiet` omits it.
The publisher is `UNVERIFIED`, so decide whether to use the content on what
you know about the CRID's source, not on the name.

When stdout is not a terminal, get never opens a browser: pass `--file`,
or use `qurl share` if you only need the link. With `-o json`, get is a
machine asking for data, so browser mode and `--file -` are refused
loudly; `--file <path> -o json` downloads and emits the outcome document,
which includes `resource_created_at` (the resource's creation date, not the
link's) and `publisher`.

Releases include the production settings used by `share` and `get` to verify
the signed link and its CRID. Production needs no deployment file. For sandbox
or a custom deployment, set `QURL_DEPLOYMENT` to that deployment's settings file. Without usable settings, the command
fails with exit code 3 before printing a link, opening a browser, or downloading.
Direct or pre-signed URLs in share responses are rejected because they cannot
be checked against the advertised CRID.

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
undone: the CRID can no longer be shared, and republishing the same target
later mints a different CRID.

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
requires the saved local target to be reachable. `restart` re-registers just
that share on the Connector session under a fresh serving epoch, so a stale
session cannot keep serving it; the other shares are not disturbed. `status`
and `inspect` use the same authoritative view. Both work for remote resources
and include the local target only when this machine owns one. Both also show
the `Publisher` and `Created` rows that people who request a link are shown;
`qurl grants` prints the same rows. `start`, `stop`, and `restart` report the
change and print no publisher rows. All five return `publisher` and
`created_at` in `-o json`.

`restart --target <origin>` additionally moves the share to a different
loopback origin on this machine, keeping its CRID and links; see
[Move a local share](#move-a-local-share).

Custom deployments must support the current CLI resource-status API. The CLI
does not scan the full account inventory when one resource-status request
fails.

One daemon serves every local share on **one Connector session**: it knocks,
logs in, and holds one heartbeat stream once for the whole machine, then serves
each share as its own route on that session. A share can fail, restart, or be
added or removed on its own without touching the others, and the daemon still
recovers assignment, sleep/wake, and network failures automatically with
persisted bounded backoff — customers never need a refresh approval flag. One
machine serves up to 2000 local shares (see [Scale](#scale)). On macOS the
first local `publish` or `start` installs an owner-only LaunchAgent. On Windows
it installs a least-privilege per-user Task Scheduler job. The installed `qurl`
path survives normal upgrades, and a binary-version change reloads the resident
daemon deliberately. On macOS, replacement waits for the prior daemon to finish
its configured shutdown before it retries startup. If that shutdown times out,
retry the command to restore the background job.
Ordinary lifecycle commands reload desired state over an
owner-only local control channel without restarting healthy sibling shares.

<!-- TODO(upstream-contract): the grace mirrors qurl-connector groupControlRecoveryGrace. -->
If the tunnel control connection cannot restore its proxy table within 25
seconds, the daemon retires that session and requests fresh admission. Brief
connection losses can recover with the existing session.

### Scale

A single machine can publish up to 2000 local shares under one account. Every
share is one route on the daemon's single Connector session rather than its own
session, so the whole set costs one knock, one login, one authorization stream,
and one heartbeat stream — not one of each per share. Publishing, starting,
stopping, or restarting a share reconciles the live session in place: a new
share joins without a second knock, a stopped or deleted share is dropped, and
a restart re-registers only that share's route. See
[docs/session-groups.md](../../docs/session-groups.md) for the model and its
per-share failure isolation.

`qurl list` prints every full CRID. For locally registered tunnel rows it also
prints the canonical loopback target and durable desired state. The paged list
does not make one live API request per row, so its observed column is
`unknown`; use `qurl status <CRID>` for the authoritative `stopped`,
`connecting`, or `serving` observation. If the owner-only local registry is
unavailable, list omits local targets and emits one warning.

### Session group modes

The daemon has two ways of mapping local shares onto Connector sessions,
selected by the `QURL_SHARE_GROUP_MODE` setting (flag `--share-group-mode` on
`qurl daemon run`, config key `share_group_mode`):

| Mode | What it does | When to use it |
|------|--------------|----------------|
| `single` (default) | Every share is one route on **one** session: one knock, one login, one heartbeat stream for the whole machine. | The target model; use it whenever the platform authorizes every route of a Connector on one session. |
| `per-share` | Every share runs on **its own** session: one knock, one login, and one journal per share, rotated per share. | The compatibility mode for a platform that admits a session's routes only for the one resource that session was signed for. In `single` mode against such a platform, every share beyond the first is refused and shows as `retrying` / `platform_denied` in `qurl inspect`. |

Both modes keep the same lifecycle semantics — `publish` adds a share, `stop`
removes it, `restart` re-registers only that share, and a refused or
permanently denied share never disturbs its siblings — and the same `qurl
status` / `qurl inspect` output. `per-share` simply pays the per-session cost
for each share, so the per-owner platform budgets for sessions and heartbeat
streams cap it well below the [2000-share scale](#scale) of `single`; above
300 shares the daemon logs a warning so a retrying excess can be attributed
to that budget. Start-up in `per-share` mode admits the shares one after
another on the same shared admission path, so a large fleet takes roughly one
knock round-trip per share to come fully up.

The mode is part of the daemon's job definition. To switch a machine that is
already sharing, set the mode durably and run any command that installs the
job — `qurl start <CRID>`, `qurl restart <CRID>`, or `qurl publish` — which
replaces the resident daemon in the new mode, exactly as a version upgrade
would:

```bash
printf 'share_group_mode: per-share\n' >> ~/.config/qurl/config.yaml
qurl start <CRID>
```

Setting only the environment variable works the same way but lasts one
command: the next lifecycle command run without it resolves `single` again and
switches the daemon back. Put the setting in the config file for anything
durable. A headless `qurl daemon run` reads the same setting, or takes
`--share-group-mode` directly.

### qurl login / whoami

`qurl login` reads the account key from piped stdin or a hidden interactive
prompt — never as an argument — validates it, enrolls the registered device,
checks that the device belongs to the same account, and discards the account
key. `qurl whoami` checks the registered device and shows its account identity
and the device's public key (`device_public_key_b64` in `-o json`), plus which
[key storage](#key-storage) protects local state (`key_storage`), with no plan
or usage data. It never shows the device's private key or any part of an API
key secret: since 3.2.0 it no longer prints the key prefix (`lv_live_…`), and
`-o json` no longer has `api_key.key_prefix`; use `api_key.key_id` to identify
the key. `--quiet` prints just the owner id.

```bash
op read op://team/qurl/key | qurl login
qurl whoami -o json
```

A supervisor such as qURL Desktop can instead run
`qurl login --enrollment-token-file /absolute/path/to/token --supervision external -o json`.
It must supply a one-time token minted for `target=agent`, set
`LAYERV_KEY_PROVIDER=local-key`, and pass the 32-byte wrapping key through the
inherited descriptor named by `LAYERV_LOCAL_KEY_FD`. Do not set `QURL_API_KEY`
or `QURL_API_KEY_FILE` for this form. Token-file login is tested on macOS and
Linux. Non-Unix platforms, including Windows, reject it before changing local state.

The token file must be an owner-owned regular file with one hard link and
mode `0400` or `0600`. The CLI reads it only when enrollment needs it. The
supervisor must remove it after the command exits, including on failure.
An enrolled device can log in again with the same path after the file is
removed. Invalid command options return exit code 2; an unusable token file
returns 4; incompatible encrypted-state settings return 3. Correct a token
file error and retry with the same state directory. A namespace with a different
supervision mode returns 3; one bound to another account returns 7.
If the stored device credential is revoked, this form cannot recover it with
account-key authority: preserve the old state directory and enroll a new
namespace with a fresh token. Login JSON includes
`owner_id`, `auth_type`, `device_enrolled`, and `device_key_id` when the
service supplies a key ID.

### qurl publisher

`qurl publisher` shows the publisher profile people see with your CRIDs.
Anyone who requests a link with `qurl share` or `qurl get` is shown the name
you set here, or `no name provided` when you set none.

```bash
qurl publisher                    # show the current profile
qurl publisher set "Acme Docs"    # set or replace the name
qurl publisher clear              # remove the name
```

```text
Publisher: "Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)
```

The name is optional and self-declared. Setting one does not confirm who you
are: LayerV cannot confirm any publisher yet, so every publisher is shown as
`UNVERIFIED`.

The same name appears on every CRID this device's owner publishes, including
ones already published, so people can tell those CRIDs come from the same
publisher.

<!-- TODO(upstream-contract): qurl-service owns the publisher naming rules. -->
A name is 1 to 64 characters: letters, digits, single spaces, and common
punctuation. It cannot spell "verified", "LayerV" or "qURL", even split by
spaces or punctuation. The service checks the name; a refused name exits
with code 8 and the reason on stderr.

`-o json` prints `{"publisher": {"name": "...", "verified": false}}`, with
`name` omitted when none is set. `--quiet` prints only the name, with any
non-printing characters escaped, and nothing when none is set.

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
| `--supervision native\|external` | Who runs the sharing daemon: qurl's per-user background job, or another program running `qurl daemon run` — see [External supervision](#external-supervision) |

## Scripting contract

- **stdout carries data, stderr carries everything else.** `qurl share`
  piped into another command prints the bare link and nothing more on
  stdout; notes, warnings, and confirmation prompts go to stderr.
- **`qurl share` and `qurl get --file` report the publisher on stderr** as
  one line: `Warning: UNVERIFIED publisher "<name>" (...)`. stdout is
  unchanged, so `link="$(qurl share <CRID>)"` still captures only the link.
  Do not parse that line: read `publisher` from `-o json`. Pass `--quiet`
  when a script requires an empty stderr.
- **`--quiet` prints only the primary value**, one per line: the CRID for
  `publish`, the link for `share`, full CRIDs for `list`, the
  destination path for a `get --file` download, the owner id for
  `whoami` and `login`, the publisher name for `publisher`, the device id
  for `requests <CRID>`, the CRID and the device id for `requests` with no
  argument, the device id or the code you gave for `deny`, the CRID for
  `requests --on` and `--off`, and the device id for `approve`. A listing
  that may be incomplete says so on stderr.
- **Verification is built in:** before printing anything, `qurl share`
  and `qurl get` check the service's answer against the CRID you asked
  for and discard mismatches (exit 12).

`publish -o json` always includes `private`: `true` unless the resource was
published with `--public`, or was already published as public and kept. In
that second case, a publish with no privacy flag that kept an existing public
resource, the document also has `kept_public: true`; the member is absent
otherwise. It is how a script tells that case from `--public` on an existing
resource, which has the same `private: false` and `found_existing: true`,
without reading stderr. `--quiet` prints only the CRID in both cases.

`list` and resource status JSON include `private` when known and an
`allowed_device_keys` array, including `[]` when no devices are allowed. The
text resource list includes a `PRIVATE` column. Grant changes show the
resulting complete device list in text and JSON output.

Access requests in `-o json`:

| Command | Members |
|---------|---------|
| `publish` | `access_requests` when the service's answer says whether people can ask; `resource_url`, the resource's address for people with no CLI, only when access requests are on and this install knows the web address for its deployment |
| `requests`, `requests <CRID>` | `requests`: an array, `[]` when there are none, of `name` (omitted when the person typed none), `name_verified` (always `false`), `device_id`, `requested_at`, `expires_at`, `crid`. No member holds a request's code. Beside it, `approval_rule`: always present, one sentence; and `has_more`: always present, `true` when there may be more requests than the listing shows |
| `requests <CRID> --on`, `--off` | `crid`, `access_requests`, and `resource_url` under the same rule as `publish` |
| `approve` | `crid`, `approved` (`true`), `device_id`, `name`, `name_verified` (always `false`), `approved_at`, and `name_note`: always present, one sentence |
| `deny` | `crid`, `denied` (`true`), and what you named the request by: `device_id`, or `code` |
| `grants` | the resource document with `allowed_device_keys`, `approved_people` (an array, `[]` when there are none, of `name`, `name_verified`, `device_id`, `approved_at`), and `access_requests` when the service says |
| `grants --remove <device id>` that did not finish | `crid` and three arrays that are always present and together hold every device id the command named: `removed`, `not_found`, `not_removed`. `public_keys_changed` (`false`) when a removal failed and the command also named public keys. `public_keys_command` when every person was removed and the change to the public keys failed or was not reached: the command that makes that change alone |

A requester's `name` is that person's own text, exactly like a publisher's:
quote or escape it before showing it, and never treat it as proof of who
asked. `name_verified` is always present and always `false`.

Two members carry, for a reader of JSON, the sentences the text output has.
An agent that runs these commands reads JSON, so the rule it must follow is
in the document it reads:

- `approval_rule`, in both listings: "To let one of these people in, ask them
  for the six-digit code on their screen and run `qurl approve <CRID> <code>`;
  a name can be typed by anyone, so the code is the only proof of who is
  asking." It is one member beside `requests`, not one for each request, and
  it is there for an empty listing too.
- `name_note`, in the `approve` document: "The name was typed by the person
  who asked. Nobody checked it."

A removal that fails is the one failure that writes a document to stdout:
exit code 5 when a device id was not found, with the message on stderr as in
text mode. Read `removed` before deciding what to do next: those people have
lost access, whatever the exit code says. The document is written for every
failure from the first person removed on; a failure with no document came
before any access was taken away. When `public_keys_command` is present, every
person was removed and only the change to the public keys is left: running
that command finishes the job.

### Exit codes

Exit codes are stable. The meanings below mirror the CLI's single
exit-code authority in code (`apps/cli/internal/exitcode`):

| Code | Name | Meaning |
|-----:|------|---------|
| 0 | success | The command did what was asked. |
| 1 | general | An unclassified failure, including features not yet available in this build. |
| 2 | usage | The command line itself was wrong: flags, arguments, or missing confirmation. |
| 3 | configuration | Settings or profiles are invalid, or this CRID needs a newer CLI. |
| 4 | authentication | No credential, an implausible credential, or the service rejected the credential. |
| 5 | not found | The resource does not exist or is retired — revoked and tombstoned resources included; the stderr message distinguishes them. `share` and `get` also get this answer on a device that is neither the owner's nor allowed; the service does not say which. Also an access-request code or device id that is not pending for the resource, and a device id that is not among its approved people. |
| 6 | permission | The credential lacks permission for this operation. |
| 7 | conflict | The request conflicts with current state — including `--file` refusing to replace an existing destination without `--force`, and `publish` with a flag that asks for what the already published resource is not: the other privacy, or another list of allowed devices. |
| 8 | invalid input | An operand or request rejected as invalid (by the service, or locally for inputs that can never be valid). |
| 9 | rate limited | Still rate limited after the CLI's bounded automatic retries. |
| 10 | server error | The service failed or answered outside its contract — including a `publish` answer that does not confirm the privacy that was asked for, and a `grants` answer that does not show the change that was asked for. |
| 11 | unavailable | The service cannot be reached or is not serving this surface: HTTP 503, network failures, timeouts, and a service that does not offer access requests yet. Also a local TPM that is not responding. |
| 12 | verification failed | The response failed CRID-anchored verification. Nothing was printed — treat it as tampering, not transience. |
| 130 | interrupted | The foreground daemon or another command was canceled with Ctrl-C or SIGTERM. |

### JSON output (`-o json`)

Every command's `-o json` document uses field names owned by this repo —
a stable contract independent of upstream renames. Fields that only
sometimes apply (`found_existing`, `kept_public`, `already_gone`) are omitted
rather than emitted empty. Every resource result requires a verified `crid`.

For `qurl list`, **`has_more` — not `next_cursor` presence — is the
pagination terminator.** The service legitimately serves short and even
zero-item pages with `"has_more": true`, so consumers must keep following
`next_cursor` until `"has_more": false`; `has_more` is always emitted for
exactly that reason.

```bash
qurl list -o json | jq -r '.resources[].crid'
```

Where the publisher appears:

| Command | Text | `-o json` |
|---------|------|-----------|
| `share` | `Publisher` and `Created` rows on a terminal; one stderr notice when piped | `publisher`, `resource_created_at` |
| `get` | Same rows before the browser opens; one stderr notice before a `--file` download | `publisher`, `resource_created_at` (`--file <path>`) |
| `status`, `inspect`, `grants` | `Publisher` and `Created` rows | `publisher`, `created_at` |
| `publisher`, `publisher set`, `publisher clear` | `Publisher` row | `publisher` |
| `publish`, `list` | Not shown | `publisher`, `created_at` |
| `start`, `stop`, `restart` | Not shown | `publisher`, `created_at` |

```json
"resource_created_at": "2026-03-01T00:00:00Z",
"publisher": { "name": "Acme Docs", "verified": false }
```

`verified` is always present and is `false` for every publisher today. `name`
is omitted when the publisher set none.

The resource's creation date has two keys. `share` and `get --file` describe
a minted link, so they name it `resource_created_at`: it is the resource's
creation date, not the link's. `publish`, `status`, `inspect`, `grants`,
`start`, `stop`, `restart`, and `list` rows describe the resource itself and
use `created_at`. Either key is omitted when the service did not report the
date.

A name is the publisher's own text: quote or escape it before showing it, and
never show it without its `verified` status. An agent that acts for a person
must tell that person the publisher is unverified.

```bash
qurl share <CRID> -o json | jq '{link: .qurl, publisher: .publisher}'
```

Account setup and recovery return `owner_id` and `status` (`linked` or
`recovered`) with `-o json`. With `--quiet`, they print only the owner ID.

### Private CRIDs

A resource is private unless you publish it with `--public`
(`qurl publish <target-url> --public`). A private resource is limited to its
owner and the devices allowed with `--allow-device-key <public-key>`. This
works for remote URLs and local apps. Earlier releases published a public
resource unless `--private` was given; that flag is still accepted and asks
for what a publish does anyway, except that it is refused for a target that is
already published as public. A resource those releases published is still
public, and `qurl publish` with no privacy flag keeps it and says so; see
[Private or public](#private-or-public).

`qurl share <CRID>` and `qurl get <CRID>` use this device's identity, so an
allowed device needs no LayerV account or browser login. They work on the
resource owner's devices and, for a private resource, on the devices the
publisher allowed; any other device gets "not found". To be allowed on a
private resource, a recipient sends the publisher their device's public key
from `qurl whoami -o json`.

Where the deployment offers it, a public resource can also be opened by anyone
who has its CRID. The deployment this release ships does not offer it yet. See
[`qurl get`](#qurl-get) for how that works.

Read the current list with `qurl grants <CRID>`. With `-o json` it is
`allowed_device_keys`, an empty array when there are no grants. The publisher
allows a device, or takes one off the list, without touching the others:

```sh
qurl grants <CRID> --add <public-key>
qurl grants <CRID> --remove <public-key>
qurl grants <CRID> --clear
```

<!-- TODO(upstream-contract): keep the grant limit and key encoding in sync with qurl-service. -->
`--clear` removes all device grants. The list accepts up to 256 canonical
X25519 public keys. Privacy is set at creation; a retry cannot change it.
Device grants have no effect on a public resource.
Removing a device stops new link requests. Previously issued links retain
their expiry and revocation rules.

A device key is for a recipient who runs the qURL CLI. For people who do not,
turn on access requests and approve each person by the code they give you:
see [qurl requests, approve and deny](#qurl-requests-approve-and-deny).
