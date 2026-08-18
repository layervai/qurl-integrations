# qURL CLI

Publish URLs as protected resources and turn their **CRIDs** back into
working access links — from your terminal or a script.

A **qURL™** resource is a protected target with a permanent, verifiable ID:
its CRID. Publish once, share the CRID anywhere (it contains no secrets),
and anyone authorized can resolve it into a short-lived access link when
they need one. Links expire on their own; the CRID does not.

```bash
# Publish a URL and get its CRID
qurl publish https://api.example.com/reports

# Turn a CRID into a working access link (composes with anything)
curl "$(qurl resolve <CRID>)"
```

## Install

**Homebrew** (macOS / Linux):

```bash
brew install layervai/tap/qurl
```

**Debian / RPM** — download the `.deb` or `.rpm` for your architecture from
the [latest release](https://github.com/layervai/qurl-integrations/releases)
and install it with `dpkg -i` / `rpm -i`.

**Prebuilt binaries** — download the archive for your OS and architecture
(`linux`, `darwin`, `windows` × `amd64`, `arm64`) from the
[releases page](https://github.com/layervai/qurl-integrations/releases),
extract it, and put the `qurl` binary on your `PATH`.

Confirm the install:

```bash
qurl version
```

## Authentication

Every command talks to the qURL API with an API key (`lv_live_…` for
production, `lv_test_…` for test). There is deliberately no `--api-key`
flag — command-line arguments leak into shell history and process lists.
The CLI looks for the key in this order:

1. `QURL_API_KEY` environment variable — recommended for scripts and CI.
   When set, nothing on disk is read or written.
2. The key `qurl login` stored: in your OS keyring (Keychain on macOS,
   Credential Manager on Windows, the freedesktop Secret Service on Linux),
   or — only where no keyring is available — in `~/.config/qurl/token`, a
   file readable by your user alone. Commands warn when the key is served
   from that file. Other LayerV tools (the SDK, the Connector) read only
   that file, never the keyring — so on keyring machines, give them the
   key via `QURL_API_KEY` rather than expecting them to see `qurl login`'s.

`qurl login` checks the key against the qURL service before storing it, so
a mistyped key fails loudly instead of breaking every later command.
`qurl logout` removes the stored key from every place it may sit, and
`qurl whoami` shows which account and key identity the configured
credential maps to.

The API endpoint defaults to the production host `https://api.layerv.ai`.
Override it with `--endpoint`, the `QURL_ENDPOINT` environment variable, or
a profile file (`~/.config/qurl/profiles/<name>.yaml`, selected with
`--profile` or `QURL_PROFILE`). Config files never hold secrets.

## Commands

| Command | Description |
|---------|-------------|
| `qurl publish <target-url>` | Publish a URL as a protected resource and get its CRID |
| `qurl resolve <CRID>` | Turn a CRID into a short-lived access link |
| `qurl get <CRID>` | Fetch what a CRID points to (arrives in an upcoming release) |
| `qurl list` | List your published resources |
| `qurl delete <CRID>` | Delete a published resource |
| `qurl login` / `qurl logout` | Store your API key (validated first, OS keyring preferred) / remove it everywhere |
| `qurl whoami` | Show which account and key identity your credential maps to |
| `qurl completion <shell>` | Generate shell completions (`bash`, `zsh`, `fish`, `powershell`) |
| `qurl version` | Print version information |

Run `qurl <command> --help` for the full flag list. Global flags:

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
  piped into another command prints the bare link and nothing more.
- **Exit codes are stable:** 0 success · 1 general · 2 usage · 3
  configuration · 4 authentication · 5 not found · 6 permission · 7
  conflict · 8 invalid input · 9 rate limited · 10 server error · 11
  service unavailable · 12 verification failed (nothing was printed) · 130
  interrupted.
- **Verification is built in:** before printing anything, `qurl resolve`
  checks the service's answer against the CRID you asked for and discards
  mismatches (exit 12).
