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

The Bot supports setup, resource listing/get, URL and connector protection,
aliases, display names, admin membership, uninstall, feedback, and private
Teams delivery. Policies and principals are stored as normalized rows matching
the `qurl-teams-ddb` Terraform module; OAuth state uses `state_handle_hash`
and numeric `expires_at`. Tenant administrators can manage tenant-wide resource
metadata and revocation from any channel; channel aliases and visibility remain
scope-specific.

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
