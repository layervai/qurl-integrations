# qURL CLI and integrations for private app sharing

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Use [qURL™ by LayerV](https://layerv.ai/qurl/) to share localhost web apps,
private APIs, and files through expiring access links. Share a development
preview, an internal dashboard, or a document with a teammate or AI agent.
This repository contains
LayerV's CLI, Slack and Discord integrations, browser extensions for Gmail,
and Microsoft Teams integration source.

For an app running on localhost, the CLI connects outward to LayerV without
opening an inbound port. Publish the app, create a short-lived access link,
and send that link to its recipient. The app must stay running, and its own
sign-in still applies. Adding a link to an existing remote URL does not
restrict any separate public route to that URL.

qURL uses [OpenNHP](https://github.com/OpenNHP/opennhp) to authorize access to
protected resources. Start with the guide for the task you want to complete:

| Task | Guide |
| --- | --- |
| Share a local web app or API with a temporary link | [qURL CLI quickstart](apps/cli/README.md#publish-localhost-in-60-seconds) |
| Share internal resources from a Slack channel | [qURL for Slack](apps/slack/README.md) |
| Send one-time file or location links in Discord | [qURL Discord bot](apps/discord/README.md) |
| Share files in Gmail through expiring links | [Browser extension for Gmail](apps/chrome-extension/README.md) |
| Build or submit the Microsoft Edge extension | [Edge installation and release guide](apps/edge-extension/README.md) |
| Develop or deploy the Microsoft Teams integration | [Teams integration source and deployment guide](apps/teams/README.md) |
| Serve a private S3 static site through a connector | [Private S3 origin image](origins/s3-static-connector/README.md) |

The CLI guide documents local publishing and sharing. Each integration guide
lists its own setup requirements. Teams deployment is managed outside this
repository; the presence of its source does not establish a public rollout.

[Localhost quickstart](#share-a-localhost-web-app) ·
[File sharing](#share-files-and-documents) · [ngrok comparison](#qurl-and-ngrok-for-localhost-sharing) ·
[Sharing questions](#localhost-and-file-sharing-questions) ·
[Product overview](https://layerv.ai/qurl/) · [Private links](https://qurl.link/)

## Share a localhost web app

For example, let a teammate review a preview running on your laptop without
deploying a staging server. Your app stays on your machine; the CLI runs the
Connector that carries access to it.

1. Install the CLI. On macOS with Homebrew:

   ```bash
   brew install layervai/tap/qurl
   qurl version
   ```

   See the [CLI setup guide](apps/cli/README.md#authentication) for the
   authentication and deployment requirements of your installation, and
   [other installation options](apps/cli/README.md#install) for Linux or Windows.

2. Keep your existing app running on port 3000. To try a simple file-based
   preview, run this from a directory containing only files you intend to share:

   ```bash
   python3 -m http.server 3000 --bind 127.0.0.1
   ```

3. In a second terminal, publish that local HTTP service:

   ```bash
   qurl publish http://127.0.0.1:3000
   ```

4. Replace `REPLACE_WITH_CRID` with the resource identifier printed after the
   route is ready. Create a link with a requested five-minute access window:

   ```bash
   qurl share REPLACE_WITH_CRID --ttl 5m --session-duration 5m
   ```

   Send the returned full qURL link privately. A browser recipient opens the
   link; your app's own login still applies. The service can shorten the
   requested lifetime. Keep the app, publishing host, and background daemon
   available while the recipient uses it.

The CRID identifies the resource; the minted link grants access. CRID-based
link requests also depend on resource privacy and device grants. Read
[private CRIDs](apps/cli/README.md#private-crids) before handing a CRID to
someone else's CLI or agent.

When the preview is over, stop serving it:

```bash
qurl stop REPLACE_WITH_CRID
```

`stop` pauses serving without retiring the resource. See the
[command reference](apps/cli/README.md#commands) for restarting, deleting,
and managing individual links.

## Share files and documents

Choose a file workflow that matches where the file lives and how it should
reach the recipient:

| Use case | Workflow |
| --- | --- |
| Review files in a local preview directory | Serve a dedicated directory with a local HTTP server, publish its root address with the CLI, and share the access link. Recipients can reach the files that server exposes. |
| Send an uploaded document from Gmail | Use the [browser extension](apps/chrome-extension/README.md#quickstart) to upload selected files and insert expiring links into a compose draft. |
| Send separate file links to Discord recipients | Use [`/qurl send`](apps/discord/README.md#commands); the configured bot delivers recipient links by DM. |
| Let an AI agent share a selected PDF or image | Use the [MCP upload tools](https://github.com/layervai/qurl-mcp#upload-tools), with their supported file types, transport modes, and expiry limits. |

A localhost file server and an uploaded file are different resource lifecycles.
Local serving needs the host to remain available. Uploads use their configured
file service. Opening or testing a single-use link can consume it; create a
fresh recipient link when needed. Use each integration's guide for its actual
revocation support and file-size limits.

## qURL and ngrok for localhost sharing

Both tools can connect an app on your machine to remote users without inbound
port forwarding. The sharing workflow and access model are the useful points
to compare:

| Question | qURL local sharing | ngrok's documented localhost workflow |
| --- | --- | --- |
| What do I send? | An expiring access link for a published resource. | A public endpoint URL, which can be secured with Traffic Policy. |
| How is access controlled? | Resource identity, signed access grants, and the resource's link and session rules. The app can keep its own login. | Endpoint policies such as OAuth, OIDC, Basic Auth, and IP restrictions. |
| Does the origin need inbound port forwarding? | No; the Connector connects outbound. | No; the agent establishes an outbound tunnel. |

For private previews and temporary access to an internal app, qURL centers the
workflow on issuing access to a resource. ngrok also documents public endpoint
delivery, request inspection, and replay. Choose based on the access and
testing workflow you need. See [ngrok's localhost guide](https://ngrok.com/use-cases/share-localhost),
[ngrok security](https://ngrok.com/security), and the
[LayerV sharing FAQ](https://layerv.ai/faq/).

## Localhost and file-sharing questions

### Does sharing move my app to LayerV?

No. In the local workflow, the app runs on your host and the CLI's embedded
Connector carries access traffic. A sleeping laptop or stopped app cannot
serve the preview.

### Does a qURL make an existing public URL private?

Creating a qURL adds an access path. Close or restrict any separate public
route if access should be available only through qURL.

### Does the link's expiry end an open session?

Link expiry limits new redemption. An already admitted session has its own
lifetime. The quickstart requests both limits; the service enforces them.

### Is every link single-use, and does it replace application login?

Use limits depend on the link and issuing workflow. A link is an access
credential, so share it privately. It does not replace your app's login or
prove a recipient's identity merely because you sent it to a named person.

## Structure

```
apps/                Per-integration apps (released apps get independent release tracks)
  slack/             Slack Secure Access Agent — /qurl slash commands (Go)
  discord/           Discord app — one-time qURL links for files & locations (Node.js)
  chrome-extension/  Shared Chrome and Edge extension source (MV3)
  edge-extension/    Edge release metadata and store documents
  cli/               CLI — publish, share, and manage qURL resources by CRID (Go)
  teams/             Microsoft Teams Bot and OAuth integration (TypeScript)
origins/             Reusable origin images for qURL Connector-protected resources
  s3-static-connector/  Private S3 static site origin behind qURL Connector
shared/              Shared Go libraries used by the Go apps
  client/            qURL API client
  auth/              API key helpers
  observability/     OpenTelemetry setup
```

## Claude Code and Codex plugins

The qURL plugins live in their own repositories:
[layervai/claude-plugins](https://github.com/layervai/claude-plugins) for Claude
Code and [layervai/codex-plugins](https://github.com/layervai/codex-plugins) for
OpenAI Codex. Each bundles the qURL agent skill; none needs an account.

## SDKs & MCP server (separate repos)

Language SDKs and the qURL MCP server live in standalone repositories:

| Library | Install | Repo |
|---------|---------|------|
| Python SDK | `pip install qurl-python` | [layervai/qurl-python](https://github.com/layervai/qurl-python) |
| TypeScript SDK | `npm install @layervai/qurl` | [layervai/qurl-typescript](https://github.com/layervai/qurl-typescript) |
| Go SDK | `go get github.com/layervai/qurl-go/qurl` | [layervai/qurl-go](https://github.com/layervai/qurl-go) |
| MCP server | `npx @layervai/qurl-mcp` | [layervai/qurl-mcp](https://github.com/layervai/qurl-mcp) |

## Configuration

The Slack, Discord, and CLI apps connect to the qURL API:

- **Endpoint** — the qURL API is `https://api.layerv.ai`, set via `QURL_ENDPOINT`. Required for Slack; the CLI and Discord use it by default.
- **Authentication** — the CLI needs no account: the first command creates a restricted device identity. An existing account API key can still enroll a device once with `qurl login` or `QURL_API_KEY`; qurl does not store the account key. See [apps/cli/README.md](apps/cli/README.md#authentication).

The Chrome and Edge builds use the same extension source and upload to a qURL file server; see the [browser extension README](apps/chrome-extension/README.md) and [Edge release notes](apps/edge-extension/README.md).

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
`shared/` and `apps/teams/` ship nothing, so neither has a track:

- Commits scoped to an app bump only that app: `feat(slack): add thread replies` → `slack-v0.2.0`
- The CLI is the one component tagged **without** its prefix (`v0.2.0`, not `cli-v0.2.0`) so OSS
  GoReleaser can parse the tag — see the header of
  [`.github/workflows/release-please.yml`](.github/workflows/release-please.yml) before "normalizing" it
- Only commits touching an app's directory trigger its release; `shared/` changes ship with each
  app's next release
- Each released app gets its own `CHANGELOG.md` once its first release lands
- CLI release assets are keyless-signed (cosign/Sigstore) and ship per-archive SPDX SBOMs — the
  consumer verification recipe lives in [RELEASING.md](RELEASING.md)

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
as a pass, or makes the contract check conditional. It also records every pull-request workflow's
intended `branches:` filter, so one recorded as deliberately narrow fails the moment it reports a
required context — including the nine aggregates, whose recorded filter is weighed against the
documented contexts rather than only against itself, so narrowing one cannot be laundered by
editing `requiredWorkflowSpecs` to match. This is the paths filter's trap inverted: a
workflow filtered off PRs stacked on a feature branch never registers its checks at all, and
protection guards only `main`, so the stacked PR reads green having run none of them
(#1183, #1185). Deleting the merged base does not recover the run: GitHub retargets the PR onto
`main`, but a base change arrives as the `edited` activity type, which no branch-filtered workflow
here takes, so the retarget re-runs nothing. The PR stalls on the check that never registered until
its next push (#1219). The package's own check — `Workflow Contract` — is unfiltered and reports
on every PR, because a check behind a paths filter cannot police the paths filters (#1081).

## License

[MIT](LICENSE) — Copyright (c) 2025-present LayerV, Inc.
