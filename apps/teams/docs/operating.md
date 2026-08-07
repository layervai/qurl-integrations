# Operating the qURL Secure Access Agent for Teams

This guide is for operators running the Microsoft Teams Secure Access Agent
themselves: endpoints, environment variables, OAuth wiring, and local
development. If you're a Teams user or tenant admin, start with the
[README](../README.md) instead.

## Command dispatch

The Teams bot exposes one inbound message endpoint: `POST /teams/messages`.
Unlike Slack, there are no slash commands or modal callbacks. Teams delivers
Bot Framework activities, the bot normalizes the incoming message text, and
then dispatches the same qURL verbs the Slack bot uses where the platform
allows.

The bot accepts plain verbs (`get $docs`) plus optional textual prefixes
(`qurl get $docs`, `qurl-admin set-alias ...`). In a channel, Teams usually
requires the bot mention at the transport layer, so operators should document
examples as `@qurl ...` even though the command parser itself does not depend
on the literal mention token after normalization.

## Architecture

- **Runtime**: stateless HTTP service, normally deployed behind a
  TLS-terminating load balancer that routes `/teams/messages`,
  `/oauth/qurl/*`, and `/health`.
- **Bot auth**: incoming Bot Framework bearer tokens are validated against
  Microsoft metadata and the request `serviceUrl`. Local test deployments can
  disable this with `QURL_TEAMS_SKIP_BOT_AUTH=true`; production should not.
- **qURL auth**: one qURL API key is stored per Teams tenant as
  `teams:<tenant-id>` in `workspace_state`, encrypted by the shared
  `WORKSPACE_STATE_KMS_KEY_ARN` contract.
- **Tenant policy state**: Teams-specific owner/admin state, channel allow
  lists, aliases, and personal-chat conversation references live in the two
  Teams DynamoDB tables configured by
  `QURL_TEAMS_WORKSPACE_MAPPINGS_TABLE` and
  `QURL_TEAMS_CHANNEL_POLICIES_TABLE`.
- **Personal chat dependency**: Teams uses stored personal conversation
  references for `dm:true` and connector bootstrap-key delivery. Users must
  message the bot once in personal chat before those flows work.

## Endpoints

| Method + path | Purpose |
|---------------|---------|
| `POST /teams/messages` | Bot Framework activity endpoint. Accepts message and conversation-update activities. |
| `GET /oauth/qurl/start` | Start Auth0-backed tenant setup. |
| `GET /oauth/qurl/callback` | Finish Auth0 setup, mint/replay/rotate tenant qURL API keys, and bind the Teams tenant. |
| `GET /health` | Liveness probe. Returns `200 ok`. |

## Environment variables

The table below is derived from `apps/teams/cmd/main.go`,
`apps/teams/internal/oauth/router.go`, `apps/teams/internal/teamsdata/store.go`,
and `shared/auth/ddb_provider.go`.

| Variable | Required | Purpose |
|----------|----------|---------|
| `QURL_ENDPOINT` | Yes | qURL API origin used for resource operations and key provisioning. |
| `TEAMS_APP_ID` or `MICROSOFT_APP_ID` | Yes | Bot Framework app ID used for incoming token validation and outbound replies. |
| `TEAMS_APP_PASSWORD` or `MICROSOFT_APP_PASSWORD` | Yes | Bot Framework app secret/password used for outbound replies. |
| `WORKSPACE_STATE_TABLE` | Yes | Shared workspace-state DynamoDB table storing the per-tenant qURL API key. |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | CMK ARN used to envelope-encrypt the stored qURL API key. |
| `QURL_TEAMS_WORKSPACE_MAPPINGS_TABLE` | Yes | DynamoDB table for tenant owner/admin state plus stored personal conversation references. |
| `QURL_TEAMS_CHANNEL_POLICIES_TABLE` | Yes | DynamoDB table for per-channel allow lists and alias bindings. |
| `TEAMS_BASE_URL` | Required for setup | Bare `https://` origin of this deployment. Must not contain a path. Used to generate `/oauth/qurl/start` and `/oauth/qurl/callback` URLs. |
| `OAUTH_STATE_SECRET` | Required for setup | State-signing secret for Teams setup. Must be at least 32 bytes. |
| `AUTH0_DOMAIN` | Required for setup | Bare Auth0 domain (host only, no scheme/path). |
| `AUTH0_CLIENT_ID` | Required for setup | Auth0 application client ID. |
| `AUTH0_CLIENT_SECRET` | Required for setup | Auth0 application client secret. |
| `AUTH0_AUDIENCE` | Required for setup | Auth0 audience for qURL API access. |
| `AUTH0_EMAIL_CONNECTION` | No | Optional override to force a specific Auth0 connection when `setup <email>` passes a login hint. |
| `QURL_CONNECTOR_IMAGE` | Required in production | Image name used in `protect-connector` install instructions. Must be a specific non-`latest` tag or `image@sha256:<64 lowercase hex>` unless `QURL_CONNECTOR_IMAGE_FALLBACK=dev-sandbox` explicitly opts into the local/sandbox fallback. |
| `QURL_CONNECTOR_IMAGE_FALLBACK` | No | Set `dev-sandbox` to allow an empty `QURL_CONNECTOR_IMAGE` and render the local/sandbox fallback image in install instructions. Leave unset in production. |
| `FEEDBACK_WEBHOOK_URL` | No | HTTPS webhook target for `feedback <message>`. Invalid values disable feedback with a warning. |
| `QURL_TEAMS_SKIP_BOT_AUTH` | No | When true-ish (`1/true/yes/on`), skip incoming Bot Framework bearer-token validation. Local testing only. |

### OAuth enablement contract

Teams setup routes are registered only when every setup dependency above is
present. If any are missing, the bot still starts and serves `/teams/messages`
and `/health`, but `setup` fails closed because `/oauth/qurl/*` is not wired.

## Teams bot registration checklist

The Teams and Azure portal UIs change frequently. Treat this as the stable
contract the repo needs, not as pixel-perfect portal instructions:

- A bot registration that issues an app ID and secret/password.
- Messaging endpoint pointed at
  `https://<TEAMS_BASE_URL host>/teams/messages`.
- Installation scopes that allow both personal chat and channel/team usage.
- A Teams app package that installs the bot into the tenant and channel where
  qURL commands will run.
- Auth0 callback URL pointed at
  `https://<TEAMS_BASE_URL host>/oauth/qurl/callback`.

## Local development

```bash
# Unit tests
go test -race -count=1 ./apps/teams/...

# Run locally (export the required env vars first)
go run ./apps/teams/cmd/

# Build the production-style binary
make build-teams

# Build the container image
docker build -f apps/teams/Dockerfile -t qurl-bot-teams:dev .
```

For quick manual verification in a sandbox, the repo also includes
`scripts/teams-manual-deploy-test-only.sh`.
