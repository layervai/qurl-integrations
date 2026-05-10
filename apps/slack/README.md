# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with link unfurling and channel notifications.

## Features

- `/qurl create <url>` — Create a qURL
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

## Architecture

- **Runtime:** AWS Fargate (arm64, distroless container) behind an
  ALB. Always-on; long-running-process needed for the
  `response_url` async-defer pattern Slack publishes for slash
  commands.
- **Auth:** Workspace API key (per-user OAuth planned)
- **Endpoints:**
  - `POST /slack/commands` — Slash command handler
  - `POST /slack/events` — Event subscriptions (link unfurling)
  - `POST /slack/interactions` — Interactive components
  - `GET /health` — Health check (ALB / ECS probe target)

## Development

```bash
# Run tests
go test -count=1 ./apps/slack/...

# Build the container image (canonical CI/prod path):
docker buildx build --platform linux/arm64 \
  -f apps/slack/Dockerfile -t qurl-bot-slack:dev .

# Build a local Go binary (development / debugging):
make build-slack
# binary lands at release/slack/qurl-bot-slack (gitignored)
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `QURL_API_KEY` | Yes | qURL API key for this workspace |
| `QURL_ENDPOINT` | No | qURL API base URL (default: `https://api.layerv.xyz`) |
