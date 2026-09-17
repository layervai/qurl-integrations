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

`/qurl map` shares a location instead of a file: it takes a required `location`
(a Google Maps URL, or a place/address to search) in place of `attachment`, the
same `recipients` / `expires-in` / `self-destruct` / `personal-message` options,
and an optional `location-name` to override the label recipients see.

## Getting started

### 1. Add the bot to your server

Invite the qURL bot using the install link from your qURL operator. The bot
requests only four permissions: **View Channels**, **Send Messages**,
**Embed Links**, and **Use Application Commands**.

> On the multi-tenant public bot, slash commands can take up to an hour to
> appear the first time the bot joins a server, while Discord propagates the
> global command registration. Single-server deployments register per-guild,
> so commands appear right away.

### 2. Connect qURL (admin)

A server admin runs `/qurl setup` once and follows the prompts to connect this
server to its own qURL account — by authorizing qURL or entering an API key,
depending on the deployment. The key is stored **encrypted at rest** and scoped
to the server. Run `/qurl status` to confirm the connection.

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
| `DISCORD_CLIENT_ID` | Yes | Discord application client ID |
| `QURL_API_KEY` | `/qurl detect` | Requires `qurl:read` and `qurl:write` for detect; also the fallback for send operations without a server key from `/qurl setup`. |
| `QURL_ENDPOINT` | No | qURL API base URL (defaults to production; localhost in dev) |
| `CONNECTOR_URL` | No | qURL connector URL for file upload + serving |
| `BASE_URL` | OAuth setup | Public `https://` origin of the bot; required to complete the OAuth `/qurl setup` flow (defaults to `http://localhost:3000`). |
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

## Architecture

- **Multi-tenant by default** — the bot serves every server it's invited to.
  Each server connects its own qURL account via `/qurl setup`; keys are
  envelope-encrypted (AES-256-GCM) at rest in DynamoDB.
- **qURL API client** — creates one-time links and revokes them, with an
  SSRF guard on target URLs.
- **Connector** — uploads and serves shared files through the qURL connector
  behind an SSRF-guarded fetch.
- **HTTP surface** — `/health` for load-balancer probes, `/metrics` (bearer
  authenticated), and the OAuth callback that completes the `/qurl setup` flow.

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

## Sandbox application metadata

Use the LayerV-owned sandbox Discord application, not the previous personal
developer-portal app:

- Application ID: `1511450217789128885`
- Public Key: `f951fb4d407da2ac37ebb862f074e311d530b6e95940984695a320a1ac9f00ea`

1. https://discord.com/developers/applications → `qURL (sandbox)`
2. General Information → set the application name to **qURL (sandbox)**, upload
   `assets/discord-app-icon.png`, add the description from
   `discord-metadata.json`, and set the privacy/terms URLs listed there.
3. Bot → set the unique username to `qurl` (`bot.unique_username` in
   `discord-metadata.json`); the application/profile branding remains
   **qURL**. Upload `assets/discord-avatar.png`, and enable **Server Members
   Intent** under Privileged Gateway Intents.
4. Installation → default install settings should request `bot` and
   `applications.commands` with permissions `2147503104` (View Channels, Send
   Messages, Embed Links, Use Slash Commands).
5. Copy the bot token.

The repeatable metadata source of truth is `discord-metadata.json`. With a
target bot token in `DISCORD_TOKEN`, operators can apply the bot/app fields
that Discord exposes through API (`description`, `icon`, `cover_image`, tags,
install params, bot username, avatar, and banner). The script refuses to run if
the token belongs to any Discord application other than LayerV sandbox app
`1511450217789128885`.

`bot.username` is the cased **qURL** brand source only; Discord's bot-user API
receives `bot.unique_username` (`qurl`) as the lowercase unique username.

```bash
npm run apply-discord-metadata
```

Preview the API payload without making changes:

```bash
npm run apply-discord-metadata -- --dry-run
```

Dry-run also verifies that every asset referenced by `discord-metadata.json`
exists, can be read, stays under the local byte cap for its Discord surface,
and matches the local dimension rules.

Run the live apply as an operator step after seeding the LayerV-owned token; do
not wire it as an unconditional CI job until image/app PATCH idempotency lands
in https://github.com/layervai/qurl-integrations/issues/588. Username and
image updates are separate Discord `/users/@me` PATCHes, so respect
`retry_after` and rerun instead of looping if either edit is rate-limited.

After the live metadata apply, restart the bot with the same token/app pair
so automatic command registration updates Discord's slash-command picker. Keep `DISCORD_CLIENT_ID` aligned with the metadata application
ID; the script verifies the token's app, while command registration and invite
links read the client ID from environment/SSM/infra wiring.

Exit codes:
- `0` — API fields applied, including the lowercase unique username `qurl`;
  application/profile branding remains **qURL**.
- `1` — partial API apply. The script prints `retry_after` when Discord
  rate-limits username/avatar/banner updates, flags ignored avatar/banner
  writes, and keeps `1` if a portal action is also needed. The script does not
  sleep/retry automatically; wait for `retry_after` and rerun. For legacy
  case-only username drift, rerun after Discord reports `discriminator: "0"` to
  confirm the lowercase unique username converges; app and image fields may
  have applied while this exit remains non-zero.
- `2` — API writes completed, but the application name still differs from
  `discord-metadata.json`; update the name in Developer Portal.
- `3` — fatal application metadata PATCH failure. The application fields did
  not finish applying; inspect the Discord response, wait for `retry_after` if
  present, and rerun after fixing the API error.
- `4` — pre-flight token/app verification failed before writes. Fix the token,
  app ID, or public key mismatch before rerunning.

Application name and legal URLs are Developer Portal-only. Discord's
Edit Current Application API does not list legal URLs as writable fields, so
verify the privacy and terms links directly in Developer Portal after editing
them.

The live apply uses Discord v10's documented current-user fields
(`username`, `avatar`, `banner`) and current-application fields (`description`,
`icon`, `cover_image`, `tags`, `install_params`); an application PATCH failure
is fatal, while bot image failures are reported as partial applies.

