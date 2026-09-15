# qURL Teams OAuth core

This package is the dependency-injected OAuth security core for the future
qURL™ Microsoft Teams integration. It implements opaque one-shot state,
PKCE S256, OIDC nonce binding, a hardened confidential-client token exchange,
ID-token verification, and an interface-only provider bind.

It intentionally contains no HTTP routes, database adapter, Teams SDK, Auth0
deployment configuration, or qURL provider-binding adapter. Those integration
layers arrive in S04b.

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

