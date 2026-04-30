# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with link unfurling and channel notifications.

## Features

- `/qurl create <url>` — Create a qURL
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

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
| `QURL_API_KEY` | Yes | qURL API key for this workspace |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.xyz`) |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Slack bot is busy` acks; tune down if memory pressure during retry storms is observed. |
