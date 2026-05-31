# qurl-integrations

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Open-source integrations for [qURL™](https://layerv.ai) — Quantum URLs that make protected resources invisible by default.

qURL is built on [OpenNHP](https://github.com/OpenNHP/opennhp) (Network-infrastructure Hiding Protocol), a cryptography-driven protocol that makes servers, ports, and domains invisible to unauthorized users. A qURL wraps any resource behind a short-lived, policy-bound, cryptographically protected access token. When the token is resolved, an NHP knock opens temporary firewall access for the caller's IP — the resource literally does not exist on the network until that moment. Think of it like quantum observation: the resource only becomes visible when an authorized user observes it.

This monorepo contains Go-based platform integrations (Slack, Teams, Discord), a CLI tool, and shared libraries for creating and managing qURLs.

## Structure

```
apps/           Per-integration applications (independent release tracks)
  slack/        Slack bot — slash commands, link unfurling, notifications
  cli/          CLI tool for qURL
  teams/        Microsoft Teams (planned)
  discord/      Discord bot (planned)
  zapier/       Zapier integration (planned)
shared/         Shared libraries used by all integrations
  client/       qURL API client
  auth/         API key helpers
  events/       Webhook event parsing
  formatting/   Chat message templates
  observability/ OpenTelemetry setup
```

## SDKs (separate repos)

Language-specific SDKs have been extracted into standalone repositories:

| SDK | Package | Repo |
|-----|---------|------|
| Python | `pip install layerv-qurl` | [layervai/qurl-python](https://github.com/layervai/qurl-python) |
| TypeScript | `npm install @layerv/qurl` | [layervai/qurl-typescript](https://github.com/layervai/qurl-typescript) |

## Configuration

All integrations connect to the qURL API. The endpoint is configurable via the `QURL_ENDPOINT` environment variable (defaults to `https://api.layerv.xyz`). Authentication is handled via API keys set in the `QURL_API_KEY` environment variable.

## Slack Tunnel Onboarding

Customer onboarding is install-first: install the qURL Slack app, run `/qurl setup`, then use `/qurl-admin tunnel install` or `/qurl get`. The Slack app install stores the workspace bot token for modal support; customers do not manually provide a bot token. Admin verbs live under a separate `/qurl-admin` slash command (registered against the same request URL as `/qurl`); user verbs — including `setup` — stay on `/qurl`.

Slack admins can run `/qurl-admin tunnel install` to generate a guided install for the qURL reverse-tunnel sidecar. The workflow asks for the customer-facing tunnel ID, an optional channel shortcut, the local HTTP port, and the target runtime. Slack then returns copy/paste output tailored for Docker, Docker Compose, AWS ECS/Fargate, or Kubernetes.

The Docker and Docker Compose paths create the proxy config, bootstrap-key secret, and per-tunnel durable agent-state directory in one guarded shell block. ECS/Fargate and Kubernetes output runtime-native task or pod artifacts, including a per-sidecar persistent state mount. Bootstrap API keys are short-lived and should be removed from the runtime after the sidecar logs show a successful tunnel connection.

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
