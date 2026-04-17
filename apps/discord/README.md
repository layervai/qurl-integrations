# OpenNHP Discord Bot

Automatically assigns the **@Contributor** role when community members get PRs merged to OpenNHP repositories.

## Features

- **GitHub OAuth Linking**: Users verify their GitHub identity via `/link` command
- **Auto Role Assignment**: Automatically assigns @Contributor when PRs are merged
- **PR Notifications**: Announces merged PRs and prompts unlinked users to connect
- **Contribution Tracking**: Records all contributions per user

## Commands

| Command | Description |
|---------|-------------|
| `/link` | Link your GitHub account |
| `/unlink` | Unlink your GitHub account |
| `/whois [@user]` | Check GitHub link for a user |
| `/stats` | Show bot statistics |

## Setup Guide

### Prerequisites

- Node.js 18+
- A Discord bot (you already have one: `Config Bot`)
- A GitHub OAuth App (we'll create this)
- Somewhere to host the bot (Railway, Fly.io, etc.)

---

### Step 1: Create GitHub OAuth App

1. Go to https://github.com/settings/developers
2. Click **"New OAuth App"**
3. Fill in:
   - **Application name**: `OpenNHP Discord Bot`
   - **Homepage URL**: `https://github.com/OpenNHP`
   - **Authorization callback URL**: `https://YOUR_DOMAIN/auth/github/callback`
     - (Use your Railway/Fly.io URL once deployed)
4. Click **"Register application"**
5. Copy the **Client ID**
6. Generate a **Client Secret** and copy it

---

### Step 2: Configure Discord Bot

Your existing bot needs a few more permissions.

1. Go to https://discord.com/developers/applications
2. Select your bot application
3. Go to **Bot** section
4. Under **Privileged Gateway Intents**, enable:
   - **Server Members Intent** ✓
5. Save changes

---

### Step 3: Deploy to Railway

1. Push this `opennhp-bot` folder to a GitHub repo

2. Go to https://railway.app and sign in with GitHub

3. Click **"New Project"** → **"Deploy from GitHub repo"**

4. Select your repository

5. Once deployed, go to **Settings** → **Networking** → **Generate Domain**
   - Copy your domain (e.g., `opennhp-bot-production.up.railway.app`)

6. Go to **Variables** and add:

   ```
   DISCORD_TOKEN=your_discord_bot_token
   DISCORD_CLIENT_ID=your_discord_app_client_id
   GUILD_ID=your_discord_guild_id
   GITHUB_CLIENT_ID=your_github_oauth_client_id
   GITHUB_CLIENT_SECRET=your_github_oauth_client_secret
   BASE_URL=https://your-app.up.railway.app
   ```

7. Railway will auto-redeploy with the new variables

---

### Step 4: Update GitHub OAuth Callback URL

1. Go back to your GitHub OAuth App settings
2. Update **Authorization callback URL** to:
   ```
   https://your-app.up.railway.app/auth/github/callback
   ```

---

### Step 5: Add GitHub Webhook

For **each** OpenNHP repository you want to track:

1. Go to the repo → **Settings** → **Webhooks** → **Add webhook**

2. Configure:
   - **Payload URL**: `https://your-app.up.railway.app/webhook/github`
   - **Content type**: `application/json`
   - **Secret**: (optional, but recommended - add same value to `GITHUB_WEBHOOK_SECRET` env var)
   - **Events**: Select **"Let me select individual events"**
     - Check only: **Pull requests**
   - Click **"Add webhook"**

3. Repeat for each repo:
   - `OpenNHP/opennhp`
   - `OpenNHP/StealthDNS`
   - `OpenNHP/jsDemo`
   - `OpenNHP/ietf-rfc-nhp`

---

### Step 6: Test It!

1. In Discord, type `/link`
2. Click the link to authorize with GitHub
3. You should see a success page and receive a DM

To test the full flow:
1. Have a linked user create and merge a PR
2. The bot should auto-assign @Contributor and post in #general

---

## Local Development

```bash
# Clone and install
cd opennhp-bot
npm install

# Copy environment template
cp .env.example .env

# Edit .env with your values
# For local dev, use ngrok for BASE_URL:
# ngrok http 3000
# Then use the ngrok URL

# Start the bot
npm run dev
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     OpenNHP Discord Bot                      │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  Discord                    Web Server                       │
│  ┌──────────────┐          ┌──────────────────────────────┐ │
│  │ Slash Cmds   │          │ GET  /auth/github           │ │
│  │ /link        │          │ GET  /auth/github/callback  │ │
│  │ /unlink      │          │ POST /webhook/github        │ │
│  │ /whois       │          └──────────────────────────────┘ │
│  │ /stats       │                      │                    │
│  └──────────────┘                      │                    │
│         │                              │                    │
│         └──────────┬───────────────────┘                    │
│                    │                                        │
│                    ▼                                        │
│           ┌──────────────┐                                  │
│           │   SQLite DB  │                                  │
│           │  - links     │                                  │
│           │  - contribs  │                                  │
│           └──────────────┘                                  │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DISCORD_TOKEN` | Yes | Discord bot token |
| `DISCORD_CLIENT_ID` | Yes | Discord application client ID |
| `GUILD_ID` | Yes | Discord server ID |
| `GITHUB_CLIENT_ID` | Yes | GitHub OAuth app client ID |
| `GITHUB_CLIENT_SECRET` | Yes | GitHub OAuth app client secret |
| `BASE_URL` | Yes | Public URL of the bot (e.g., Railway domain) |
| `PORT` | No | Web server port (default: 3000) |
| `GITHUB_WEBHOOK_SECRET` | No | Webhook secret for signature verification |
| `DATABASE_PATH` | No | SQLite database path |
| `CONTRIBUTOR_ROLE_NAME` | No | Role to assign (default: "Contributor") |
| `GENERAL_CHANNEL_NAME` | No | Channel for announcements (default: "general") |

---

## Troubleshooting

**Bot not responding to commands**
- Check that slash commands are registered (check logs on startup)
- Verify bot has proper permissions in the server

**OAuth "Invalid state" error**
- Link expired (10 min timeout) - use `/link` again
- Multiple tabs open - close all and try again

**Webhook not triggering**
- Verify webhook URL is correct
- Check GitHub webhook delivery logs for errors
- Ensure "Pull requests" event is selected

**Role not being assigned**
- Verify bot role is higher than @Contributor role
- Check bot has "Manage Roles" permission

---

## License

Apache-2.0 - Same as OpenNHP
