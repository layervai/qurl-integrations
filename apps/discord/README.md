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
| `QURL_API_KEY` | No | Optional fallback qURL API key. Each server normally connects its own key via `/qurl setup`. |
| `QURL_ENDPOINT` | No | qURL API base URL (defaults to production; localhost in dev) |
| `CONNECTOR_URL` | No | qURL connector URL for file upload + serving |
| `BASE_URL` | OAuth setup | Public `https://` origin of the bot; required to complete the OAuth `/qurl setup` flow (defaults to `http://localhost:3000`). |
| `KEY_ENCRYPTION_KEY` | Production | 32 random bytes, base64 — encrypts stored keys at rest |
| `METRICS_TOKEN` | Production | Bearer token guarding the `/metrics` endpoint |
| `MAP_COMMAND_ENABLED` | No | Set to `true` to enable `/qurl map` (default off) |
| `GOOGLE_MAPS_API_KEY` | `/qurl map` | Google Maps key for location autocomplete (needed when map is enabled) |
| `GUILD_ID` | No | Scope commands to a single server; unset runs the multi-tenant public bot |
| `PORT` | No | HTTP listen port (default 3000) |

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
