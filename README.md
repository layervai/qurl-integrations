# qurl-integrations

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Open-source integrations for [qURL™](https://layerv.ai) — Quantum URLs that make protected resources invisible by default.

qURL is built on [OpenNHP](https://github.com/OpenNHP/opennhp) (Network-infrastructure Hiding Protocol), a cryptography-driven protocol that makes servers, ports, and domains invisible to unauthorized users. A qURL wraps any resource behind a short-lived, policy-bound, cryptographically protected access token. When the token is resolved, an NHP knock grants the caller's IP temporary access — the resource literally does not exist on the network until that moment. Think of it like quantum observation: the resource only becomes visible when an authorized user observes it.

This monorepo contains qURL integrations across several surfaces — a Slack app and a CLI tool (Go), a Discord app (Node.js), and Chrome and Edge extensions for Gmail — plus shared Go libraries. A Microsoft Teams OAuth core is in progress; Zapier is planned.

## Structure

```
apps/                Per-integration apps (released apps get independent release tracks)
  slack/             Slack Secure Access Agent — /qurl slash commands (Go)
  discord/           Discord app — one-time qURL links for files & locations (Node.js)
  chrome-extension/  Chrome extension — Gmail file uploads as expiring qURL links (MV3)
  edge-extension/    Edge extension — Gmail file uploads as expiring qURL links (MV3)
  cli/               CLI — create & manage qURLs from the terminal (Go)
  teams/             Microsoft Teams OAuth security core — no routes/SDK yet (TypeScript)
  zapier/            Zapier integration (planned)
origins/             Reusable origin images for qURL Connector-protected resources
  s3-static-connector/  Private S3 static site origin behind qURL Connector
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

- **Endpoint** — the qURL API is `https://api.layerv.ai`, set via `QURL_ENDPOINT`. Required for Slack; the CLI and Discord use it by default.
- **Authentication** — an API key (`lv_live_…`) in `QURL_API_KEY`.

The Chrome and Edge extensions upload to a qURL file server instead; see their [Chrome README](apps/chrome-extension/README.md) and [Edge README](apps/edge-extension/README.md) for configuration.

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

# Run all checks for the Go apps, shared/, and the repo itself (fmt, vet, lint, test)
make check

# The Node.js suites are opt-in — run the one matching your change
# (make check-node runs all five, but that is five npm installs)
make check-discord

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
Each *released* app has an independent version track. A track is earned by cutting a semver
version stream that something downstream pins to — not by merely publishing an artifact.
`origins/s3-static-connector/` ships a container image but tags it only `:main` and `:<sha>`, and
`shared/`, `apps/teams/`, and `apps/zapier/` ship nothing, so none of them have a track:

- Commits scoped to an app bump only that app: `feat(slack): add thread replies` → `slack-v0.2.0`
- The CLI is the one component tagged **without** its prefix (`v0.2.0`, not `cli-v0.2.0`) so OSS
  GoReleaser can parse the tag — see the header of
  [`.github/workflows/release-please.yml`](.github/workflows/release-please.yml) before "normalizing" it
- Only commits touching an app's directory trigger its release; `shared/` changes ship with each
  app's next release
- Each released app gets its own `CHANGELOG.md` once its first release lands

## CI

Each app's workflow runs on every PR. A `changes` detector job inside it decides whether that
app's quality gates actually execute, and an always-reporting aggregate check — `slack / required`,
`discord / required`, `chrome-extension / required`, `edge-extension / required`,
`teams / required`, `cli / required`, `s3-static-connector / required`, `e2e / required`,
`shared / required` — summarizes the result. Branch protection requires those aggregates, never
the gates themselves. The full required-context set, and the rules for changing it, live in
[CONTRIBUTING.md](CONTRIBUTING.md#merge-result-checks) — keep this list in step with that one.

Path filtering deliberately lives in the detector rather than in `on: paths:`: a workflow skipped
by a trigger-level path filter never reports its checks at all, so a required aggregate would
block every PR that happens not to touch that app. The detector's filter is the source of truth
for which paths need validation, and `shared-test.yml` runs all Go app tests when `shared/`
is modified.

That pattern is itself under test. `internal/ciworkflows` reads every file in
`.github/workflows` and fails when a workflow grows a `required` aggregate with no registered
spec, leaves a quality gate out of `required.needs`, ships a verifier that treats a skipped gate
as a pass, or makes the contract check conditional. Its check — `Workflow Contract` — is
unfiltered and reports on every PR and merge group, because a check behind a paths filter cannot
police the paths filters (#1081).

## License

[MIT](LICENSE) — Copyright (c) 2025-present LayerV, Inc.
