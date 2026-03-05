# QURL Slack Integration

Slack bot for creating and managing QURLs via slash commands, with link unfurling and channel notifications.

## Features

- `/qurl create <url>` — Create a QURL
- `/qurl list` — List recent QURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on QURL events (planned)

## Architecture

- **Runtime:** AWS Lambda (arm64) behind API Gateway
- **Auth:** Workspace API key (per-user OAuth planned)
- **Endpoints:**
  - `POST /slack/commands` — Slash command handler
  - `POST /slack/events` — Event subscriptions (link unfurling)
  - `POST /slack/interactions` — Interactive components
  - `GET /health` — Health check

## Development

```bash
# Run tests
go test -count=1 ./apps/slack/...

# Build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bootstrap ./apps/slack/cmd/
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `QURL_API_KEY` | Yes | QURL API key for this workspace |
| `QURL_ENDPOINT` | No | QURL API base URL (default: `https://api.layerv.xyz`) |
