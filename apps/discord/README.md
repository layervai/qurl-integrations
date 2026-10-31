# qURL Discord Bot

Share files and locations in Discord as **one-time, expiring qURL™ links** —
delivered privately to each recipient's DMs, never posted in the channel, and
revocable at any time.

## Features

- **One-time links** — each recipient gets their own link that works exactly
  once.
- **Private delivery** — links arrive as a DM, never in the channel.
- **Expiry & self-destruct** — links expire (default 24 hours) and can start a
  countdown after the first open.
- **Personal message** — attach a note shown to each recipient.
- **Revoke anytime** — kill every link from a previous share with one command.
- **Per-server setup** — each server connects its own qURL account; keys are
  encrypted at rest.

## Commands

| Command | Description |
|---------|-------------|
| `/qurl send` | Share a file as one-time qURL links, DM'd to the recipients you pick |
| `/qurl map` | Share a Google Maps location as one-time qURL links *(where enabled)* |
| `/qurl revoke` | Revoke every link from a previous send |
| `/qurl help` | Show the command reference |
| `/qurl setup` | *(admin)* Connect this server to qURL |
| `/qurl status` | *(admin)* Verify the stored key and show its prefix and scopes |

### `/qurl send` options

| Option | Required | Description |
|--------|----------|-------------|
| `attachment` | Yes | The file to share |
| `recipients` | No | Paste `@mentions`. Leave blank to pick from a menu. |
| `expires-in` | No | How long the links stay valid (default: 24 hours) |
| `self-destruct` | No | Countdown after the first open (default: no timer) |
| `personal-message` | No | A note included in each recipient's DM |

When `MAP_COMMAND_ENABLED=true`, `/qurl map` shares a location instead of a
file: it takes a required `location` (a Google Maps URL, or a place/address to
search) in place of `attachment`, the
same `recipients` / `expires-in` / `self-destruct` / `personal-message` options,
and an optional `location-name` to override the label recipients see.

## Getting started

### 1. Add the bot to your server

Use the **Add to Discord** link on layerv.ai. It opens the bot's
`/oauth/discord/install` endpoint, where the deployment builds the matching
Discord authorization URL and chains the install into qURL sign-in. The
Discord OAuth request includes `identify` because the callback uses
`/users/@me` to bind setup to the installing admin. The Discord application
must register the deployment's exact `/oauth/discord/callback` URL and enable
**Require OAuth2 Code Grant** so Discord waits for the callback exchange before
finishing the bot installation. The callback always binds the server from the
authoritative guild in Discord's token response. Grant only **View Channels**,
**Send Messages**, **Embed Links**, and **Use Application Commands**
(permission bitfield `2147503104`).

After seeding or rotating `DISCORD_CLIENT_SECRET`, restart the HTTP service;
ECS injects the SSM value and the bot derives install readiness only at process
start.

**Require OAuth2 Code Grant** applies to the entire Discord application. Deploy
the callback endpoint and register its exact URL before enabling the setting;
legacy static invite links do not complete installation after it is enabled.

> On the multi-tenant public bot, slash commands can take up to an hour to
> appear the first time the bot joins a server, while Discord propagates the
> global command registration. Single-server deployments register per-guild,
> so commands appear right away.

### 2. Connect qURL (admin)

The Add to Discord flow prompts the installing admin to sign in to qURL and
connects the server automatically. For a bot that is already installed, a
server admin can run `/qurl setup` to complete or replace that connection. The
key is stored **encrypted at rest** and scoped to the server. Run `/qurl status`
to confirm the connection.

Every Add to Discord and `/qurl setup` asks for a fresh qURL sign-in, even if
the browser is already signed in, so a guild is never silently bound to
whichever qURL account that browser session belongs to.

### 3. Share

```
/qurl send attachment:<file> recipients:@alice @bob
```

Each recipient receives a DM with a one-time link. Use `/qurl revoke` to
invalidate the links from any previous send.

> Recipients must allow direct messages from server members to receive their
> link.

## Child revocation rollout

This release upgrades `@layervai/qurl` from 0.6.x to 2.x. Detect uses CRIDs
and binds the signed link to the expected CRID. The image omits the optional
native state store; the bot does not use producer state.

Deploy and validate the connector revoke endpoint before deploying this
consumer. Until that endpoint is enabled, ordinary children use the SDK
fallback. Watermarked children cannot be confirmed through that fallback;
the send stays available for retry. Legacy send rows with no usable `qurl_id`
also remain unconfirmed after endpoint activation; deleting their shared parent
is not a safe recovery. A repeated connector 429 fails closed
after one bounded retry, without an SDK fallback. Revoke buttons and selects
share a process-local guard for each sender and send. The guard remains active
until the work settles, including after the 13-minute result wait if needed.
The revoke work deadline is 12 minutes. The same signal stops queued resource
work, connector requests, SDK calls, DynamoDB requests/pages, and recipient DM
edits. Confirmed deletes are finalized before best-effort DM edits. Waiting on
Discord's queue is also bounded by the remaining deadline. Already confirmed
children remain confirmed; all others stay unconfirmed
and the durable intent remains for retry. An abort can race a completed remote
write, so retry reconciles uncertain results instead of claiming success.
Ordinary SDK revokes use one attempt per read/delete: SDK GET Retry-After sleeps
do not accept caller cancellation, so transient failures return for an explicit
retry. The connector retains its one bounded admission retry. Store checks still
enforce ownership and prevent new recipients across processes.

While the connector route is off, each resource re-probes it on every retry.
The SDK then tries up to ten child GETs to find a parent. Watermarked children
all return 404 through this fallback, so a retry can repeat all ten GETs per
resource, across five resources at once. An ordinary batch can also need up to
ten sequential child DELETEs after its parent is found. Each SDK operation
may retry once on a transient failure. Do not stop parent lookup after a few 404s: a later ordinary child can still identify the parent.
The 13-minute message bounds the interaction wait, not total background work.
Enable the connector route through its normal validation and rollout gates;
measure large-send throughput separately (#1489).

Before deployment, configure alarms for `Connector mint_link returned a link
without a valid qurl_id`, `Connector mint_link over-minted`, `Connector mint_link
returned more partial links than requested`, `Minted link revoke incomplete`,
and `Revoke select acknowledgement failed`. Update any filters or saved queries
that use `Failed to revoke QURL`, `missing resource identity`, or the partial-mint
`resource_id` field: these become `Failed to revoke qURL`, `missing resource or
token identity`, and `resource_ref`. The load-test warning now says
`carried no usable resource identifier`.

Each mint request allows 65 seconds for the connector’s 55-second deadline
and response transport. Only the connector’s `request_admission_rejected` 429
with `success: false` and an empty `links` array is retried, up to five times,
with bounded backoff and a 100-second total mint budget. Other errors and
partial results are never retried. A failing mint and its cleanup can take
up to 290 seconds before the error is reported; earlier batches add time.
Cleanup can continue after that reply; under degraded service a 30-child
resource can take about 17 minutes. Check completion logs before manual cleanup.
Load-test upload records rejected by SDK 2.x stay in the cleanup ledger;
remove them only after cleanup or link expiry is confirmed.

## Configuration

The bot is a Node.js service (**Node ≥ 22**) backed by DynamoDB. Copy
`.env.example` to `.env` and fill it in — every variable is documented inline.
The variables below are the ones most deployments need; see `.env.example` for
the complete reference, including advanced operational and per-deployment knobs.

In the **Required** column: **Yes**/**No** means always/never required; **Production**
means required when `NODE_ENV=production`; a feature label (e.g. `/qurl map`, OAuth
setup) means required to use that feature.

| Variable | Required | Description |
|----------|----------|-------------|
| `DISCORD_TOKEN` | Yes | Discord bot token |
| `DISCORD_CLIENT_ID` | Customer install | Numeric Discord application ID for the one-click Add to Discord flow; `PLACEHOLDER` or a malformed value disables that flow |
| `DISCORD_CLIENT_SECRET` | Customer install | Discord OAuth2 client secret for the one-click Add to Discord flow |
| `QURL_API_KEY` | `/qurl detect`; Production when `QURL_WEBHOOK_SECRET` is set | Requires `qurl:read` and `qurl:write` for detect; also the fallback for send operations without a server key from `/qurl setup`. |
| `QURL_ENDPOINT` | No | qURL API base URL (defaults to production; localhost in dev) |
| `QURL_WEBHOOK_SECRET` | Default webhook | Shared HMAC secret written by the registrar Lambda; required in every process that links guild webhooks when a default subscription exists. |
| `QURL_WEBHOOK_PURE_BYOK` | No | Set to `true` only when the deployment intentionally has no default webhook subscription. |
| `CONNECTOR_URL` | No | qURL connector URL for file upload + serving |
| `BASE_URL` | OAuth setup | Public `https://` origin of the bot; required to complete OAuth setup (defaults to `http://localhost:3000`). Local customer-install testing must use `localhost` or HTTPS because its `__Host-` session cookie is always `Secure`. |
| `AUTH0_EMAIL_CONNECTION` | No | Auth0 connection pinned on setup/install authorize redirects (e.g. `email`). Unset or `PLACEHOLDER` sends no pin; a malformed value blocks every `/qurl setup` entry path until corrected. |
| `RATE_LIMIT_INSTALL_MAX_REQUESTS` | No | Per-IP ceiling for the public `/oauth/discord/install` page per rate-limit window (default 120). Completed installs are still bounded by `RATE_LIMIT_MAX_REQUESTS` (two callback slots per install). Both limits are per bot task, not global. |
| `KEY_ENCRYPTION_KEY` | Production | 32 random bytes, base64 — encrypts stored keys at rest |
| `METRICS_TOKEN` | Production | Bearer token guarding the `/metrics` endpoint |
| `MAP_COMMAND_ENABLED` | No | Set to `true` to enable `/qurl map` (default off) |
| `DETECT_COMMAND_ENABLED` | No | Set to `true` to enable `/qurl detect` (default off) |
| `QURL_DEPLOYMENT` | Native `/qurl detect` | Environment-specific public SDK trust: JSON or an absolute JSON file path, with `issuers` and `cells` |
| `DETECT_TUNNEL_SLUG` | `/qurl detect` | qURL tunnel resource slug used to mint short-lived `/api/detect/discord/<guild_id>` qURLs |
| `DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS` | No | Comma-separated extra non-prod `QURL_ENDPOINT` hosts for `/qurl detect` (extends the built-in set below) |
| `DETECT_EXTRA_NON_PROD_HOST_SUFFIXES` | No | Comma-separated extra `qurl_site` suffixes granted for the hosts above; each entry must start with `.` |
| `GOOGLE_MAPS_API_KEY` | `/qurl map` | Google Maps key for location autocomplete (needed when map is enabled) |
| `GUILD_ID` | No | Scope commands to a single server; unset runs the multi-tenant public bot |
| `PORT` | No | HTTP listen port (default 3000) |

Map autocomplete has a per-user burst budget of ten new Places requests,
refilling one request per second. Cache hits and shared in-flight results spend
no tokens. Prefix and guild changes share that budget. Short queries,
pasted URLs and disabled or unsupported contexts use no tokens. Throttled or
unidentified interactions receive an empty suggestion list before Places I/O.
The process keeps at most 10,000 active user budgets; at capacity, new users
receive empty suggestions until the oldest budget is idle for ten seconds.
This protects each process; it is not a global multi-replica limit or a billing
cap. Move to a shared store if cross-replica abuse requires a global budget.

Discord uses `@layervai/qurl/node` to open current `qv2t1` links. Set
`QURL_ENDPOINT` and `QURL_DEPLOYMENT` for the same environment. The deployment
settings contain trusted issuer public keys (`kid`, `spki_der_b64`) and cell
endpoints (`host`, `port`, `server_public_key_b64`). No trust root is embedded
in the bot image. The SDK verifies the link and opens native UDP access, then
Discord sends the image to the authenticated detect endpoint. Each request
closes its opener on success or failure. Detect requires a signed native link.
The bot mints `target_path=/api/detect/discord/<guild_id>` from the authenticated
Discord interaction. The image request carries no API key or guild header.
The detect service uses one exact guild-scoped attribution read. The private
binding route remains separate and does not accept these guild-scoped rows.

Run `npm run test:detect:live` with the deployment environment above and
`DETECT_SMOKE_GUILD_ID` set to a test server ID. The check mints, opens, and
POSTs an unmarked PNG through the real tunnel, and requires a no-match result.
To check known attribution, add `DETECT_SMOKE_QURL_ID` and run
`npm run test:detect:live -- /absolute/path/to/watermarked.png`.

When enabling `/qurl detect`, the minted `qurl_site` must be host-only. The
detect target is constructed from that value, so both have the same hostname
after URL case normalization. Validation retains that equality as a fail-closed
invariant if the target source changes later. The hostname must also sit under a
supported qURL tunnel suffix. A qURL site may use an `r_<11 chars>` Traefik
routing label, but that label carries no resource identity and is not compared
with the resource's opaque public-key ID.

The authenticated mint is the authority for that hostname, so any hostname
with only non-empty labels that it returns beneath an allowlisted suffix is
accepted after the URL and SSRF guards, including hostnames with multiple
routing labels. The suffix allowlist constrains the target to a trusted qURL
tunnel namespace; it is not a tenant identity signal. The authenticated native
target must match the exact expected URL before image bytes leave the bot.
The mint response also must echo the selected resource ID and guild path.
Both the qURL expiry and access-session duration are set to five minutes;
expiring a qURL does not shorten an already-open session.

Production `QURL_ENDPOINT` accepts only `*.qurl.site`; sandbox/staging
tunnel suffixes are accepted as a non-prod set only for explicit non-prod qURL API hosts
(`localhost`, `127.0.0.1`, `[::1]`, `api.test.local`,
`api.staging.layerv.ai`); the endpoint host does not bind to one specific
non-prod suffix. Unknown endpoint hosts, including unlisted `.local` hosts, fail
closed to production tunnel suffixes. If tunnel infra adds a suffix or a
path-based `qurl_site`, update the detect host-pin/path contract and tests before
flipping `DETECT_COMMAND_ENABLED=true`.
The built-in non-prod set above can be extended via
`DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS` and `DETECT_EXTRA_NON_PROD_HOST_SUFFIXES`
(comma-separated, trimmed, lowercased; suffixes must start with `.`) — e.g.
`DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS=api.sandbox.example` paired with
`DETECT_EXTRA_NON_PROD_HOST_SUFFIXES=.tunnel.sandbox.example` — so a private
deploy can grant its own non-prod tunnel suffix without a code change to this
public repo. A malformed suffix (missing the leading `.`) fails the bot at boot.
The bot lists the detect resource by slug only and filters active resources
client-side because the live API rejects combining `slug` and `status`; the SDK
auto-paginator walks historical revoked rows for this single dark-launch slug.
If a tunnel rotation creates more than one active resource for the slug, detect
fails closed instead of guessing which tunnel should receive the image POST.
Persistent hard failures arm a short process-wide retry backoff for
the single dark-launch slug so a broken tunnel does not re-walk the full slug
history on every detect attempt. Before broad enablement, keep the detect slug's
revoked-resource history trimmed or add upstream server-side active filtering;
cold-cache and backoff-recovery scans grow with accumulated historical rows.

Generate `KEY_ENCRYPTION_KEY` with:

```bash
node -e "console.log(require('crypto').randomBytes(32).toString('base64'))"
```

In production the process refuses to boot without `KEY_ENCRYPTION_KEY` and
`METRICS_TOKEN`. In local development, leaving `KEY_ENCRYPTION_KEY` unset stores
keys in plaintext with a loud warning.

The bot's Discord application must have the **Server Members Intent** privileged
gateway intent enabled (Developer Portal → Bot → Privileged Gateway Intents).
It is required to resolve recipients for `/qurl send` and `/qurl map`, and the
bot fails to start without it.

## Development

```bash
npm ci
npm run dev   # node --watch
npm test      # jest
npm run lint  # eslint, zero warnings
```

Slash commands register automatically when the bot starts.

`npm test` mocks the AWS SDK and needs no external services. Running the bot
locally (`npm run dev`) needs a DynamoDB endpoint — `docker-compose.yml` spins
up a local DynamoDB and `scripts/provision-ddb-local.js` creates the tables.
See `.env.example` for the local-development environment setup.

The provisioner reads `scripts/ddb-local-schema.json`. Infrastructure CI compares
that data file from upstream `main` with mocked Sandbox and production plans:
table names, keys and types, GSIs and projections, and TTL. Schema changes need
a paired infrastructure check. Existing local tables are not migrated; recreate
only your disposable local tables when needed. The provisioner fails with a
repair instruction if an existing table lacks a required GSI. It never drops data.

## Architecture

- **Multi-tenant by default** — the bot serves every server it's invited to.
  Each server connects its own qURL account via `/qurl setup`; keys are
  envelope-encrypted (AES-256-GCM) at rest in DynamoDB.
- **qURL API client** — creates one-time links and revokes them, with an
  SSRF guard on target URLs.
- **Connector** — uploads and serves shared files through the qURL connector
  behind an SSRF-guarded fetch.
- **HTTP surface** — `/health` for load-balancer probes, `/metrics` (bearer
  authenticated), `/oauth/discord/install` plus its callback for customer
  installs, and the qURL OAuth start/callback routes used by `/qurl setup`.

Gateway session persistence admits at most 100 in-flight background writes.
The live in-memory session keeps advancing during backpressure. Look for
`gateway-session-store: write backpressure`, limited to one warning per minute
with count-only fields. Skipped checkpoints can require a fresh IDENTIFY after
restart. A skipped null-clear can leave a rejected session row for Discord to
validate on the next boot; final flush drains writes but does not retry a
null-mirror delete. A non-null mirror is persisted after the drain.

## Troubleshooting

**"qURL is not configured"** — an admin needs to run `/qurl setup` on this
server. Check the current state with `/qurl status`.

**Recipients didn't get a DM** — each recipient must allow direct messages from
server members. The link is delivered privately, never in the channel.

**Slash commands don't appear** — after a first invite, global commands can take
up to an hour to propagate (single-server installs appear right away). If they
still don't show, confirm the bot was invited with the **Use Application
Commands** permission.

## License

[MIT](../../LICENSE) — Copyright (c) 2025-present LayerV, Inc.

Setup and install cookies use `__Host-` names with `Secure`, `HttpOnly`,
`SameSite=Lax`, and `Path=/`. Use HTTPS (or browser-supported localhost) for
local setup tests. Deploying this change invalidates older setup cookies;
admins with an in-flight setup must start again.

### Recover an id-less legacy send

The original send time and duration do not prove current expiry: an owner can
extend a link. If a legacy row has no child ID, keep it unconfirmed until an
operator independently verifies that every upstream link for that send is
closed. Do not use a recipient resolve/knock as a read-only expiry check.

After that verification, use the bounded one-shot tool below from the repo
root. First confirm the approved AWS account/profile, region and table prefix.
The deployed bot must enforce the durable `revoking_at` guard on Add Recipients
writes. These commands use normal AWS credentials; no bot or qURL API key is
needed.

```sh
export AWS_PROFILE='<approved-profile>'
export AWS_REGION='<region>'
export DDB_TABLE_PREFIX='<env-prefix-ending-in-dash>'
aws sts get-caller-identity
node apps/discord/scripts/finalize-legacy-send.js --send-id '<send-id>' --sender-id '<sender-id>'
```

The default dry run reports aggregate local eligibility only. It does not
verify upstream closure. After reviewing that result and the independent
upstream evidence, repeat the same command with both write flags:

```sh
node apps/discord/scripts/finalize-legacy-send.js --send-id '<send-id>' --sender-id '<sender-id>' --apply --confirm-upstream-closed
```

The tool reads one send partition with strong consistency. Each snapshot is
bounded to 200 pages of 100 evaluated rows; apply takes two snapshots within
one 60-second operation budget. It rejects foreign,
malformed or identified rows, establishes the existing no-more-recipients
barrier, then re-reads the same rows before a conditional finalization. It only
updates the send-config bookkeeping and records
`legacy_close_reason=operator_attested_upstream_closed`. It leaves recipient
rows and upstream resources unchanged. It reads no bearer links or encrypted
config fields and prints no participant identifiers.

A failure can leave the intent barrier set, and a lost write response can mean
the finalization committed. Re-run the dry run against the same target. Do not
remove the barrier blindly; use normal revoke/repair when the rows changed.
A `local_record_already_closed` result describes local bookkeeping, not
upstream proof.
