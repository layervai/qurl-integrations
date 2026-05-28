# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with per-workspace OAuth setup.

## Features

- `/qurl setup` — Connect qURL to the workspace (admin-only; one-shot OAuth flow against Auth0)
- `/qurl get <url>` — Mint a qURL for a URL
- `/qurl get $shortcut` — Mint a qURL for a channel shortcut
- `/qurl set-alias $shortcut <url|resource-id|$tunnel-slug>` — Bind a channel shortcut (admin-only)
- `/qurl unset-alias $shortcut` — Remove a channel shortcut binding (admin-only)
- `/qurl tunnel install` — Guided tunnel sidecar setup with target-environment choices (admin-only; requires `SLACK_BOT_TOKEN` with `views:write`)
- `/qurl tunnel install <slug|$slug> [port:<n>] [alias:$shortcut] [env:<target>] [container:<name>]` — Provision a tunnel from a typed command (admin-only; default local port is 8080)
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

Run `/qurl help` in Slack for the canonical command modifiers enabled
by the current bot deployment.

## Architecture

- **Runtime:** AWS Fargate (arm64, distroless container) behind an
  ALB that terminates TLS and routes `/slack/*`, `/oauth/qurl/*`, and `/health`.
- **Auth:** Per-workspace qURL API key, minted via `/qurl setup` →
  `/oauth/qurl/start` → Auth0 → `/oauth/qurl/callback`. Keys are
  field-level encrypted in the `workspace_state` DynamoDB table using
  KMS envelope encryption with `workspace_id` bound as AAD.
- **Tunnel onboarding:** `/qurl tunnel install` opens a Slack modal when
  `SLACK_BOT_TOKEN` is configured, letting an admin choose the tunnel slug,
  optional channel shortcut, local port, and target environment
  (Docker, Docker Compose, ECS/Fargate, or Kubernetes). `/qurl tunnel install <slug>` (or
  `$slug`) remains available for CLI-style admins and sandbox deployments
  without modal support. Both paths use the
  workspace API key to find-or-create a tunnel resource scoped to the
  connected qURL account, bind `$<slug>` or the `alias:` shortcut override in
  the current Slack channel, and mint a 1-hour `tunnel_bootstrap` API
  key. When `alias:` is omitted, the slug doubles as the channel shortcut.
  The Slack response hides the internal resource id and renders output
  tailored to the selected environment. Docker and Docker Compose receive
  guarded pasteable shell blocks that write `qurl-proxy.yaml`, create a
  bootstrap-key file, create/chown slug-scoped durable agent state, pass
  `QURL_API_KEY_FILE`, and pass `QURL_TUNNEL_SLUG=<slug>` to the client.
  ECS/Fargate and Kubernetes receive the same contract as deployment
  snippets: co-locate the sidecar with the target container, mount durable
  per-instance state at `/var/lib/layerv/agent`, mount or inject the
  bootstrap key through the runtime's secret mechanism, and remove the key
  after the logs show a successful tunnel connection. ECS/Fargate uses the
  client's supported `QURL_API_KEY` fallback because AWS injects task secrets
  as environment variables; Docker, Docker Compose, and Kubernetes prefer
  `QURL_API_KEY_FILE`. Do not share one agent state volume across
  concurrently running sidecars.
- **Endpoints:**
  - `POST /slack/commands` — Slash command handler (ack-then-async)
  - `POST /slack/events` — Event subscriptions (link unfurling planned)
  - `POST /slack/interactions` — Interactive components and modals
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
SLACK_BOT_TOKEN=xoxb-... \
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
| `SLACK_BOT_TOKEN` | No | Slack bot token used for `views.open` so `/qurl tunnel install` can show the guided installer. Requires the Slack app to grant `views:write`. Without it, the typed `/qurl tunnel install <slug>` path still works. |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.xyz`) |
| `WORKSPACE_STATE_TABLE` | Yes | DynamoDB table holding per-workspace API keys (provisioned by `qurl-integrations-infra`) |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | KMS CMK ARN used to envelope-encrypt the workspace API key column |
| `AUTH0_DOMAIN` | OAuth | Auth0 tenant FQDN, e.g. `layerv.us.auth0.com`. Scheme prefix and trailing slash are stripped at config-load. |
| `AUTH0_CLIENT_ID` | OAuth | Auth0 application client_id for the bot |
| `AUTH0_CLIENT_SECRET` | OAuth | Auth0 application client_secret |
| `AUTH0_AUDIENCE` | OAuth | Auth0 audience identifier for the qurl-service API |
| `SLACK_BASE_URL` | OAuth | Public origin of the bot, e.g. `https://slack-bot.example`. Used to compose `redirect_uri` and the `/qurl setup` link. |
| `OAUTH_STATE_SECRET` | OAuth | HMAC-SHA256 key for state-token signing. Must be ≥32 bytes. |
| `QURL_TUNNEL_IMAGE` | No | Docker image reference rendered by `/qurl tunnel install`. Set this to an immutable release tag or digest for production rollout, for example `ghcr.io/layervai/qurl-reverse-tunnel-client@sha256:<digest>`. Empty uses `ghcr.io/layervai/qurl-reverse-tunnel-client:latest` as a dev/sandbox fallback. Values with whitespace or control characters fail startup validation. |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Slack bot is busy` acks; tune down if memory pressure during retry storms is observed. |

`WORKSPACE_STATE_TABLE` + `WORKSPACE_STATE_KMS_KEY_ARN` are
unconditionally required at startup — the bot needs DDB+KMS for
per-workspace key lookups even on `/qurl get`/`/qurl list`.

The `OAuth` group is required only when the bot needs to serve the
`/oauth/qurl/{start,callback}` surface. Boots without these vars still
serve `/slack/*` and `/health`; `/qurl setup` replies "OAuth is not
configured" until the OAuth env vars are populated.
