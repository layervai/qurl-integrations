# qURL Teams integration

This package is the deployable, dependency-injected TypeScript implementation
for the qURL™ Microsoft Teams integration. It implements opaque one-shot state,
PKCE S256, OIDC nonce binding, a hardened confidential-client token exchange,
ID-token verification, and a production provider-binding adapter.

The Bot implementation is exposed from `src/bot.ts`, `src/qurl-client.ts`,
and the official `@microsoft/teams.apps` adapter in `src/server.ts` and
`src/teams-sdk.ts`. `src/setup-link.ts` is the
boundary to the existing OAuth core: it creates links into the local start
route and reuses the current state manager and confidential client without
duplicating OAuth callbacks or token verification. `src/server.ts` provides
the qURL HTTP entrypoint, and `src/teams-data.ts` contains DynamoDB
document-client adapters for the normalized principals, channel policies,
personal conversations, and tenant credentials tables. The official Teams SDK
owns inbound Activity handling, Bot Framework token validation, and outbound
Connector calls. Its OAuth/Signin features are intentionally not enabled.

## Tenant trust boundary

The Teams SDK must remain the only public ingress for `/api/messages`: it
validates the Bot Framework token before the qURL bot receives an Activity.
The SDK handler seam used by this package does not expose the validated token's
`tid` claim, so tenant scoping intentionally relies on the tenant identifiers
delivered in that authenticated Activity. `deriveScope` rejects contradictory
`channelData.tenant.id` and `conversation.tenantId` values, but cannot perform
a cryptographic claim cross-check until the upstream SDK exposes that claim.
Do not add a route that invokes the bot directly with caller-supplied Activity
objects; doing so would violate this tenant-isolation trust boundary.

### Removing channel visibility

`unset-alias` removes the alias row only; the resource stays exposed in that
channel, so `list` still shows it and `get $<resource-id>` still mints links
there. That is deliberate — the alias is a shortcut, not the grant. Today the
only way to remove a resource's channel visibility is the tenant-wide `revoke`.
`TeamsDataStore.purgeResourceFromScope` exists for a future `unprotect`/`hide`
verb and is currently exercised only by tests.

### Minted qURLs in channel history

`get` replies in place by default, so the minted one-time link lands in channel
history where any member — and any export/eDiscovery path — can consume it
inside its window before the requester does. `dm:true` sends it privately
instead. This is a deliberate difference from the setup link, which is *never*
posted in a channel: a setup link binds the tenant account, a minted qURL is
scoped to one already-protected resource that everyone in the channel can mint
for themselves anyway. Revisit if channel-scoped exposure stops implying that.

### Setup-link handling and the forged-start residual

The setup URL carries the opaque one-shot state handle, so `qurl setup` replies
in the originating conversation with a confirmation only and sends the link
itself to the requester's personal chat. Never change that to an in-place reply:
in a shared channel it would put a bearer capability into persistent history and
into every channel-export path.

`/oauth/qurl/start` necessarily accepts a `state` handle from anyone and issues
the double-submit cookie itself, so the cookie cannot defend against a *forged
start*. Residual risk: an attacker with their own Teams tenant mints a setup
link for a victim's email and gets the victim to open it; if the victim has a
live Auth0 session for that address and clicks through consent, the attacker's
tenant binds to the victim's qURL account. The chain requires a deliberate
consent click, so it is not a drive-by, but it is not fully closed either.
Closing it needs an interstitial on `/oauth/qurl/start` naming the Teams tenant
and target email before redirecting — tracked as follow-up, not shipped here.

The Bot supports setup, resource listing/get, URL and connector protection,
aliases, display names, admin membership, uninstall, feedback, and private
Teams delivery. Policies and principals are stored as normalized rows matching
the `qurl-teams-ddb` Terraform module; OAuth state uses `state_handle_hash`
and numeric `expires_at`. Tenant administrators can manage tenant-wide resource
metadata and revocation from any channel; channel aliases and visibility remain
scope-specific.

## Connector flow

As in Slack, an administrator protects a Connector from a team channel; the bot
sends install instructions and a separate one-time enrollment token to their
personal chat. Open that personal chat first. For example:

```text
qurl protect-connector internal-web env:compose service:web port:8080 alias:$docs
qurl get $docs dm:true
```

Supported installs are Docker, Docker Compose, ECS/Fargate, and Kubernetes.
The flow resolves the account owner, creates or reuses the Connector resource,
restarts sharing, and issues a credential restricted to enrollment of that
Connector. Failed instruction/token delivery revokes the credential and restores
previously disabled sharing. Install snippets persist the daemon identity so
warm restarts do not require another token. Re-running protection supplies new
bootstrap instructions; `list`, `aliases`, and `get` use the channel exposure,
and administrators can `revoke $docs` to revoke the resource tenant-wide.

The install must run alongside the HTTP service: `service:` identifies the
Docker container or Compose service, while ECS/Kubernetes share the task/Pod
network. `port:` is that service's local HTTP port. Verify enrollment and a
minted link in the sandbox before production rollout.

## App package (sideloading)

`manifest/` holds the Teams app manifest template, the two required icons, and
a deterministic builder. The template carries placeholders; an operator builds
the package manually using the target environment's bot app ID and hostname
from the private infrastructure configuration. The deployment workflow does
not build or upload the Teams app package.

```bash
npm run manifest -- --env sandbox \
  --app-id <bot-application-client-id> \
  --domain <bot-hostname>
# -> manifest/dist/qurl-teams-sandbox.zip  (+ sha256)
```

Use `--env production` for the production package. Use separate bot registrations
for sandbox and production: each registration has one messaging endpoint.
`--app-id` must match the deployed `TEAMS_APP_ID`, and `--domain` must match the
host in `TEAMS_BASE_URL`. Build from the same application commit as the runtime.
Upload the zip in Teams (Apps -> Manage your apps -> Upload a custom app) or
import it in the Developer Portal. Rebuilds are byte-identical for identical inputs.

The manifest uses supported schema **1.28**; newer versions are unnecessary for
these capabilities. See [Microsoft's GA schema list](https://learn.microsoft.com/en-us/microsoftteams/platform/resources/schema/manifest-schema).
Command menus are scoped to the implemented behavior: resource operations run
in team channels; personal chat supports setup, administration, and private delivery.

**Prerequisites:** a Microsoft 365 tenant that permits custom-app upload, a bot
registration with the Teams channel enabled and messaging endpoint
`https://<domain>/api/messages`, and the six required `/qurl-bot-teams/*` SSM
parameters seeded in the target environment using the infrastructure rollout order.
The registration can be [Teams-managed](https://microsoft.github.io/teams-sdk/cli/concepts/bot-locations/)
without an Azure subscription. Our Auth0 flow runs in the AWS-hosted application;
Teams-managed OAuth connections and Teams SSO are not used. The manifest therefore
has no `webApplicationInfo` or Application ID URI requirement. Entra app registration
alone does not configure the bot's messaging endpoint or install the Teams app.

## Development

```bash
npm ci
npm run typecheck
npm run lint
npm test
npm run build
```

Node.js 22 is pinned in `.nvmrc`, matching the repository's shipped Discord
runtime convention.

The npm overrides in `package.json` pin `fast-xml-parser`, `brace-expansion`,
and `nanoid` to audited versions. Review these overrides when upgrading
dependencies, especially the `nanoid` major-version constraint, so security
fixes do not become stale or conflict with a future transitive requirement.

## Configuration

Every variable below is read in `src/server.ts`. The required ones are read
through a helper that throws `<NAME> is required` on an empty or missing value,
so the process fails at startup rather than mid-request. Provisioning the
backing AWS resources (tables and KMS keys) lives in the
`qurl-integrations-infra` repository; Microsoft registration is an operator task, and this table is the contract the process
itself enforces.

| Variable | Required | Notes |
|---|---|---|
| `TEAMS_BASE_URL` | yes | Public HTTPS origin serving the OAuth routes. The callback is derived as `<origin>/oauth/qurl/callback`. Origin only — no path, query, fragment, or credentials. |
| `QURL_ENDPOINT` | yes | qURL API HTTPS origin, same origin-only rule. |
| `AWS_REGION` | yes | Region for the DynamoDB and KMS clients. |
| `TEAMS_APP_ID` | yes | Bot Framework app (client) id. |
| `TEAMS_APP_PASSWORD` | yes | Bot Framework client secret. |
| `TEAMS_SERVICE_URL` | no | Pins the outbound Bot Framework service URL. Validated against the trusted-host allowlist in `src/teams-sdk.ts`; unset lets the SDK use the inbound Activity's own service URL. |
| `QURL_IMAGE` | yes | Released qURL CLI image (`ghcr.io/layervai/qurl@sha256:...`) rendered into Connector installs, which run `qurl daemon run`. The retired standalone `qurl-connector` image is rejected. Validated by `validateTunnelImageRef`. |
| `QURL_CONNECTOR_HUB_HOST` | no | Canonical LayerV-owned NHP Hub hostname, forwarded to the Connector environment. Set all three `QURL_CONNECTOR_HUB_*` values together; omission uses the CLI image’s embedded production pin. A different sandbox Hub or an image without that pin requires the triple. |
| `QURL_CONNECTOR_HUB_PORT` | no | NHP Hub UDP port, exactly `443`. |
| `QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64` | no | Base64 NHP Hub server public key pinned by rendered installs. |
| `QURL_TEAMS_TENANT_PRINCIPALS_TABLE` | yes | Owner and admin rows. |
| `QURL_TEAMS_CHANNEL_POLICIES_TABLE` | yes | Channel alias and resource-visibility rows. Must carry the `resource_scopes` GSI (`tenant_resource_key` / `scope_item_type_key`, KEYS_ONLY) — `revoke` queries it directly, so a table without it fails at revoke time rather than at startup. |
| `QURL_TEAMS_PERSONAL_CONVERSATIONS_TABLE` | yes | Personal-chat references used by `dm:true` and connector bootstrap delivery. |
| `QURL_TEAMS_TENANT_CREDENTIALS_TABLE` | yes | Encrypted tenant qURL API keys. Keyed by `tenant_id`, not `teams_tenant_id` — see the note in `src/teams-data.ts`. |
| `QURL_TEAMS_TENANT_CREDENTIALS_KMS_KEY_ARN` | yes | KMS key for the tenant credential envelope. The encryption context binds each ciphertext to its tenant, so this key must stay stable across deploys or stored credentials become undecryptable. |
| `OAUTH_STATE_TABLE` | yes | One-shot OAuth state. Rows carry a numeric `expires_at` in epoch seconds; expiry is enforced in the consume condition, so a table TTL on that attribute is cleanup, not the security control. |
| `AUTH0_DOMAIN` | yes | OIDC issuer. Compared as an exact string against the ID token's `iss`, including the root slash. |
| `AUTH0_CLIENT_ID` | yes | Confidential client id; also the expected ID-token audience. |
| `AUTH0_CLIENT_SECRET` | yes | Confidential client secret. |
| `AUTH0_CLIENT_SECRET_FALLBACK` | no | Second secret accepted during a rotation window. Remove it once the primary is cut over. |
| `AUTH0_AUDIENCE` | yes | Access-token audience requested for the qURL API. |
| `HOST` | no | Listen address, default `127.0.0.1`. See the deployment note below. |
| `PORT` | no | Listen port, default `3000`. Rejected unless an integer in 1-65535. |

### Known limitations

- The OAuth routes (`/oauth/qurl/start`, `/oauth/qurl/callback`) carry no
  application-level rate limit. They are unauthenticated public entrypoints and
  each request performs one DynamoDB operation, so the ingress in front of this
  service is expected to provide that limit. The `qurl-teams-ddb` module already
  provisions an `operation-receipts` table with a `receipt_rate_key` sort key
  and TTL, which is where an application-level limiter would keep its counters
  if the ingress-level control turns out not to be enough.

## Deployment

The production entrypoint listens on `127.0.0.1:3000` by default; `HOST` and
`PORT` can override those values. Container deployments that receive traffic
from a sidecar or external load balancer must explicitly set `HOST=0.0.0.0`.
For ECS/Fargate, set `HOST` in the container's task-definition environment;
otherwise the process is reachable only from the task itself.
Put it behind an HTTPS/TLS terminator before exposing `/oauth/qurl/start`,
`/oauth/qurl/callback`, or `/api/messages`.
OAuth cookies are marked `Secure`, and the configured `TEAMS_BASE_URL` must be
the public HTTPS origin used for the callback. The proxy should forward the
Teams messaging route and the OAuth routes to the same server instance.

The first tenant member who runs `setup <email>` and completes the verified,
email-bound OAuth flow becomes the tenant owner. This first-authenticated-
installer behavior is intentional; later setup attempts are owner-gated and
cannot silently rebind the tenant to another qURL account.

Uninstall removes all local tenant state after revoking the tenant API key.
The current upstream qURL external-identity-binding API has no documented
owner-authorized delete or rebind operation. If a later setup reports a
retained upstream binding, the qURL operator must remove it before reinstall;
the bot deliberately does not treat an unowned local tenant plus upstream 409
as permission to claim that binding.
