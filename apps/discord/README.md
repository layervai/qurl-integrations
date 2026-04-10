# QURL Discord Bot

Protect and share files on Discord through LayerV QURL. Users DM files to the bot, which uploads them to the QURL API and returns a resource ID. Owners then use slash commands to dispatch single-use, time-limited links to specific recipients via DM.

## Features

- **DM Upload:** Send a file to @QurlBot in DM to protect it and receive a resource ID
- **Slash Commands:** `/qurl send`, `/qurl list`, `/qurl status`, `/qurl revoke`, `/qurl help`
- **Owner-Only Dispatch:** Only the file owner can send links; guild-scoped
- **Per-Recipient Links:** Each recipient gets a unique, single-use, 15-minute link via DM
- **Input Validation:** File size (25 MB), file type (PNG, JPG, GIF, WebP, PDF), CDN allowlist, resource ID regex
- **Rate Limiting:** 5 requests per user per minute (sliding window)
- **SQLite Storage:** Persistent owner registry and dispatch audit log
- **Health Check:** HTTP endpoint at `:3000/health`

## Quick Start

### 1. Create a Discord App

1. Go to [Discord Developer Portal](https://discord.com/developers/applications) and create a new application
2. Under **Bot**, create a bot and copy the token
3. Under **Privileged Gateway Intents**, enable:
   - **Server Members Intent** (required for guild member verification)
   - Note: Message Content Intent is NOT required (dispatch uses slash commands)
4. Under **OAuth2 > URL Generator**, select `bot` + `applications.commands` scopes

### 2. Configure Environment

```bash
cp .env.example .env
# Edit .env with your credentials
```

### 3. Install and Run

```bash
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
python run.py
```

## Project Structure

```
qurl-bot-discord/
├── run.py                    # Entry point (bot + health server)
├── config.py                 # pydantic-settings configuration
├── db.py                     # SQLite owner registry + dispatch log
├── validation.py             # Input validation (files, IDs, CDN URLs)
├── rate_limiter.py           # Sliding window rate limiter
├── adapters/
│   └── discord_bot.py        # Discord slash commands + DM upload
├── services/
│   ├── upload_client.py      # QURL upload API client
│   └── mint_link_client.py   # QURL mint_link API client
├── terraform/                # EC2 infrastructure
├── docs/                     # Design specs
├── deploy.sh                 # Server deploy script
├── requirements.txt          # Pinned dependencies
└── .env.example              # Environment template
```

## Infrastructure

- **EC2** on Amazon Linux 2023, managed by Terraform
- **Outbound-only** networking (Discord WebSocket) — no HTTP/HTTPS ingress needed
- **systemd** service running as `ec2-user`
- **CI/CD** via GitHub Actions with OIDC authentication

## Usage

### Protect a File (DM)

1. DM any supported file to @QurlBot
2. Bot uploads it and replies with the resource ID and a link

### Share with Users (Server)

```
/qurl send resource_id:r_abc123def users:@alice @bob
```

Each mentioned user receives a unique, single-use link via DM.

### Other Commands

- `/qurl list` — list your protected files
- `/qurl status r_abc123def` — check dispatch stats
- `/qurl revoke r_abc123def` — revoke a file
- `/qurl help` — show help
