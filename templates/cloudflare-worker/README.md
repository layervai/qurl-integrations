# qURL™ mint endpoint

This endpoint lets a trusted server or agent mint a qURL for one configured
HTTPS target. It uses the published TypeScript SDK's `createPortalForUrl()`
helper. The older proposed `hideUrl()` name in issue #123 became this helper.

It accepts `POST` only and returns `{ "link": "…", "expires_at": "…" }`.
The link is valid for 60 seconds; the API controls the account limits and
session duration. The target and lifetime come from server configuration,
never from the request body or query string. Responses use `Cache-Control:
no-store`, including errors. There are no automatic mint retries.

Configure these values in the platform's secret/environment settings:

| Name              | Value                                                                                        |
| ----------------- | -------------------------------------------------------------------------------------------- |
| `QURL_API_KEY`    | A qURL API key with permission to create links                                               |
| `QURL_TARGET_URL` | The fixed HTTPS target, for example `https://internal.example.com/admin`                     |
| `MINT_TOKEN`      | A separate random secret of at least 32 characters; generate one with `openssl rand -hex 32` |

Keep both credentials on trusted servers. Do not embed them in browser code,
commit them, or put them in a URL. Use different credentials for previews and
production. This small example uses a bearer token for server-to-server
calls; use your application's authorization before minting for browser users.
Apply your platform's rate and spend limits to the endpoint before sharing it.

Registering a target and minting a link do not remove its existing public
access. Configure the origin's LayerV access boundary separately. This is an
on-demand mint endpoint, not middleware to run on every application request.
Do not log returned links: they are access credentials.

From a trusted terminal, invoke the deployed endpoint with the token in a
header, not the URL:

```sh
curl --fail-with-body --request POST \
  --header "Authorization: Bearer $MINT_TOKEN" \
  https://YOUR_MINT_HOST/api/create-qurl
```

A successful request creates a real link using the configured account.
Use a test account and test target when you first check the deployment.
Unauthenticated requests return 401; missing configuration returns 503;
upstream failures return 502 without exposing API error details.

## Cloudflare Worker

Copy this directory, then run `npm ci --omit=optional`.
Set the three values with `npx wrangler secret put NAME` (one invocation per
name). For local tests, put them in `.dev.vars`, which is ignored by Git.
Run `npx wrangler dev --local` and POST to
`http://localhost:8787/api/create-qurl`. The Worker accepts POST on any path;
use a dedicated Worker hostname, or configure a route for just this endpoint.

Review the Worker name in `wrangler.toml`, then deploy with
`npx wrangler deploy`. This command publishes the Worker. A bundle-only check
is `npx wrangler deploy --dry-run`.

Platform references: [secrets](https://developers.cloudflare.com/workers/configuration/secrets/)
and [fetch handlers](https://developers.cloudflare.com/workers/runtime-apis/handlers/fetch/).
