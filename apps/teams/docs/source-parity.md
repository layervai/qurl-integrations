# Teams OAuth source-parity checklist

This checklist records the security contracts carried into the qURL™ Teams
OAuth core from the shipped Discord flow and the Slack design reference. It is
an implementation provenance record, not deployment documentation.

Sources reviewed in order:

1. Discord #853 live files: `apps/discord/src/routes/qurl-oauth.js`,
   `apps/discord/src/utils/oauth-pkce.js`, `oauth-state.js`,
   `qurl-oauth-state.js`, `cookies.js`, `oauth-cookies.js`, `auth0-jwks.js`,
   `crypto.js`, `oauth-rate-limit.js`, and `apps/discord/README.md`.
2. Slack #856 at signed design head
   `bbda66734005e24eb4b1db5884d5200d6aa3bfc0`.

The superseded Teams PR #987 was not used as an implementation source; no
behavior from it survives in this package.

## Discord #853 parity

| Security-relevant behavior | Status | Teams rationale |
| --- | --- | --- |
| Confidential Authorization Code client sends `client_secret` | Ported | The injected token client always authenticates the code exchange. |
| PKCE uses a 256-bit verifier and S256 challenge | Ported | Verifier stays in the backend transaction and is sent only to the token endpoint. |
| OAuth state expires after five minutes | Ported | Numeric application-clock expiry is checked during atomic consume; storage TTL is cleanup only. |
| Double-submit state cookie | Ported | Constant-time cookie/state comparison happens before state consumption or network work. |
| Cookie is HttpOnly and SameSite=Lax | Ported | The cookie contract fixes both attributes. |
| Cookie is Secure only when the request appears HTTPS | Intentionally divergent | Teams always requires `Secure`; S04b must terminate the public flow on HTTPS. |
| Cookie path is limited to the qURL OAuth surface | Ported | The contract fixes `/oauth/qurl`, avoiding exposure to unrelated routes. |
| Authorization requests use `openid`, `email`, `qurl:read`, and `qurl:write` | Ported | The exact immutable scope sequence is `openid email qurl:read qurl:write`. |
| `offline_access` is absent and refresh tokens are not used | Ported | The token result omits refresh tokens even if an upstream response includes one. |
| Token exchange uses form encoding and a bounded timeout | Ported | The whole exchange, including its single rotation retry, has one strict deadline. |
| Upstream response text is bounded before parsing or logging | Strengthened | Both token and JWKS bodies are capped; neither body is included in logs or errors. |
| Access token type/absence is rejected | Ported | Access and ID tokens must both be non-empty strings before the callback continues. |
| ID token verifies signature, issuer, audience, and expiry against cached JWKS | Ported | Verification also requires nonce, subject, email, and `email_verified: true`. |
| JWKS refreshes on a signing-key miss | Ported | A cached key miss forces one bounded refresh for normal key rotation. |
| Verified email is a display cue but not a Discord binding gate | Intentionally divergent | Teams setup email is a security boundary; mismatch is a stable typed failure. |
| Callback cookie is cleared after use and refresh cannot repeat setup | Contract moved to S04b | This core exports the clear-cookie contract and rejects callback reuse through consumed state. |
| Generic browser errors; detailed upstream values stay out of the wire response | Ported | Core errors contain stable codes and sanitized messages only. |
| OAuth rate limiting and no-store/browser security headers | Deferred to S04b | They are route concerns and no HTTP routes exist in S04a. |
| Consent prompting and first-install/re-run UI decisions | Deferred to S04b | The pure authorization builder does not decide route UX. |
| API-key mint, encrypted credential persistence, orphan cleanup, and confirmation DM | Intentionally absent | The future SDK-backed provider-binding adapter owns the immediate post-verification call. |

## Slack #856 parity

| Security-relevant behavior | Status | Teams rationale |
| --- | --- | --- |
| State is a 256-bit opaque random handle with no front-channel identity payload | Ported | The browser receives only a canonical 43-character base64url handle. |
| State binds platform tenant, actor, delivery identity, normalized email, mode, PKCE verifier, nonce, and expiry | Ported and narrowed | The persisted transaction contains exactly those fields plus its SHA-256 lookup key; the initial Teams mode is only `bind`. |
| Raw state handle is stored as a lookup key | Intentionally strengthened | Persistence sees only `SHA-256(handle)`; there is no raw-handle storage or compatibility path. |
| Conditional create rejects a random-handle collision | Ported | The persistence interface returns an explicit conditional conflict and mint fails closed. |
| Callback consumes state atomically before token exchange | Ported | The callback orchestrator cannot call fetch or binding until consume succeeds. |
| Consumed state cannot be replayed | Ported | Missing and already-consumed handles share the same fail-closed typed result. |
| Expiry is enforced from application time despite asynchronous TTL cleanup | Ported | The consume contract receives application time and the core double-checks returned expiry, including Slack's 30-second cross-worker clock-skew allowance. |
| OIDC nonce is bound to the transaction and verified on the signed token | Ported | Missing or mismatched nonce is fatal. |
| Subject is mandatory for provider binding | Ported | Empty or missing `sub` is fatal after signature verification. |
| Email must be present, verified, normalized, and equal setup email | Ported and tightened | Email is mandatory for every Teams setup transaction rather than best-effort display data. |
| Double-submit cookie binds callback to the browser that started setup | Ported | Missing or mismatched cookie fails before consume. |
| PKCE verifier stays backend-side | Ported | It is never placed in state, cookies, errors, or logs. |
| First binding wins; a different account cannot silently overwrite it | Ported as interface contract | `ProviderBinder` requires an atomic result of bound, already-bound, or conflict; S04b supplies the adapter. |
| Slack `reuse`, `rotate`, and `repoint` setup modes | Intentionally divergent | Teams exposes only `bind`; additional modes arrive with the future #910/#956 adapters as their own reviewed change. |
| Legacy signed/HMAC state accepted for rollout overlap | Intentionally absent | Teams has no deployed legacy flow, so there is no Slack legacy-HMAC path. |
| State rows share the durable workspace credential table | Intentionally divergent | S04b must provide a dedicated ephemeral state store, not the durable credential table. |
| Slack platform-install OAuth under `/oauth/slack/*` | Intentionally absent | Teams installs by manifest sideload; there is no platform-install OAuth surface. |
| qURL-service binding and legacy mint fallback | Interface-only / intentionally divergent | S04a defines the provider-binding result contract only; no ad hoc HTTP or legacy mint path exists. |

## Secret rotation and scope fences

- `AUTH0_CLIENT_SECRET_FALLBACK` is eligible only after a bounded token response
  parses to the exact OAuth error `invalid_client` from the primary attempt.
- The fallback is attempted at most once within the original timeout. Network
  errors, timeouts, malformed bodies, oversized bodies, and all other OAuth
  errors never select it.
- Neither secret, authorization code, state handle, PKCE verifier, nonce,
  access token, ID token, refresh token, cookie value, nor upstream body is
  logged or embedded in an error.
- There is no `offline_access`, legacy-HMAC, raw-state, provider-HTTP, route,
  DynamoDB/AWS, Teams SDK, or cross-runtime shared-package path in S04a.
