# qurl-integrations

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Open-source integrations for [qURL™](https://layerv.ai) — Quantum URLs that make protected resources invisible by default.

qURL is built on [OpenNHP](https://github.com/OpenNHP/opennhp) (Network-infrastructure Hiding Protocol), a cryptography-driven protocol that makes servers, ports, and domains invisible to unauthorized users. A qURL wraps any resource behind a short-lived, policy-bound, cryptographically protected access token. When the token is resolved, an NHP knock grants the caller's IP temporary access — the resource literally does not exist on the network until that moment. Think of it like quantum observation: the resource only becomes visible when an authorized user observes it.

This monorepo contains qURL integrations across several surfaces — a Slack app and a CLI tool (Go), a Discord app (Node.js), and a Chrome extension for Gmail — plus shared Go libraries. Microsoft Teams and Zapier are planned.

## Structure

```
apps/                Per-integration apps (independent release tracks)
  slack/             Slack Secure Access Agent — /qurl slash commands (Go)
  discord/           Discord app — one-time qURL links for files & locations (Node.js)
  chrome-extension/  Chrome extension — Gmail file uploads as expiring qURL links (MV3)
  cli/               CLI — create & manage qURLs from the terminal (Go)
  teams/             Microsoft Teams (planned)
  zapier/            Zapier integration (planned)
shared/              Shared Go libraries used by the Go apps
  client/            qURL API client
  auth/              API key helpers
  events/            Webhook event parsing
  formatting/        Chat message templates
  observability/     OpenTelemetry setup
```

## SDKs & MCP server (separate repos)

Language SDKs and the qURL MCP server live in standalone repositories:

| Library | Install | Repo |
|---------|---------|------|
| Python SDK | `pip install layerv-qurl` | [layervai/qurl-python](https://github.com/layervai/qurl-python) |
| TypeScript SDK | `npm install @layervai/qurl` | [layervai/qurl-typescript](https://github.com/layervai/qurl-typescript) |
| MCP server | `npx @layervai/qurl-mcp` | [layervai/qurl-mcp](https://github.com/layervai/qurl-mcp) |

## Configuration

The Slack, Discord, and CLI apps connect to the qURL API:

- **Endpoint** — `QURL_ENDPOINT`: production `https://api.layerv.ai`, sandbox `https://api.layerv.xyz`. Required for Slack; the CLI and Discord fall back to a default.
- **Authentication** — an API key in `QURL_API_KEY`: `lv_live_…` (production) or `lv_test_…` (sandbox).

The Chrome extension uploads to a qURL file server instead; see its [README](apps/chrome-extension/README.md) for configuration.

## Slack Connector Onboarding

Onboarding is install-first: install the qURL Slack app, run `/qurl setup <email>`,
then an admin runs `/qurl-admin protect` to expose a resource in a channel and
anyone runs `/qurl get` to mint a one-time link.

See [apps/slack/README.md](apps/slack/README.md) for the full command reference
and onboarding walkthrough, and [apps/slack/docs/operating.md](apps/slack/docs/operating.md)
for deploying and operating the Secure Access Agent.

## Development

```bash
# Install pre-commit hooks
pip install pre-commit && pre-commit install

# Run all checks (lint, vet, test)
make check

# Run all tests
go test ./...

# Run tests for a specific app
go test ./apps/slack/...

# Build Slack Lambda
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bootstrap ./apps/slack/cmd/
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development workflow, PR requirements, and code conventions.

## Releases

This repo uses [Release Please](https://github.com/googleapis/release-please) in monorepo mode.
Each app has an independent version track:

- Commits scoped to an app bump only that app: `feat(slack): add thread replies` → `slack/v0.2.0`
- Changes to `shared/` bump all apps
- Each app has its own `CHANGELOG.md`

## CI

Each app has a path-filtered workflow that only runs when its code (or shared code) changes.
A `shared-test.yml` workflow runs all app tests when `shared/` is modified.

## License

[MIT](LICENSE) — Copyright (c) 2025-present LayerV, Inc.
