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
