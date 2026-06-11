# Operating the qURL Secure Access Agent for Slack

This guide is for operators **running the Secure Access Agent themselves** — endpoints,
environment variables, Slack app configuration, and local development. If
you're a Slack user or workspace admin, you want the
[README](../README.md) instead.

## Command dispatch

User commands live under `/qurl`; admin commands live under a separate
`/qurl-admin` slash command. Both POST to the same request endpoint and share
the same signature verification — Slack stamps which command was invoked in
the `command` field, and the Secure Access Agent dispatches on it.

**Deploy prerequisite:** `/qurl-admin` must be registered as a slash command
in the Slack app config pointing at the **same request URL** as `/qurl`. The
admin verbs are inert until that registration exists.

Admin enforcement is **in-code** — every admin verb checks the qURL admin set
(`admin_slack_user_ids`) via `requireAdminSync`, so the AdminStore must be
wired. The "admins only" restriction on the `/qurl-admin` registration is a
cosmetic Slack-picker hint, not the enforcement boundary (Slack does not gate
slash-command invocation on workspace-admin role).

`setup` is deliberately **not** an admin verb — it stays on `/qurl` so the
first user to connect an unbound workspace can reach it (qURL is
first-come-claims). The overwrite guard for an already-bound workspace lives
at the OAuth-callback bind layer.

## Architecture

- **Runtime:** stateless HTTP service, shipped as a small container. Sits
  behind a TLS-terminating load balancer that routes `/slack/*`,
  `/oauth/slack/*`, `/oauth/qurl/*`, and `/health`.
- **Auth:** per-workspace qURL API key, established via `/qurl setup <email>`
  → `/oauth/qurl/start` → Auth0 → `/oauth/qurl/callback`. Supplying an email
  address on setup stores it in signed state, sends Auth0 `login_hint`, and
  requires the verified Auth0 email claim to match before any workspace bind
  or key mint. By default the Secure Access Agent does not force an Auth0 `connection`; the
  Auth0 application and tenant-level Actions own the login method and the
  cross-connection uniqueness policy. Prefer passwordless on the existing
  database connection when available, or enforce account-linking /
  duplicate-deny behavior before enabling a separate passwordless `email`
  connection for the same audience. `AUTH0_EMAIL_CONNECTION` is an optional
  recovery override when a deployment must force a specific connection. The
  callback's security gate is the verified email claim, not the connection
  hint by itself. If a workspace already has a qURL API key and qurl-service
  still accepts it, setup reuses that key instead of minting another one.
  Missing or revoked stored keys ask qURL to provision the Slack workspace key;
  if qURL reports that the workspace is already connected but the stored key
  cannot be recovered, setup stops with admin-facing recovery guidance.
  If the callback reports that a qURL key was provisioned but not stored,
  rerun `/qurl setup <email>` for the same workspace and qURL account within
  24 hours. qURL can replay the setup key during that window. After the window
  expires, or if the admin abandons setup, use qURL account/API-key management
  or operator tooling to revoke the unused workspace key before retrying.
  Rerunning setup is intentionally not a healthy-key rotation or qURL-account
  switch command; use the qURL dashboard / API-key management surface or
  operator tooling for rotation and admin hand-off. Keys are field-level
  encrypted at rest using KMS envelope encryption, with `workspace_id` bound as
  AAD.
- **Slack app install:** customer workspaces install qURL through
  `/oauth/slack/install`, which redirects to Slack OAuth with the bot scopes
  needed by the slash command and modal surfaces. The callback stores Slack's
  workspace bot token using the same KMS envelope-encryption posture as qURL
  API keys. `SLACK_BOT_TOKEN` is only a legacy single-workspace fallback;
  customers do not manually provide bot tokens, and production guided setup
  should use the per-workspace token captured by Slack install OAuth.
  Enterprise Grid org-level installs are also supported: the enterprise-scoped
  bot token is stored under the Slack `enterprise_id`, while qURL API keys and
  admin state remain scoped to each invoking workspace's `team_id`.
- **Connector onboarding:** `/qurl-admin protect-connector` provisions a qURL
  Connector sidecar.
  - **Entry points** — a guided modal (opens with the bot token for the
    invoking workspace; the admin chooses the qURL Connector ID, optional
    channel alias, local port, and target environment: Docker, Docker Compose,
    ECS/Fargate, or Kubernetes), or the typed
    `/qurl-admin protect-connector <id>` (or `$id`) for CLI-style admins.
  - **Backend work (both paths)** — use the workspace API key to
    find-or-create a qURL Connector resource scoped to the connected qURL
    account, bind `$<id>` or the `alias:` override in the current Slack
    channel, and mint a one-hour bootstrap API key. When `alias:` is omitted,
    the ID doubles as the channel alias.
  - **Idempotency** — retrying the install within the modal's 25-minute
    validity window reuses the same bootstrap-key idempotency bucket. Retrying
    after that window can mint a new key, so operators should run the newest
    Slack install block and discard older bootstrap-key messages.
  - **Output** — hides the internal resource id and is tailored to the selected
    environment:
    - **Docker / Docker Compose** — guarded pasteable shell blocks that write
      `qurl-proxy.yaml`, create a bootstrap-key file, create/chown
      per-connector durable agent state, pass `QURL_API_KEY_FILE`, and pass
      `QURL_CONNECTOR_ID=<id>` to the client.
    - **ECS/Fargate / Kubernetes** — the same contract as deployment snippets:
      co-locate the sidecar with the target container, mount durable
      per-instance state at `/var/lib/layerv/agent`, mount or inject the
      bootstrap key through the runtime's secret mechanism, and remove the key
      after the logs show a successful connection.
  - **Key delivery** — ECS/Fargate uses the client's supported `QURL_API_KEY`
    fallback because AWS injects task secrets as environment variables; Docker,
    Docker Compose, and Kubernetes prefer `QURL_API_KEY_FILE`.
  - **Constraint** — do not share one agent state volume across concurrently
    running sidecars.

## Endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /slack/commands` | Slash command handler (ack-then-async) |
| `POST /slack/events` | Event subscriptions (link unfurling planned) |
| `POST /slack/interactions` | Interactive components and modals |
| `GET /oauth/slack/install` | Begin Slack app install; redirects to Slack OAuth |
| `GET /oauth/slack/callback` | Slack OAuth redirect target; encrypts + persists workspace bot token |
| `GET /oauth/qurl/start` | Begin OAuth flow (state token required) |
| `GET /oauth/qurl/callback` | Auth0 redirect target; provisions + persists key |
| `GET /health` | Load-balancer health probe |

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
QURL_ENDPOINT=https://api.layerv.ai \
AUTH0_DOMAIN=layerv.us.auth0.com \
AUTH0_CLIENT_ID=... \
AUTH0_CLIENT_SECRET=... \
AUTH0_AUDIENCE=https://api.layerv.ai \
SLACK_BASE_URL=https://slack-bot.example \
OAUTH_STATE_SECRET=$(openssl rand -hex 32) \
  go run ./apps/slack/cmd/

# Build the production container (linux/arm64 to match the deploy target)
docker buildx build --platform linux/arm64 \
  -f apps/slack/Dockerfile -t qurl-bot-slack:dev .
```

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `SLACK_CLIENT_ID` | Slack install | Slack app client ID used by `/oauth/slack/install`. Required for customer installs that capture per-workspace bot tokens. |
| `SLACK_CLIENT_SECRET` | Slack install | Slack app client secret used by `/oauth/slack/callback` to exchange Slack's OAuth code. |
| `SLACK_INSTALL_STATE_SECRET` | Slack install | HMAC-SHA256 key for Slack install state signing. Must be ≥32 bytes. Use a distinct production secret from `OAUTH_STATE_SECRET`; the fallback is only for local/dev compatibility. |
| `SLACK_BOT_SCOPES` | No | Comma/space-separated bot scopes requested by `/oauth/slack/install`. Empty defaults to `commands` (the captured token is used only for `views.open`, which requires no scope); any override must still include `commands`. |
| `SLACK_BOT_TOKEN` | Legacy | Single-workspace fallback token for `views.open` when a workspace has not yet completed Slack install OAuth. Accepts `xoxb-` and `xoxe.xoxb-` token shapes. Production multi-customer installs should not depend on this fallback. |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.ai`) |
| `WORKSPACE_STATE_TABLE` | Yes | DynamoDB table holding per-workspace API keys (provisioned by `qurl-integrations-infra`) |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | KMS CMK ARN used to envelope-encrypt workspace API keys and Slack bot tokens |
| `AUTH0_DOMAIN` | OAuth | Auth0 tenant FQDN, e.g. `layerv.us.auth0.com`. Scheme prefix and trailing slash are stripped at config-load. |
| `AUTH0_CLIENT_ID` | OAuth | Auth0 application client_id for the Secure Access Agent |
| `AUTH0_CLIENT_SECRET` | OAuth | Auth0 application client_secret |
| `AUTH0_AUDIENCE` | OAuth | Auth0 audience identifier for the qurl-service API |
| `AUTH0_EMAIL_CONNECTION` | No | Optional Auth0 connection name to force during `/qurl setup <email>` (for example `Username-Password-Authentication`). Empty sends no `connection` hint and lets the Auth0 application choose from its enabled connections. |
| `SLACK_BASE_URL` | OAuth/Slack install | Public origin of the Secure Access Agent, e.g. `https://slack-bot.example`. Used to compose Slack install, Slack callback, Auth0 callback, and `/qurl setup <email>` URLs. |
| `OAUTH_STATE_SECRET` | OAuth | HMAC-SHA256 key for state-token signing. Must be ≥32 bytes. |
| `QURL_CONNECTOR_IMAGE` | No | Container image reference rendered by `/qurl-admin protect-connector`. Set this to an immutable release tag or digest for production rollout, for example `ghcr.io/layervai/qurl-connector@sha256:<digest>`; pin **v0.3.0 or newer**, since the rendered snippets emit the v0.3.0 client contract (route `id` / `QURL_CONNECTOR_ID`) that older sidecar clients won't read. Empty uses `ghcr.io/layervai/qurl-connector:latest` as a dev fallback. Values with whitespace or control characters fail startup validation. |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Secure Access Agent is busy` acks; tune down if memory pressure during retry storms is observed. |

`WORKSPACE_STATE_TABLE` + `WORKSPACE_STATE_KMS_KEY_ARN` are unconditionally
required at startup — the Secure Access Agent needs DynamoDB + KMS for per-workspace key
lookups even on `/qurl get` / `/qurl list`.

The `Slack install` group is required for low-friction customer onboarding.
Without it, a deployment can still use a manually supplied `SLACK_BOT_TOKEN`
fallback, but customers cannot self-install the Secure Access Agent.

The `OAuth` group is required only to serve the
`/oauth/qurl/{start,callback}` surface. Without it the Secure Access Agent still serves
`/slack/*` and `/health`; `/qurl setup <email>` replies "OAuth is not
configured" until the OAuth env vars are set.

## Slack app configuration

For customer Slack installs, configure the Slack app with:

- OAuth redirect URL: `https://<SLACK_BASE_URL host>/oauth/slack/callback`
- Customer install link: `https://<SLACK_BASE_URL host>/oauth/slack/install`
- Slash command request URL: `https://<SLACK_BASE_URL host>/slack/commands`
- Interactivity request URL: `https://<SLACK_BASE_URL host>/slack/interactions`
- Bot scopes: `commands` (the captured token is used only for `views.open`,
  which requires no scope of its own)
- Installation mode: workspace-level installs or Enterprise Grid org-level
  installs. Org-level bot tokens are stored under Slack `enterprise_id`; qURL
  workspace setup and admin checks still use workspace `team_id`.
- Token posture: non-rotating bot tokens. The validator accepts `xoxe.xoxb-`
  shapes defensively, but qURL does not request or persist Slack refresh
  tokens yet, so rotation-enabled apps need refresh support before production
  use.

With per-workspace token storage in place, existing customer workspaces must
reinstall or reauthorize the Slack app so Slack issues a per-workspace bot
token. New installs through `/oauth/slack/install` store that token
automatically, and guided `/qurl-admin protect-connector` uses it for
`views.open`. If Slack tells a customer guided connector setup needs the
latest qURL Slack app install, send them through this reinstall link.
