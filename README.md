# qurl-integrations

QURL integrations monorepo — Slack, Teams, Discord, CLI, Zapier, and more.

## Structure

```
apps/           Per-integration applications (independent release tracks)
  slack/        Slack bot — slash commands, link unfurling, notifications
  teams/        Microsoft Teams (planned)
  discord/      Discord bot (planned)
  cli/          CLI tool (planned)
  zapier/       Zapier integration (planned)
shared/         Shared libraries used by all integrations
  client/       QURL API client
  auth/         Auth0 M2M + API key helpers
  events/       Webhook event parsing
  formatting/   Chat message templates
  observability/ OpenTelemetry setup
```

## Development

```bash
# Run all tests
go test ./...

# Run tests for a specific app
go test ./apps/slack/...

# Build Slack Lambda
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bootstrap ./apps/slack/cmd/
```

## Releases

This repo uses [Release Please](https://github.com/googleapis/release-please) in monorepo mode.
Each app has an independent version track:

- Commits scoped to an app bump only that app: `feat(slack): add thread replies` → `slack/v0.2.0`
- Changes to `shared/` bump all apps
- Each app has its own `CHANGELOG.md`

## CI

Each app has a path-filtered workflow that only runs when its code (or shared code) changes.
A `shared-test.yml` workflow runs all app tests when `shared/` is modified.
