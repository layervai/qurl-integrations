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
| `/qurl status` | *(admin)* Check whether qURL is configured |

### `/qurl send` options

| Option | Required | Description |
|--------|----------|-------------|
| `attachment` | Yes | The file to share |
| `recipients` | No | Paste `@mentions`. Leave blank to pick from a menu. |
| `expires-in` | No | How long the links stay valid (default: 24 hours) |
| `self-destruct` | No | Countdown after the first open (default: no timer) |
| `personal-message` | No | A note included in each recipient's DM |

When `MAP_COMMAND_ENABLED=true`, `/qurl map` shares a location instead of a file: it takes a required `location`
(a Google Maps URL, or a place/address to search) in place of `attachment`, the
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
authoritative guild in Discord's token response. Grant only **View Channels**, **Send Messages**,
**Embed Links**, and **Use Application Commands** (permission bitfield
`2147503104`).

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

### 3. Share

```
/qurl send attachment:<file> recipients:@alice @bob
```

Each recipient receives a DM with a one-time link. Use `/qurl revoke` to
invalidate the links from any previous send.

> Recipients must allow direct messages from server members to receive their
> link.

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
| `QURL_API_KEY` | No | Optional fallback qURL API key. Each server normally connects its own key via `/qurl setup`. |
| `QURL_ENDPOINT` | No | qURL API base URL (defaults to production; localhost in dev) |
| `CONNECTOR_URL` | No | qURL connector URL for file upload + serving |
| `BASE_URL` | OAuth setup | Public `https://` origin of the bot; required to complete OAuth setup (defaults to `http://localhost:3000`). Local customer-install testing must use `localhost` or HTTPS because its `__Host-` session cookie is always `Secure`. |
| `KEY_ENCRYPTION_KEY` | Production | 32 random bytes, base64 — encrypts stored keys at rest |
| `METRICS_TOKEN` | Production | Bearer token guarding the `/metrics` endpoint |
| `MAP_COMMAND_ENABLED` | No | Set to `true` to enable `/qurl map` (default off) |
| `DETECT_COMMAND_ENABLED` | No | Set to `true` to enable `/qurl detect` (default off) |
| `DETECT_TUNNEL_SLUG` | `/qurl detect` | qURL tunnel resource slug used to mint short-lived `/api/detect` qURLs |
| `DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS` | No | Comma-separated extra non-prod `QURL_ENDPOINT` hosts for `/qurl detect` (extends the built-in set below) |
| `DETECT_EXTRA_NON_PROD_HOST_SUFFIXES` | No | Comma-separated extra `qurl_site` suffixes granted for the hosts above; each entry must start with `.` |
| `GOOGLE_MAPS_API_KEY` | `/qurl map` | Google Maps key for location autocomplete (needed when map is enabled) |
| `GUILD_ID` | No | Scope commands to a single server; unset runs the multi-tenant public bot |
| `PORT` | No | HTTP listen port (default 3000) |

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
tunnel namespace; it is not a tenant identity signal. The case-sensitive
`resource_id` returned by `resolve()` is checked against the slug-resolved
public key. A missing, empty, or mismatched `resource_id` fails closed before
the bot sends image bytes or its API-key Bearer to the tunnel host.

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
fails closed instead of guessing which tunnel should receive the Bearer-carrying
image POST. Persistent hard failures arm a short process-wide retry backoff for
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
