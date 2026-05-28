# qURL Slack Integration

Slack bot for creating and managing qURLs via slash commands, with per-workspace OAuth setup.

Customer onboarding is install-first:

1. Install the qURL Slack app from `/oauth/slack/install`.
2. Run `/qurl setup` in Slack.
3. Use `/qurl tunnel install` or `/qurl get`.

## Features

- `/qurl setup` — Connect qURL to the workspace (admin-only; one-shot OAuth flow against Auth0)
- `/qurl get <url>` — Mint a qURL for a URL
- `/qurl get $shortcut` — Mint a qURL for a channel shortcut
- `/qurl set-alias $shortcut <url|resource-id|$tunnel-slug>` — Bind a channel shortcut (admin-only)
- `/qurl unset-alias $shortcut` — Remove a channel shortcut binding (admin-only)
- `/qurl tunnel install` — Guided tunnel sidecar setup with target-environment choices (admin-only; uses the workspace bot token stored during Slack app install with `views:write`)
- `/qurl tunnel install <slug|$slug> [port:<n>] [alias:$shortcut] [env:<target>] [container:<name>]` — Provision a tunnel from a typed command (admin-only; default local port is 8080)
- `/qurl list` — List recent qURLs
- Link unfurling for `qurl.link` URLs (planned)
- Channel notifications on qURL events (planned)

Run `/qurl help` in Slack for the canonical command modifiers enabled
by the current bot deployment.

## Architecture

- **Runtime:** AWS Fargate (arm64, distroless container) behind an
  ALB that terminates TLS and routes `/slack/*`, `/oauth/slack/*`,
  `/oauth/qurl/*`, and `/health`.
- **Auth:** Per-workspace qURL API key, minted via `/qurl setup` →
  `/oauth/qurl/start` → Auth0 → `/oauth/qurl/callback`. Keys are
  field-level encrypted in the `workspace_state` DynamoDB table using
  KMS envelope encryption with `workspace_id` bound as AAD.
- **Slack app install:** Customer workspaces install qURL through
  `/oauth/slack/install`, which redirects to Slack OAuth with the bot scopes
  needed by the slash command and modal surfaces. The callback stores Slack's
  workspace bot token in `workspace_state` using the same KMS envelope
  encryption posture as qURL API keys. `SLACK_BOT_TOKEN` is only a legacy
  single-workspace fallback; customers do not manually provide bot tokens, and
  production guided setup should use the per-workspace token captured by Slack
  install OAuth. Org-level Enterprise Grid installs are not supported in this
  flow; install qURL to each workspace that should use guided tunnel setup.
- **Tunnel onboarding:** `/qurl tunnel install` opens a Slack modal with the
  bot token for the invoking workspace, letting an admin choose the tunnel
  slug, optional channel shortcut, local port, and target environment
  (Docker, Docker Compose, ECS/Fargate, or Kubernetes). `/qurl tunnel install <slug>` (or
  `$slug`) remains available for CLI-style admins. Both paths use the
  workspace API key to find-or-create a tunnel resource scoped to the
  connected qURL account, bind `$<slug>` or the `alias:` shortcut override in
  the current Slack channel, and mint a 1-hour `tunnel_bootstrap` API
  key. When `alias:` is omitted, the slug doubles as the channel shortcut.
  Retrying the install within the modal's 25-minute validity window reuses
  the same bootstrap-key idempotency bucket. Retrying after that window can
  mint a new key, so operators should run the newest Slack install block and
  discard older bootstrap-key messages.
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
  - `GET /oauth/slack/install` — Begin Slack app install; redirects to Slack OAuth
  - `GET /oauth/slack/callback` — Slack OAuth redirect target; encrypts + persists workspace bot token
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
SLACK_CLIENT_ID=... \
SLACK_CLIENT_SECRET=... \
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
| `SLACK_CLIENT_ID` | Slack install | Slack app client ID used by `/oauth/slack/install`. Required for customer installs that capture per-workspace bot tokens. |
| `SLACK_CLIENT_SECRET` | Slack install | Slack app client secret used by `/oauth/slack/callback` to exchange Slack's OAuth code. |
| `SLACK_INSTALL_STATE_SECRET` | Slack install | HMAC-SHA256 key for Slack install state signing. Must be ≥32 bytes. Use a distinct production secret from `OAUTH_STATE_SECRET`; the fallback is only for local/dev compatibility. |
| `SLACK_BOT_SCOPES` | No | Comma/space-separated bot scopes requested by `/oauth/slack/install`. Empty defaults to `commands,views:write`; any override must still include both required scopes. |
| `SLACK_BOT_TOKEN` | Legacy | Single-workspace fallback token for `views.open` when a workspace has not yet completed Slack install OAuth. Accepts `xoxb-` and `xoxe.xoxb-` token shapes. Production multi-customer installs should not depend on this fallback. |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.xyz`) |
| `WORKSPACE_STATE_TABLE` | Yes | DynamoDB table holding per-workspace API keys (provisioned by `qurl-integrations-infra`) |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | KMS CMK ARN used to envelope-encrypt workspace API keys and Slack bot tokens |
| `AUTH0_DOMAIN` | OAuth | Auth0 tenant FQDN, e.g. `layerv.us.auth0.com`. Scheme prefix and trailing slash are stripped at config-load. |
| `AUTH0_CLIENT_ID` | OAuth | Auth0 application client_id for the bot |
| `AUTH0_CLIENT_SECRET` | OAuth | Auth0 application client_secret |
| `AUTH0_AUDIENCE` | OAuth | Auth0 audience identifier for the qurl-service API |
| `SLACK_BASE_URL` | OAuth/Slack install | Public origin of the bot, e.g. `https://slack-bot.example`. Used to compose Slack install, Slack callback, Auth0 callback, and `/qurl setup` URLs. |
| `OAUTH_STATE_SECRET` | OAuth | HMAC-SHA256 key for state-token signing. Must be ≥32 bytes. |
| `QURL_TUNNEL_IMAGE` | No | Docker image reference rendered by `/qurl tunnel install`. Set this to an immutable release tag or digest for production rollout, for example `ghcr.io/layervai/qurl-reverse-tunnel-client@sha256:<digest>`. Empty uses `ghcr.io/layervai/qurl-reverse-tunnel-client:latest` as a dev/sandbox fallback. Values with whitespace or control characters fail startup validation. |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Slack bot is busy` acks; tune down if memory pressure during retry storms is observed. |

`WORKSPACE_STATE_TABLE` + `WORKSPACE_STATE_KMS_KEY_ARN` are
unconditionally required at startup — the bot needs DDB+KMS for
per-workspace key lookups even on `/qurl get`/`/qurl list`.

The `Slack install` group is required for low-friction customer onboarding.
Without it, a deployment can still use a manually supplied `SLACK_BOT_TOKEN`
fallback, but customers cannot self-install the bot.

The `OAuth` group is required only when the bot needs to serve the
`/oauth/qurl/{start,callback}` surface. Boots without these vars still
serve `/slack/*` and `/health`; `/qurl setup` replies "OAuth is not
configured" until the OAuth env vars are populated.

For customer Slack installs, configure the Slack app with:

- OAuth redirect URL: `https://<SLACK_BASE_URL host>/oauth/slack/callback`
- Customer install link: `https://<SLACK_BASE_URL host>/oauth/slack/install`
- Slash command request URL: `https://<SLACK_BASE_URL host>/slack/commands`
- Interactivity request URL: `https://<SLACK_BASE_URL host>/slack/interactions`
- Bot scopes: at least `commands` and `views:write`
- Installation mode: workspace-level installs; org-level Enterprise Grid
  installs are rejected until enterprise-scoped tokens are supported
- Token posture: non-rotating bot tokens. The validator accepts `xoxe.xoxb-`
  shapes defensively, but qURL does not request or persist Slack refresh tokens
  yet, so rotation-enabled apps need refresh support before production use.

After adding `views:write` or moving to per-workspace token storage, existing
customer workspaces must reinstall or reauthorize the Slack app so Slack issues
a bot token with the new scope. New installs through `/oauth/slack/install`
store that token automatically, and guided `/qurl tunnel install` will use it
for `views.open`. If Slack tells a customer guided tunnel setup needs the latest
qURL Slack app install, send them through this reinstall link and confirm the app
grants `views:write`.
