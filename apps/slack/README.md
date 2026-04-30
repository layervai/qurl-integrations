# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with link unfurling and channel notifications.

## Features

- `/qurl create <url>` — Create a qURL
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

## Architecture

- **Runtime:** AWS Fargate (arm64, distroless container) behind an
  ALB that terminates TLS and routes `/slack/*` + `/health`.
- **Auth:** Workspace API key (per-user OAuth planned, Phase 4).
- **Endpoints:**
  - `POST /slack/commands` — Slash command handler (ack-then-async)
  - `POST /slack/events` — Event subscriptions (link unfurling planned)
  - `POST /slack/interactions` — Interactive components (planned)
  - `GET /health` — ALB target-group health probe

## Development

```bash
# Run tests
go test -race -count=1 ./apps/slack/...

# Run locally (uses host networking; no AWS dependencies)
QURL_ENDPOINT=https://api.layerv.xyz \
SLACK_SIGNING_SECRET=... \
QURL_API_KEY=... \
  go run ./apps/slack/cmd/

# Build the production container (linux/arm64 to match Fargate)
docker buildx build --platform linux/arm64 \
  -f apps/slack/Dockerfile -t qurl-bot-slack:dev .
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `QURL_API_KEY` | Yes | qURL API key for this workspace |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.xyz`) |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Slack bot is busy` acks; tune down if memory pressure during retry storms is observed. |
