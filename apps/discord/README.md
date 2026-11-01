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
| `/qurl-admin setup` | *(admin)* Connect this server to qURL |
| `/qurl-admin status` | *(admin)* Verify the stored key and show its prefix and scopes |

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
server admin can run `/qurl-admin setup` to complete or replace that connection. The
key is stored **encrypted at rest** and scoped to the server. Run `/qurl-admin status`
to confirm the connection.

Every Add to Discord and `/qurl-admin setup` asks for a fresh qURL sign-in, even if
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

Saved queries for `Failed to prepare QURL links` must use
`Failed to prepare qURL links` after this source is deployed. API names,
environment variables, headers, and storage method names keep their exact spelling.

Upload, mint, create, and revoke logs use `resource_ref` instead of raw
`resource_id`; create logs also use `crid_ref` instead of raw `crid`.
To find a known resource in logs, compute its reference from the repository root:

```sh
node -e 'console.log(require("./apps/discord/src/utils/resource-id").resourceIdLogRef(process.argv[1]))' RESOURCE_ID
```

The reference is `sha256:` plus the first 12 lowercase hex digits of the
SHA-256 hash of the exact UTF-8 identifier. It is a log lookup hint; confirm
the resource in the owner-scoped store before taking action. Update saved
queries that used the old raw ID fields.

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

## Command registration scope changes

Set `GUILD_ID` to a guild snowflake for single-guild commands, or leave it
unset for global commands. Set `PREVIOUS_COMMAND_SCOPE` only during a reviewed
scope change: `global` for an old global namespace, or the old guild snowflake.
The infra runtime must forward it before startup. It works without a guild cache.

Boot registers the active scope first, then clears the explicitly named inactive
scope. It never clears the active scope or scans every joined guild's command API.
A normal single-guild boot leaves global commands alone. Global commands belong
to the application: deployments with different command modes must use separate
Discord applications. Never clear a namespace still used by another deployment.

Before the first rollout, identify known historical namespaces that need cleanup.
Carry each as the previous scope in turn; arbitrary guild leftovers are no longer
cleared automatically. Keep the setting until that namespace is verified empty,
then clear it. Cleanup failures are logged and a later boot retries the setting.

Verify active and inactive namespaces after old tasks drain. Old tasks can
register old commands again while running. Discord clients may cache prior
registrations; the dispatch-time command filter remains in place.

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
| `QURL_API_KEY` | `/qurl detect`; Production when `QURL_WEBHOOK_SECRET` is set | Requires `qurl:read` and `qurl:write` for detect; also the fallback for send operations without a server key from `/qurl-admin setup`. |
| `QURL_ENDPOINT` | No | qURL API base URL (defaults to production; localhost in dev) |
| `QURL_WEBHOOK_SECRET` | Default webhook | Shared HMAC secret written by the registrar Lambda; required in every process that links guild webhooks when a default subscription exists. |
| `QURL_WEBHOOK_PURE_BYOK` | No | Set to `true` only when the deployment intentionally has no default webhook subscription. |
| `QURL_WEBHOOK_SETUP_WAIT_MS` | No | Primary subscription-confirmation wait in milliseconds, 1–5000 (default 3000). A timeout shows a view-counting notice and leaves accepted work running. |
| `CONNECTOR_URL` | No | qURL connector URL for file upload + serving |
| `BASE_URL` | OAuth setup | Public `https://` origin of the bot; required to complete OAuth setup (defaults to `http://localhost:3000`). Local customer-install testing must use `localhost` or HTTPS because its `__Host-` session cookie is always `Secure`. |
| `AUTH0_EMAIL_CONNECTION` | No | Auth0 connection pinned on setup/install authorize redirects (e.g. `email`). Unset or `PLACEHOLDER` sends no pin; a malformed value blocks every `/qurl-admin setup` entry path until corrected. |
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
| `PREVIOUS_COMMAND_SCOPE` | No | Inactive namespace to clear during a scope change: `global` or the old guild snowflake; unset by default |
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

Setup and status are registered under `/qurl-admin`, with the Manage Server
permission. `/qurl` keeps send, revoke, help and enabled optional user actions.
Old cached `/qurl setup` and `/qurl status` submissions still reach the same
runtime permission checks; they are absent from the new public command list.

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
  Each server connects its own qURL account via `/qurl-admin setup`; keys are
  envelope-encrypted (AES-256-GCM) at rest in DynamoDB.
- **qURL API client** — creates one-time links and revokes them, with an
  SSRF guard on target URLs.
- **Connector** — uploads and serves shared files through the qURL connector
  behind an SSRF-guarded fetch.
- **HTTP surface** — `/health` for load-balancer probes, `/metrics` (bearer
  authenticated), `/oauth/discord/install` plus its callback for customer
  installs, and the qURL OAuth start/callback routes used by `/qurl-admin setup`.

The authenticated JSON `/metrics?view=consumer` response includes `eventConsumer` for this
process: `running`, `qurl_bot_event_inflight`, `qurl_bot_event_inflight_limit`,
`qurl_bot_event_polls_total`, `qurl_bot_event_atcap_polls_total`, and
`qurl_bot_event_dispatch_handler_rejections_total`. The in-flight gauge reads the
current handler set, including changes between polls. Counters last for the
process lifetime and reset on restart; an inactive consumer reports `running:
false`. No message, user, or shard IDs are retained for these metrics. Workers
can consume events from multiple gateway shards, so these are worker metrics.
Workers are HTTP-role processes with `ENABLE_EVENT_SHIPPER` enabled; they serve
this same listener. Gateway-only tasks serve a separate health listener.

The consumer-only view omits aggregate store stats and performs no table scan.
The default `/metrics` response retains its legacy totals, which scan tables;
use the consumer-only view for routine pressure scrapes. Unknown or repeated
`view` values return 400 without scans. These are JSON fields, not Prometheus
exposition text.

For a dashboard, scrape each worker as a stable target; a load-balanced URL
mixes replicas and cannot supply valid counter deltas. Chart
`qurl_bot_event_inflight / qurl_bot_event_inflight_limit` for utilization and
`delta(qurl_bot_event_atcap_polls_total) / delta(qurl_bot_event_polls_total)`
for the fraction of poll attempts at capacity. Ignore reset intervals and zero
denominators. Polls include attempts that later fail at SQS. This is a poll
fraction, not elapsed time at capacity: capped polls back off. Chart deltas of
`qurl_bot_event_dispatch_handler_rejections_total` for rejected tracked promises;
synchronous throws and untracked work are not included. These source fields do
not establish a deployed dashboard or observed capacity.

Gateway session persistence admits at most 100 in-flight background writes.
The live in-memory session keeps advancing during backpressure. Look for
`gateway-session-store: write backpressure`, limited to one warning per minute
with count-only fields. Skipped checkpoints can require a fresh IDENTIFY after
restart. A skipped null-clear can leave a rejected session row for Discord to
validate on the next boot; final flush drains writes but does not retry a
null-mirror delete. A non-null mirror is persisted after the drain.

## Troubleshooting

**"qURL is not configured"** — an admin needs to run `/qurl-admin setup` on this
server. Check the current state with `/qurl-admin status`.

**Recipients didn't get a DM** — each recipient must allow direct messages from
server members. The link is delivered privately, never in the channel.

**Slash commands don't appear** — after a first invite, global commands can take
up to an hour to propagate (single-server installs appear right away). If they
still don't show, confirm the bot was invited with the **Use Application
Commands** permission.

**Add Recipients failed after a storage timeout** — a final guarded transaction
can commit while its response is lost. The handler sends no DMs and tries to
revoke the qURLs just minted for the new recipients. Successful cleanup can
leave `qurl_sends` rows that
point at revoked children. Treat those as failed-delivery residue during
status/revoke triage. The sender sees “Failed to save link records. Recipients
were not messaged.” Find `recordQURLSendBatch failed in addRecipients; aborting
before DMs` and the same send ID in cleanup logs. `Failed to clean up freshly
minted Add Recipients qURL resources` or `Add Recipients cleanup still running
at its wait budget` means revocation is unconfirmed. Use the send ID and available cleanup
references to investigate which grants remain before manual cleanup.

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

### Send-confirm audit counts

`send_confirm_opened` records a successfully delivered `/qurl file` or `/qurl map`
card. It carries resource type, recipient count, picker state, expiry, and flags
for self-destruct and a personal message. It does not carry message text or
recipient lists. `send_confirm_picked` counts committed valid picker changes.
`send_confirm_cancelled` counts only a successful version-fenced cancellation;
its dwell uses the original `created_at`, because picker activity renews expiry.
Unknown creation time omits dwell. Failed claims and failed delivery do not count
as opened cards; failed or losing cancels do not count as cancellations.

For the observed card-to-delivery funnel, use this Logs Insights query on the
relevant bot log groups:

```text
fields audit.event, audit.send_nonce
| filter ispresent(audit.send_nonce)
| filter audit.event in ["send_confirm_opened", "dispatch_sent"]
| stats count_distinct(audit.send_nonce) as cards by audit.event
```

Compare the two distinct-card counts, not raw dispatch rows: one card can send
to many recipients. The bounded random `send_nonce` links the initial pipeline
to its opened card. Add Recipients and legacy callers without that reference
are excluded. High-cardinality distinct counts can be approximate. A time-window
boundary can include only one side of a card's journey. Missing successful
delivery also includes failures and in-progress cards, so this is an observed
completion funnel, not proof of abandonment. Confirm the query against approved
Sandbox traffic after rollout before using it in a production dashboard.

TTL deletion does not emit `send_confirm_expired`: DynamoDB reaping is delayed
and no stream hook is added here. Exact expiry-event counts are unavailable.
Do not infer an exact expiry count from open-minus-delivered cards.

### Setup and shutdown budgets

Setup confirmation ends when the primary subscription is persisted and usable;
slow best-effort sibling propagation does not delay a successful setup reply.
Accepted work remains tracked through propagation and rollback DELETEs.
Normal shutdown drains it for up to 10 seconds before session sealing and storage
close. Its total hard ceiling is 25 seconds, preserving the prior 10-second
teardown allowance plus five seconds for final cleanup. Configure a container stop grace of at least 30 seconds.
Managed Fargate tasks use that window. Active gateway push handoff keeps its
existing 12-second ceiling. Fatal shutdown keeps its existing 10-second graceful
budget inside its 15-second backstop. Both paths stop new link admissions but
skip the accepted-link drain to preserve session shutdown deadlines. Accepted
work can be interrupted on those paths; this change does not establish complete
shutdown safety for every gateway topology.

## Sandbox application metadata

Use the LayerV-owned sandbox Discord application, not the previous personal
developer-portal app:

- Application ID: `1511450217789128885`
- Public Key: `f951fb4d407da2ac37ebb862f074e311d530b6e95940984695a320a1ac9f00ea`

1. https://discord.com/developers/applications → `qURL (sandbox)`
2. General Information → set the application name to **qURL (sandbox)**, upload
   `assets/discord-app-icon.png`, add the description from
   `discord-metadata.json`, and set the privacy/terms URLs listed there.
3. Bot → set the bot username to `qURL` (`bot.username` in
   `discord-metadata.json`); the application/profile branding remains
   **qURL**. Upload `assets/discord-avatar.png`, and enable **Server Members
   Intent** under Privileged Gateway Intents.
4. Installation → default install settings should request `bot` and
   `applications.commands` with permissions `2147503104` (View Channels, Send
   Messages, Embed Links, Use Slash Commands).
5. Copy the bot token.

This script targets the sandbox app only. A production apply needs a separate
reviewed metadata change.

The repeatable metadata source of truth is `discord-metadata.json`. With a
target bot token in `DISCORD_TOKEN`, operators can apply the bot/app fields
that Discord exposes through API (`description`, `icon`, `cover_image`, tags,
install params, bot username, avatar, and banner). The script refuses to run if
the token belongs to any Discord application other than LayerV sandbox app
`1511450217789128885`.

The bot-user API receives `bot.username` exactly. An exact name match is complete
regardless of the discriminator; case differences are applied as name changes.
Discord keeps bots on their existing username system; this script does not wait
for human-account username migration. See the [Discord developer guidance](https://support-dev.discord.com/hc/en-us/articles/13667755828631-Updating-Apps-to-Support-Unique-Usernames).

```bash
npm run apply-discord-metadata
```

Preview the API payload without making changes:

```bash
npm run apply-discord-metadata -- --dry-run
```

Unless `--skip-images` is set, dry-run verifies that every asset referenced by `discord-metadata.json`
exists, can be read, stays under the local byte cap for its Discord surface,
and matches the local dimension rules.

For a repeat apply with unchanged images, use
`npm run apply-discord-metadata -- --skip-images` (also works with `--dry-run`).
This preserves all four stored images and does not read or upload local image
files. Omit the flag for the first branding apply or after changing an asset.
Discord CDN hashes are not source-file hashes, so the script does not guess
whether local images match. Application text/install metadata is still applied
on every run to restore the configured state; its log says "Applied" rather
than claiming those fields changed.

Run the live apply as an operator step after seeding the LayerV-owned token; do
not wire it as an unconditional CI job until image/app PATCH idempotency lands
in https://github.com/layervai/qurl-integrations/issues/588. Username and
image updates are separate Discord `/users/@me` PATCHes, so respect
`retry_after` and rerun instead of looping if either edit is rate-limited.

After the live metadata apply, restart the bot with the same token/app pair
so automatic command registration updates Discord's slash-command picker.
Keep `DISCORD_CLIENT_ID` aligned with the metadata application
ID; the script verifies the token's app, while command registration and invite
links read the client ID from environment/SSM/infra wiring.

Exit codes:
- `0` — API fields applied, including the configured bot username `qURL`;
  application/profile branding remains **qURL**.
- `1` — invalid arguments, local configuration/assets, or a partial API apply.
  Argument and local-validation failures occur before writes. After writes
  start, warnings identify skipped fields; other fields can already be applied.
  The script prints `retry_after` for rate-limited edits and does not retry them
  automatically. Wait for that delay before rerunning.
- `2` — API writes completed, but the application name still differs from
  `discord-metadata.json`; update the name in Developer Portal.
- `3` — fatal application metadata PATCH failure. The application fields did
  not finish applying; earlier bot fields may already be applied. Inspect the
  Discord response, wait for `retry_after` if
  present, and rerun after fixing the API error.
- `4` — pre-flight verification failed before writes. This includes identity
  mismatches, network errors, timeouts and temporary API refusals. Check the
  error first: fix a token/app mismatch, or wait for `retry_after` when the
  failure is temporary. No fields were written by that run.

Application name and legal URLs are Developer Portal-only. Discord's
Edit Current Application API does not list legal URLs as writable fields, so
verify the privacy and terms links directly in Developer Portal after editing
them.

The live apply uses Discord v10's documented current-user fields
(`username`, `avatar`, `banner`) and current-application fields (`description`,
`icon`, `cover_image`, `tags`, `install_params`); an application PATCH failure
is fatal, while bot image failures are reported as partial applies.

### Mint batches and account limits

File and location sends upload once per preparation, then mint against the returned resource in requests of at most ten links. Ten is a request size, not a share limit. The current service reports `max_tokens_per_qurl: -1` as a deprecated compatibility field: there is no per-resource qURL cap. Re-uploading identical content can return the same resource and does not reset account usage. Other account limits, access rules and resource status still apply. Structured `quota_exceeded` failures direct users to their account usage; they do not promise that another upload will fix the limit.

Add Recipients still resolves the retained file or location through the connector once before minting. It does not re-upload between batches. If a later mint or persistence step fails, known child links are still revoked before delivery. The standalone load test follows the same single-upload pattern and retains old re-upload counters only to read historical report inputs.

### Guild scope in the revoke menu

In multi-tenant mode, `/qurl revoke` lists only sends recorded for the invoking guild. Legacy rows without `guild_id` stay hidden; the bot does not infer their guild from the sender. Both send-recording methods write the provided guild on each recipient row. Before live adoption, verify that retained rows have the scope needed for the menu or complete their approved retirement path. The sender-index search remains bounded to ten pages of 100 rows; heavy history in other guilds can exhaust that window. This change does not claim complete cross-guild history coverage or backfill legacy records.

### Recipient-cap policy

Keep one `QURL_SEND_MAX_RECIPIENTS` setting for explicit users, role expansion, voice/stage expansion, `@everyone`, and recipient pickers. Its source default remains 20,000, including the previously selected voice/stage size. A workspace that uses that default also gets the same ceiling for non-voice selections; `@everyone` still requires its existing permission. Operators who need a smaller blast radius must lower the shared setting, which also lowers the voice ceiling. No separate voice-only setting is introduced.

The ceiling is a selection bound, not a delivery-rate or large-send capacity guarantee. Other permissions and operation limits still apply. The large-send warning is visibility, not permission or proof that a send can finish within its interaction window. Keep the existing main-adoption, quota, load and rollout gates; this policy decision does not authorize a live blast or remove those gates.

### Member-cache prewarm

HTTP-only workers reuse a completed member walk for 60 seconds. The cache object
and its size must still match. Failed, capped or stalled walks do not establish
freshness. An exact gateway member count keeps its existing completeness check.
New joiners can be missed during this short window; the existing HTTP-only
cache can also retain departed members. Each send still uses its normal
recipient and grant checks. The worker does not retain a separate guild-ID map
for completion timestamps.

With `LOG_LEVEL=debug`, use this CloudWatch Logs Insights query on the Discord
worker log group to compare completed walks with both hot-cache paths. It returns
aggregate counts and cache sizes, without guild or user IDs:

```text
fields @timestamp, @message
| filter @message like /members pre-warm (short-circuited|complete|failed|hit safety cap)/
| parse @message /members pre-warm (?<event>short-circuited \([^)]*\)|complete|failed|hit safety cap)/
| parse @message /"cache_size":(?<cache_size>[0-9]+)/
| stats count(*) as events, max(cache_size) as largest_cache by event, bin(5m)
```

The event groups distinguish recent walks, exact-count hot caches and failures.
No matching rows with debug logging off means unavailable telemetry.
The retired `/unlinked` command no longer uses an approximate-count threshold.
Long slash-command walks offer progress every ten full pages, with at most one
outstanding edit per interaction. A slow edit cannot stall the walk or other
senders; each sender waits for its own outstanding edit before its final reply.
A failed progress edit does not abort a successfully completed walk or prevent
that completed walk from being marked fresh.


### Sender counter reconciliation

The sender monitor reads a full source snapshot first. With a confirmed display
floor and a recent complete snapshot, healthy ticks read the sharded aggregate
instead of all recipient IDs. Missing or partial aggregate reads, counts below
the known floor, legacy/no-floor rows, recipient additions and clock rollback
force a source read. Incomplete source reads fail; they never establish freshness.

The aggregate remains an optimization. The source fact and shard increment are
not atomic. During dense polling, source reads repeat every 30 seconds to repair
a crash between those writes. At steady intervals above 30 seconds, each tick
still reads source. This keeps the normal five-second poll cadence and existing
webhook fast path, but a new source-only crash gap can take 30 seconds plus the
next tick/read time to repair instead of the old next-tick recovery. Known floors
never move backward, and a failed final edit does not stop the monitor or advance
the stored floor.

The local `scripts/smoke-view-counter.js` proof measures 200 source BatchGet calls
for 20,000 IDs versus one 64-key aggregate call. It also creates a real source-only
crash gap. These are local request counts, not production latency or capacity
measurements. Main rollout and real cross-replica Sandbox proof remain held.
