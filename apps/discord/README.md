# qURL Discord Bot

Discord bot for qURL-powered secure resource sharing, plus GitHub OAuth
linking and auto Contributor-role assignment for community members.

## Features

- **`/qurl send`** — share a file or Google Maps location as a one-time qURL
  link, delivered to each recipient via DM. Targets: a specific user, the
  visible members of the current text channel, or your voice channel.
- **`/qurl revoke`** — revoke all links from a previous `/qurl send`.
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
| `/qurl send` | Send a file or location as one-time qURL links to a user / channel / voice-channel |
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
- A Discord bot application
- A GitHub OAuth App
- A hosting target with a public HTTPS URL (ECS, Railway, Fly, etc.)
- A qURL API key from https://layerv.ai

### 1. Configure Discord

1. https://discord.com/developers/applications → your bot
2. Enable **Server Members Intent** under Bot → Privileged Gateway Intents.
3. Copy the bot token.

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
- `DATABASE_PATH` (default `./data/opennhp-bot.db`, kept for legacy EFS
  mounts — override for new deployments)
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

```bash
npm ci
npm start
```

## Architecture

- `src/index.js` — boot validation + graceful shutdown
- `src/commands.js` — all slash-command handlers (split tracked in #55)
- `src/discord.js` — discord.js client + role/channel cache
- `src/database.js` — SQLite (better-sqlite3, WAL) + encrypted guild keys
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

- `npm test` — jest (80/70/80/80 coverage threshold)
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
