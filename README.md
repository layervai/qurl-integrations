# qurl-integrations

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Open-source integrations for [QURL](https://layerv.ai) — a URL shortening and management API. This monorepo contains platform integrations (Slack, Teams, Discord), a CLI tool, SDKs, and shared libraries.

## Structure

```
apps/           Per-integration applications (independent release tracks)
  slack/        Slack bot — slash commands, link unfurling, notifications
  cli/          CLI tool for QURL
  sdk-python/   Python SDK
  teams/        Microsoft Teams (planned)
  discord/      Discord bot (planned)
  zapier/       Zapier integration (planned)
shared/         Shared libraries used by all integrations
  client/       QURL API client
  auth/         API key helpers
  events/       Webhook event parsing
  formatting/   Chat message templates
  observability/ OpenTelemetry setup
```

## Configuration

All integrations connect to the QURL API. The endpoint is configurable via the `QURL_ENDPOINT` environment variable (defaults to `https://api.layerv.xyz`). Authentication is handled via API keys set in the `QURL_API_KEY` environment variable.

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
