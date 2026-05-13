# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with per-workspace OAuth setup.

## Features

- `/qurl setup` — Connect qURL to the workspace (admin-only; one-shot OAuth flow against Auth0)
- `/qurl create <url>` — Create a qURL
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

## Architecture

- **Runtime:** AWS Fargate (arm64, distroless container) behind an
  ALB that terminates TLS and routes `/slack/*`, `/oauth/qurl/*`, and `/health`.
- **Auth:** Per-workspace qURL API key, minted via `/qurl setup` →
  `/oauth/qurl/start` → Auth0 → `/oauth/qurl/callback`. Keys are
  field-level encrypted in the `workspace_state` DynamoDB table using
  KMS envelope encryption with `workspace_id` bound as AAD.
- **Endpoints:**
  - `POST /slack/commands` — Slash command handler (ack-then-async)
  - `POST /slack/events` — Event subscriptions (link unfurling planned)
  - `POST /slack/interactions` — Interactive components (planned)
  - `GET /oauth/qurl/start` — Begin OAuth flow (state token required)
  - `GET /oauth/qurl/callback` — Auth0 redirect target; mints + persists key
  - `GET /health` — ALB target-group health probe

## Development

```bash
# Run tests
go test -race -count=1 ./apps/slack/...

# Run locally — requires AWS creds with read/write on the
# workspace_state table and KMS Decrypt/GenerateDataKey on the CMK.
WORKSPACE_STATE_TABLE=workspace-state-dev \
WORKSPACE_STATE_KMS_KEY_ARN=arn:aws:kms:us-east-1:...:key/... \
SLACK_SIGNING_SECRET=... \
QURL_ENDPOINT=https://api.layerv.xyz \
AUTH0_DOMAIN=layerv.us.auth0.com \
AUTH0_CLIENT_ID=... \
AUTH0_CLIENT_SECRET=... \
AUTH0_AUDIENCE=https://api.layerv.xyz \
SLACK_BASE_URL=https://slack-bot.example \
OAUTH_STATE_SECRET=$(openssl rand -hex 32) \
  go run ./apps/slack/cmd/

# Build the production container (linux/arm64 to match Fargate)
docker buildx build --platform linux/arm64 \
  -f apps/slack/Dockerfile -t qurl-bot-slack:dev .
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.xyz`) |
| `WORKSPACE_STATE_TABLE` | Yes | DynamoDB table holding per-workspace API keys (provisioned by `qurl-integrations-infra`) |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | KMS CMK ARN used to envelope-encrypt the workspace API key column |
| `AUTH0_DOMAIN` | OAuth | Auth0 tenant FQDN, e.g. `layerv.us.auth0.com`. Scheme prefix and trailing slash are stripped at config-load. |
| `AUTH0_CLIENT_ID` | OAuth | Auth0 application client_id for the bot |
| `AUTH0_CLIENT_SECRET` | OAuth | Auth0 application client_secret |
| `AUTH0_AUDIENCE` | OAuth | Auth0 audience identifier for the qurl-service API |
| `SLACK_BASE_URL` | OAuth | Public origin of the bot, e.g. `https://slack-bot.example`. Used to compose `redirect_uri` and the `/qurl setup` link. |
| `OAUTH_STATE_SECRET` | OAuth | HMAC-SHA256 key for state-token signing. Must be ≥32 bytes. |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Slack bot is busy` acks; tune down if memory pressure during retry storms is observed. |

`WORKSPACE_STATE_TABLE` + `WORKSPACE_STATE_KMS_KEY_ARN` are
unconditionally required at startup — the bot needs DDB+KMS for
per-workspace key lookups even on `/qurl create`/`/qurl list`.

The `OAuth` group is required only when the bot needs to serve the
`/oauth/qurl/{start,callback}` surface. Boots without these vars still
serve `/slack/*` and `/health`; `/qurl setup` replies "OAuth is not
configured" until the OAuth env vars are populated.
