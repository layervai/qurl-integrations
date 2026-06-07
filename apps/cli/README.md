# qURL CLI

Create, resolve, and manage **qURLs** — secure, time-limited access links — from
your terminal or a script.

A **qURL** (Quantum URL) wraps a protected resource behind a short-lived,
policy-bound access link. The resource stays invisible on the network until an
authorized caller resolves the link, which opens firewall access for that
caller's IP for a limited window. Links expire on their own and can be revoked
at any time.

## Install

**Homebrew** (macOS / Linux):

```bash
brew install layervai/tap/qurl
```

**Debian / RPM** — download the `.deb` or `.rpm` for your architecture from the
[latest release](https://github.com/layervai/qurl-integrations/releases) and
install it with `dpkg -i` / `rpm -i`.

**Prebuilt binaries** — download the archive for your OS and architecture
(`linux`, `darwin`, `windows` × `amd64`, `arm64`) from the
[releases page](https://github.com/layervai/qurl-integrations/releases), extract
it, and put the `qurl` binary on your `PATH`.

Confirm the install:

```bash
qurl version
```

## Authentication

Every command talks to the qURL API with an API key (`lv_live_…`). The CLI looks
for it in this order:

1. `--api-key` flag — visible in the process list, so prefer one of the below
2. `QURL_API_KEY` environment variable — recommended
3. `~/.config/qurl/config.yaml` — written by `qurl config set`

```bash
# Recommended: environment variable
export QURL_API_KEY=lv_live_xxx

# Or persist it to the config file
qurl config set api_key lv_live_xxx
```

The API endpoint defaults to the production host `https://api.layerv.ai`.
Override it with `--endpoint`, the `QURL_ENDPOINT` environment variable, or
`qurl config set endpoint <url>` (for example, to point at a sandbox).

## Quickstart

```bash
# Create a qURL that expires in 24 hours
qurl create https://api.example.com/data --expires 24h

# List your active qURLs
qurl list --status active

# Resolve an access token (opens firewall access for your IP)
qurl resolve at_k8xqp9h2sj9lx7r4a

# Revoke a qURL when you're done with it
qurl delete r_k8xqp9h2sj9 --yes
```

`qurl create` prints the qURL link (and details) to share. Add `--quiet` to print
only the link — handy in scripts:

```bash
LINK=$(qurl create https://api.example.com/data -e 1h --quiet)
```

## Commands

| Command | Description |
|---------|-------------|
| `qurl create <target-url>` | Create a qURL for a target URL |
| `qurl list` | List your qURLs |
| `qurl get <resource-id>` | Show details for one qURL |
| `qurl resolve <access-token>` | Resolve a token and open firewall access (headless) |
| `qurl mint <resource-id>` | Mint a fresh access link for an existing qURL |
| `qurl extend <resource-id>` | Extend a qURL's expiration |
| `qurl update <resource-id>` | Update a qURL's properties |
| `qurl delete <resource-id>` | Revoke a qURL and its tokens |
| `qurl quota` | Show usage quota and plan info |
| `qurl config` | Manage CLI configuration and profiles |
| `qurl completion <shell>` | Generate shell completions (`bash`, `zsh`, `fish`, `powershell`) |
| `qurl version` | Print version information |

Run `qurl <command> --help` for the full flag list. Frequently used flags:

| Flag | Commands | Description |
|------|----------|-------------|
| `-e, --expires <dur>` | `create` | Expiration duration, e.g. `1h`, `24h`, `7d` |
| `--one-time` | `create` | Single-use token, consumed after the first access |
| `--max-sessions <n>` | `create` | Cap concurrent sessions (`0` = unlimited) |
| `-d, --description <s>` | `create`, `update` | Human-readable description |
| `-b, --by <dur>` | `extend` | Duration to extend by, e.g. `24h` |
| `--status <s>` | `list` | Filter by `active`, `expired`, `revoked`, or `consumed` |
| `-y, --yes` | `delete` | Skip the confirmation prompt |

### Global flags

| Flag | Description |
|------|-------------|
| `--api-key <key>` | API key (prefer `QURL_API_KEY` or config to keep it out of the process list) |
| `--endpoint <url>` | API endpoint (default `https://api.layerv.ai`) |
| `-o, --output <fmt>` | Output format: `table` (default) or `json` |
| `-q, --quiet` | Print only the essential value |
| `-v, --verbose` | Show HTTP request/response details |
| `--profile <name>` | Use a named config profile (`~/.config/qurl/profiles/<name>.yaml`) |

`--output json` makes every command emit machine-readable JSON, so the CLI drops
cleanly into scripts and CI:

```bash
qurl list --status active --output json | jq -r '.qurls[].resource_id'
```

## Configuration profiles

Keep separate credentials and endpoints (for example, production and sandbox) in
named profiles:

```bash
qurl config set api_key lv_live_xxx --profile prod
qurl config set endpoint https://api.layerv.xyz --profile sandbox
qurl --profile sandbox list
```

`qurl config profiles` lists what you have, and `qurl config path` prints the
config file location.

## Shell completions

```bash
# zsh (add to ~/.zshrc or a completions directory)
source <(qurl completion zsh)
```

Homebrew installs completions and the `qurl(1)` man page automatically.

## Build from source

The CLI is part of the [qurl-integrations](https://github.com/layervai/qurl-integrations)
Go module:

```bash
go build -o qurl ./apps/cli/cmd/
go test ./apps/cli/...
```

## License

[MIT](../../LICENSE) — Copyright (c) 2025-present LayerV, Inc.
