# qURL Discord Bot

Discord bot for qURL-powered secure resource sharing, plus GitHub OAuth
linking and auto Contributor-role assignment for community members.

## Features

- **`/qurl send`** — share a file as a one-time qURL link, delivered to each
  recipient via DM. Recipients picked via @mentions or a user-select menu.
- **`/qurl map`** — share a Google Maps location as a one-time qURL link,
  delivered to each recipient via DM.
- **`/qurl revoke`** — revoke all links from a previous send.
- **`/qurl help`** — command reference.
- **`/qurl setup`** / **`/qurl status`** — admin-only, configure the
  guild's qURL API key (stored AES-256-GCM encrypted at rest).
- **GitHub OAuth Linking**: `/link` verifies GitHub identity; the callback
  is session-cookie-bound to prevent leaked-URL takeover.
- **Auto Role Assignment**: merged PRs in allowed orgs award the
  `@Contributor` role automatically.
- **Contribution Tracking + Badges**: first PR, docs hero, bug hunter,
  on-fire, streak master, multi-repo — awarded on merged PRs.
- **Good-first-issue feed + release announcements + star milestones**
  in configurable channels.

## Commands

| Command | Description |
|---------|-------------|
| `/qurl send` | Send a file as one-time qURL links to picked recipients |
| `/qurl map` | Send a Google Maps location as one-time qURL links to picked recipients |
| `/qurl revoke` | Revoke links from a previous send |
| `/qurl help` | Usage reference |
| `/qurl setup` | *(admin)* Configure the guild's qURL API key |
| `/qurl status` | *(admin)* Check whether qURL is configured |
| `/link` | Link your GitHub account to Discord |
| `/unlink` | Unlink your GitHub account |
| `/whois [@user]` | Look up a member's GitHub handle |
| `/contributions [@user]` | Show a member's merged-PR count + badges |
| `/stats` | Bot-wide contribution statistics |
| `/leaderboard` | Top contributors |
| `/forcelink` | *(admin)* Manually link a Discord user to a GitHub username |
| `/bulklink` | *(admin)* Bulk-link from a `discordId:github,...` list |
| `/unlinked` | *(admin)* List contributors who haven't linked |
| `/backfill-milestones` | *(admin)* Re-announce star milestones |

## Setup

### Prerequisites

- **Node.js ≥ 22** (see `package.json` engines)
- The LayerV-owned Discord bot application
- A GitHub OAuth App
- A hosting target with a public HTTPS URL (ECS, Railway, Fly, etc.)
- A qURL API key from https://layerv.ai

### 1. Configure Discord

Use the LayerV-owned sandbox Discord application, not the previous personal
developer-portal app:

- Application ID: `1511450217789128885`
- Public Key: `f951fb4d407da2ac37ebb862f074e311d530b6e95940984695a320a1ac9f00ea`

1. https://discord.com/developers/applications → `qURL (sandbox)`
2. General Information → set the application name to **qURL (sandbox)**, upload
   `assets/discord-app-icon.png`, add the description from
   `discord-metadata.json`, and set the privacy/terms URLs listed there.
3. Bot → set the unique username to `qurl`; the application/profile branding
   remains **qURL**. Upload `assets/discord-avatar.png`, and enable **Server
   Members Intent** under Privileged Gateway Intents.
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
in https://github.com/layervai/qurl-integrations/issues/588.

After the live metadata apply, run `npm run register` with the same token/app
pair so Discord's slash-command picker receives the updated command
descriptions.

Exit codes:
- `0` — API fields applied, including the lowercase unique username `qurl`;
  application/profile branding remains **qURL**.
- `1` — partial API apply. The script prints `retry_after` when Discord
  rate-limits username/avatar/banner updates, flags ignored avatar/banner
  writes, and keeps `1` if a portal action is also needed. Wait for
  `retry_after` and rerun. For legacy case-only username drift, rerun after
  Discord reports `discriminator: "0"` to confirm the lowercase unique
  username converges; app and image fields may have applied while this exit
  remains non-zero.
- `2` — API writes completed, but the application name still differs from
  `discord-metadata.json`; update the name in Developer Portal.
- `3` — fatal application metadata PATCH failure. The application fields did
  not finish applying; inspect the Discord response, wait for `retry_after` if
  present, and rerun after fixing the API error.

Application name and legal URLs are Developer Portal-only. Discord's
current-application API does not return the legal URLs, so verify the privacy
and terms links directly in Developer Portal after editing them.

The live apply uses Discord v10's documented current-user fields
(`username`, `avatar`, `banner`) and current-application fields (`description`,
`icon`, `cover_image`, `tags`, `install_params`); an application PATCH failure
is fatal, while bot image failures are reported as partial applies.

### 2. Configure GitHub OAuth

1. https://github.com/settings/developers → New OAuth App
2. Callback URL: `https://YOUR_DOMAIN/auth/github/callback`
3. Copy Client ID + generate a Client Secret.

### 3. Configure environment

Copy `.env.example` to `.env` and fill in. Every variable is documented
inline; the sections below call out the non-obvious ones.

**Always required:**

- `DISCORD_TOKEN`, `DISCORD_CLIENT_ID`, `GUILD_ID`
- `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_WEBHOOK_SECRET`
- `BASE_URL` (must be `https://` in production)
- `ALLOWED_GITHUB_ORGS` (comma-separated GitHub org names)

**Required when `NODE_ENV=production`** (the process refuses to boot
without them, see `src/index.js`):

- `METRICS_TOKEN` — bearer token for `/metrics`
- `QURL_API_KEY` — default qURL key (individual guilds can override via
  `/qurl setup`)
- `KEY_ENCRYPTION_KEY` — 32 random bytes, base64. Generate with:
  ```
  node -e "console.log(require('crypto').randomBytes(32).toString('base64'))"
  ```

**Optional operational knobs:**

- `PORT` (default 3000)
- `ADMIN_USER_IDS` — comma-separated Discord IDs with access to
  `/forcelink`, `/bulklink`, `/backfill-milestones`, `/unlinked`
- `RATE_LIMIT_WINDOW_MS`, `RATE_LIMIT_MAX_REQUESTS` — OAuth + webhook
  per-IP rate limiter
- `QURL_SEND_MAX_RECIPIENTS`, `QURL_SEND_COOLDOWN_MS`
- `PENDING_LINK_EXPIRY_MINUTES` — OAuth state TTL (default 10 min)
- `WEEKLY_DIGEST_CRON`, `WELCOME_DM_ENABLED`, `LOG_LEVEL`
- `CONTRIBUTOR_ROLE_NAME`, `GENERAL_CHANNEL_NAME`, etc.

### 4. Configure GitHub webhook

On each repo you want to track:

1. Repo → Settings → Webhooks → Add webhook
2. Payload URL: `https://YOUR_DOMAIN/webhook/github`
3. Content type: `application/json`
4. Secret: **required** — same value as `GITHUB_WEBHOOK_SECRET`
5. Events: Pull requests, Issues, Releases, Stars (use "Let me select").

### 5. Run

Local dev needs a DynamoDB-Local container — the bot has no in-process
data store, so the SDK has to reach a real DDB endpoint somewhere. The
`docker-compose.yml` here spins up `amazon/dynamodb-local` on port 8000;
the one-shot provisioner creates every table `ddb-store` expects.

```bash
npm ci                                  # provisioner needs @aws-sdk/client-dynamodb
docker compose up -d dynamodb-local
node scripts/provision-ddb-local.js     # idempotent, re-run after every `compose up`
DDB_TEST_ENDPOINT=http://localhost:8000 \
  DDB_TABLE_PREFIX=qurl-bot-discord-local- \
  AWS_REGION=us-east-1 \
  AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local \
  npm start
```

(The fake AWS creds keep the SDK happy without provisioning real IAM.
`DDB_TEST_ENDPOINT` is the env-var hook `ddb-store.js` already supports
for re-pointing the SDK at a local endpoint. Use the same
`DDB_TABLE_PREFIX` for the provisioner and `npm start` — the
provisioner defaults to `qurl-bot-discord-local-` if unset, and a
mismatched prefix lands `npm start` against tables that don't exist.)

The DDB-Local container runs `-inMemory`, so a `docker compose down`
flushes every table. Re-run `node scripts/provision-ddb-local.js`
after each fresh `docker compose up` (the provisioner is idempotent
on existing tables, so a re-run against the same container is a
no-op — but a new container starts empty and needs the create pass).
For sticky local data across restarts, drop `-inMemory` and add
`-dbPath ./data` to the compose command (see the `docker-compose.yml`
header comment).

`npm test` does NOT need DDB Local — every test mocks the AWS SDK via
`aws-sdk-client-mock`. The local-dev workflow is only required for
`npm start`.

The provisioner covers the **Store-contract** tables (those in
`src/store/ddb-store.js`'s `TABLES` map). Other modules use their own
dedicated tables that this script does NOT create — `flow-state`,
`gateway-session`, `gateway-lock`, `gateway-peer-heartbeat`. Running
locally with `ENABLE_EVENT_SHIPPER=true` or `ENABLE_GATEWAY_RESUME=true`
will hit `ResourceNotFoundException` on these unless you also provision
them via terraform-against-localhost or `aws dynamodb create-table
--endpoint-url http://localhost:8000`.

Linux note: `host.docker.internal` only resolves inside Docker
Desktop. If you're running Docker Engine on bare Linux, either start
the container with `--add-host=host.docker.internal:host-gateway`
or use `127.0.0.1` from the container side (and bind the
docker-compose `dynamodb-local` port to the host loopback rather
than to the container's network).

## Architecture

- `src/index.js` — boot validation + graceful shutdown
- `src/commands.js` — all slash-command handlers (split tracked in #55)
- `src/discord.js` — discord.js client + role/channel cache
- `src/store/` — DynamoDB-backed data layer (encrypted guild keys + per-table CRUD)
- `src/connector.js` — qurl-s3-connector client (SSRF-guarded CDN fetch)
- `src/qurl.js` — qURL API client (private-IP blocklist on target URLs)
- `src/routes/oauth.js` — GitHub OAuth (atomic state consumption,
  session-cookie binding, retry + background revoke sweeper)
- `src/routes/webhooks.js` — GitHub HMAC-verified webhooks, per-IP
  bad-signature rate limit
- `src/utils/crypto.js` — AES-256-GCM envelope encryption (versioned)
- `src/utils/sanitize.js` — filename + Discord-markdown escaping
- `src/orphan-token-sweeper.js` — hourly retry-revoke for failed OAuth
  token revocations; purges after 7 days
- `src/templates/page.js` — HTML templates with strict CSP + escapeHtml

## Local Development

```bash
cp .env.example .env
# For local dev: leave KEY_ENCRYPTION_KEY unset (stores plaintext with a
# loud warning). QURL_ENDPOINT/CONNECTOR_URL auto-default to localhost
# when NODE_ENV != production.
npm ci
npm run dev   # node --watch
```

Useful scripts:

- `npm test` — jest (78/68/78/78 coverage threshold)
- `npm run lint` — ESLint with `--max-warnings 0`
- `npm run register` — register slash commands with Discord

## Troubleshooting

**OAuth "Invalid Session"** — the callback requires the same browser
that opened `/auth/github`. Cleared cookies or switched browsers? Run
`/link` again.

**Webhook not triggering** — verify the webhook URL, content type, and
that `GITHUB_WEBHOOK_SECRET` matches GitHub's setting. Failed signatures
are logged at error level.

**Role not assigned** — the bot role must sit above `@Contributor` in
the role hierarchy, and the bot needs `Manage Roles`.

## License

Apache-2.0
