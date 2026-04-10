# Qurl Discord Bot — Design Document

## 1. Overview

Qurl Bot is a Discord bot developed by LayerV that protects files shared between users by wrapping them in secure, time-limited, single-use QURL links. There are two primary flows:

1. **DM Upload Flow** — A file owner DMs a file directly to Qurl Bot. The bot uploads the file to `getqurllink.layerv.ai`, which stores the file in S3, registers it as a LayerV resource with `fileviewer.layerv.ai` as the access-controlled target, and returns a `resource_id` and `qurl_link`. The bot forwards both to the owner via DM. The owner can then share the `qurl_link` directly with anyone, or use the bot to dispatch per-recipient links.

2. **Group Chat Dispatch Flow** — In a guild channel, a user @mentions Qurl Bot and includes a `resource_id` in the message (see §4.2). The bot does **not** verify resource ownership. It requests minted links from the configured mint-link service (`POST {MINT_LINK_API_URL}/{resource_id}` with `n` equal to the number of DM targets), maps each link to a mentioned user in order, and sends each link by DM. If no users are mentioned, the author receives a single link. No QURL is posted in the channel.

**Key properties:**
- **Owner-controlled distribution** — only the user who originally uploaded the file can trigger link dispatch
- **Per-recipient links** — every `@mentioned` user receives their own unique, single-use QURL; no shared links, no replay attacks
- **Invisible delivery** — links are always delivered by DM; they never appear in public or shared channels
- **Stable file storage** — files are stored in S3 via `getqurllink.layerv.ai`, avoiding Discord CDN URL expiry
- **Access-controlled viewing** — `fileviewer.layerv.ai` is the protected target; recipients access the file through the viewer only after their QURL resolves
- **Self-destructing links** — once a link is clicked it is consumed and ceases to exist

---

## 2. User Scenario

### 2.1 The Treasure Hunting Community

Competitive treasure hunting groups on Discord coordinate in real time, sharing clues in the form of images, hand-drawn maps, annotated PDFs, and cipher sheets. These materials are sensitive by nature — a clue image leaked to the wrong team, cached on Discord's servers, or forwarded by a recipient gives an unfair advantage and undermines the hunt.

The standard Discord workflow has two fundamental problems for this use case:

1. **Permanence** — files uploaded to Discord channels or DMs are stored indefinitely on Discord's CDN. Any participant (or anyone who later gains access to the channel's history) can re-download the file long after it was intended to be seen.
2. **Cacheability** — Discord generates persistent CDN URLs for attachments. These URLs can be copied, forwarded, indexed, or cached by third-party tools, browser history, and Discord's own servers, entirely outside the sender's control.

**How Qurl Bot solves this:**

A hunt organiser uploads a clue image by DMing it directly to Qurl Bot. The file is stored securely in LayerV's S3 storage — never on Discord's CDN — and wrapped in QURL access control. The organiser receives a `resource_id` and a `qurl_link` back in DM.

When it is time to release the clue to a specific team, the organiser types `@QurlBot #r_abc123def @alice @bob` in the group channel. Each named player receives their own unique, single-use QURL link by DM. When a player clicks the link, the clue renders in `fileviewer.layerv.ai` in their browser — and the link is immediately consumed and destroyed. There is nothing left to forward, cache, or replay.

**A concrete hunt round looks like this:**

> 1. Organiser DMs the clue PDF to Qurl Bot → receives `resource_id: r_clue04` and an owner preview link.
> 2. Round begins. Organiser posts in `#hunt-channel`: `@QurlBot #r_clue04 @team-alpha`.
> 3. Each member of `@team-alpha` receives a private DM with their own one-time link. No clue appears in the channel.
> 4. Players open their links, view the PDF in the browser. Each player's view is watermarked with their unique minted link ID — if anyone screenshots the clue and shares it, the leak can be traced back to the exact recipient. Links self-destruct on access.
> 5. Ten minutes later, latecomers or rival teams who try the link — or attempt to find it in Discord history — find nothing. The clue has vanished.
> 6. When the QURL link expires, the underlying file is permanently and irrecoverably destroyed from S3 storage — leaving no copy anywhere in the system.

This model gives hunt organisers precise, per-player control over who sees a clue, when, and exactly once — without any file ever touching Discord's permanent storage. When the QURL link expires, the underlying file is permanently and irrecoverably destroyed from S3 storage — leaving no copy anywhere in the system.

---

## 3. Architecture

### 3.1 Actors

| Actor | Role |
|---|---|
| **File Owner** | Discord user who DMs a file to Qurl Bot to register it as a protected resource |
| **Recipient** | Discord user who receives a one-time QURL link, either directly from the owner or dispatched by the bot |
| **Qurl Bot** | Discord app (**Python** / **discord.py**). Handles DM file uploads, resource registration, and group chat link dispatch |
| **Discord API** | Used by the bot to receive file attachments, verify guild membership, and send DMs |
| **getqurllink.layerv.ai** | LayerV upload service. Accepts a file, stores it in S3, registers `fileviewer.layerv.ai/{file_id}` as a QURL resource, and returns `resource_id` + `qurl_link` |
| **fileviewer.layerv.ai** | LayerV file viewer. Renders files (PDFs, images, etc.) in the browser; sits behind QURL access control — only reachable via a valid QURL token |
| **LayerV QURL API** | Existing Go service at `api.layerv.ai`. Used by the bot to mint additional per-recipient links via `POST /v1/qurls/{resource_id}/mint_link` |
| **Owner Registry** | Persisted map of `resource_id → discord_user_id`, maintained by the bot to enforce ownership on dispatch |

### 3.2 Service Responsibilities

```
┌─────────────────────────────────────────────────────────────┐
│                      Qurl Bot                               │
│  DM Upload ──► getqurllink.layerv.ai ──► resource_id        │
│                      + qurl_link (owner copy)               │
│                                                             │
│  Dispatch  ──► api.layerv.ai/mint_link ──► per-user links   │
│               (one call per @mentioned user)                │
└─────────────────────────────────────────────────────────────┘

Recipient clicks qurl_link
  → QURL resolves token
  → opens fileviewer.layerv.ai/{file_id}
  → token consumed (one-time-use)
```

### 3.3 Flow 1 — DM Upload

```
File Owner      Qurl Bot          getqurllink.layerv.ai
     |               |                      |
     |--DM: file---->|                      |
     |               |                      |
     |               |--POST /upload------->|
     |               |                      |
     |               |    [store file in S3]|
     |               |    [register resource]
     |               |<--{ resource_id,     |
     |               |    qurl_link,        |
     |               |    expires_at }------|
     |               |                      |
     |               | [store resource_id → discord_user_id]
     |               |                      |
     |<--DM: resource_id + qurl_link + expires_at + instructions
```

### 3.4 Flow 2 — Group Chat Dispatch

```
File Owner      Qurl Bot         Discord API     api.layerv.ai
     |               |                |                |
     |--@QurlBot #r_abc @alice @bob-->|                |
     |               |                |                |
     |               | [verify sender == owner of r_abc]
     |               |                |                |
     |               | for each @mentioned user:        |
     |               |--POST /v1/qurls/r_abc/mint_link->|
     |               |  { expires_in: "15m",            |
     |               |    one_time_use: true,            |
     |               |    label: "discord:user_id" }     |
     |               |<--{ qurl_link, expires_at }-------|
     |               |                |                |
     |               |--DM qurl_link to @alice          |
     |               |--DM qurl_link to @bob            |
     |               |                |                |
     |<--ACK (channel): "Links sent to 2 recipients"
```

---

## 4. Bot Commands & Triggers

### 4.1 DM Upload Trigger

**Trigger:** User sends a DM directly to Qurl Bot containing a file attachment. No slash command needed — the attachment in a DM is the sole trigger.

**Bot DM response:**
```
✅ Your file has been protected!

Resource ID:  r_abc123def
Link:         https://qurl.link/at_xyz789abc
Expires:      in 7 days

Share this link directly with one person, or send
individual links to multiple users in a server:

  @QurlBot #r_abc123def @user1 @user2 ...
```

### 4.2 Group Chat Dispatch Command

**Trigger:** In a guild channel, message matching:
```
@QurlBot #<resource_id> @user1 [@user2 ...]
```

**Example:**
```
@QurlBot #r_abc123def @alice @bob @charlie
```

**Bot channel ACK (visible to all in channel):**
```
Links dispatched to 3 users.
```

**Recipient DM:**
```
🔗 You've been granted access to a file.

https://qurl.link/at_def456ghi

Single use · expires in 15 minutes
```

**Owner DM:**
```
✅ Dispatch complete for #r_abc123def

• @alice — sent
• @bob — sent
• @charlie — ❌ DMs disabled, could not reach
```

---

## 5. getqurllink.layerv.ai

Handles the full upload-and-register pipeline. The bot sends a `multipart/form-data` POST with the raw file bytes and metadata; the service stores the file in S3, registers `fileviewer.layerv.ai/{file_id}` as the protected `target_url` on the LayerV QURL API, and returns a ready-to-use `resource_id` and `qurl_link`. The bot never constructs or handles the viewer URL directly.

**Endpoint:** `POST https://getqurllink.layerv.ai/upload`
**Auth:** `Authorization: Bearer <QURL_API_KEY>`
**Request:** `multipart/form-data`

| Field | Type | Description |
|---|---|---|
| `file` | binary | Raw file bytes fetched from Discord CDN |
| `filename` | string | Original filename from the Discord attachment |
| `owner_label` | string | `discord:<user_id>` — for audit trail |
| `content_type` | string | MIME type of the file |

**Response:**
```json
{
  "resource_id": "r_abc123def",
  "qurl_link":   "https://qurl.link/at_xyz789abc",
  "expires_at":  "2026-12-31T23:59:59Z"
}
```

**File lifecycle:** When the QURL link expires, `getqurllink.layerv.ai` permanently destroys the underlying file from S3 storage. No copy remains anywhere in the system after expiry.

---

## 6. fileviewer.layerv.ai

A browser-based file viewer that renders protected files behind QURL access control. Recipients reach it only by resolving a valid QURL token — the viewer is not directly accessible by URL. The bot has no direct interaction with this service.

**Access control:** `fileviewer.layerv.ai` has no public address and is not reachable directly from the internet — the only path in is through the QURL resolver and AC proxy chain. Direct access attempts are rejected before they reach the viewer.

```
Public internet
─────────────────────────────────────────────────────────────────
Recipient ──qurl_link──► QURL resolver ──token valid──► AC proxy
    │                         │                              │
    │                    invalid → rejected                  │
    │                                                        │
    │        ┌─ Private — not routable ─────────────────────┼──┐
    │        │                                               │  │
    │   ✕ no direct access                   ┌──────────────▼──┤
    │        │                               │ fileviewer        │
    │        │                               │ .layerv.ai        │
    │        │                               │ renders +         │
    │        │                               │ watermarks        │
    │        │                               └──────┬───────────┘
    │        │                                      │ fetch
    │        │                               S3 storage
    │        └──────────────────────────────────────────────────┘
    │
    └◄── rendered + watermarked view (via proxy)
```

**Watermarking:** When rendering a file, `fileviewer.layerv.ai` overlays the minted link ID (e.g. `at_def456ghi`) as a visible watermark on the rendered output. Because each recipient receives their own individually minted link with a unique ID, the watermark is unique per recipient. If a recipient takes a screenshot and the image circulates, the minted link ID in the watermark can be traced back to the exact recipient who leaked it — providing both a deterrent and a forensic audit trail.

**Supported file types:** Images (PNG, JPG, GIF, WebP) and PDF. Additional file types will be supported in future releases.

---

## 7. LayerV QURL API

### 7.1 mint_link

Used exclusively in the dispatch flow to mint per-recipient links against an already-registered resource.

- **Endpoint:** `POST https://api.layerv.ai/v1/qurls/{resource_id}/mint_link`
- **Auth:** `Authorization: Bearer <QURL_API_KEY>`
- **Request body:**

```json
{
  "expires_in":   "15m",
  "one_time_use": true,
  "label":        "discord:RECIPIENT_USER_ID"
}
```

- **Response:**

```json
{
  "data": {
    "qurl_link":  "https://qurl.link/at_def456ghi",
    "expires_at": "2026-12-31T23:59:59Z"
  }
}
```

---

## 8. Components

### 8.1 Qurl Bot (Discord side)

- **Runtime:** Python 3
- **Framework:** discord.py
- **Triggers:**
  - `on_message` on DM channel with attachment → Upload Flow
  - `on_message` on guild channel with bot mention and parsable `resource_id` → Dispatch Flow (attachments in guild are rejected)
  - Slash `interaction` handlers exist in code but no commands are registered in the default configuration

**Discord Gateway Intents (required):**
- `Guilds`
- `GuildMessages`
- `MessageContent` ⚠️ — privileged intent; must be enabled in Discord Developer Portal
- `DirectMessages`

**OAuth2 bot permissions:**
- `Send Messages`
- `Send Messages in Threads`
- `Read Message History`
- `Use Slash Commands`

### 8.2 Owner Registry

```javascript
// Production: replace with Redis or Postgres
const ownerRegistry = new Map();
// ownerRegistry.set('r_abc123def', '123456789012345678');
```

---

## 9. Environment Variables

| Variable | Description |
|---|---|
| `DISCORD_BOT_TOKEN` | Bot OAuth token. Used only for Discord API calls — never transmitted to LayerV |
| `DISCORD_CLIENT_ID` | Application client ID, used for slash command registration at startup |
| `QURL_API_KEY` | LayerV API key (`lv_live_...`). Used to authenticate with both `getqurllink.layerv.ai` and `api.layerv.ai` |
| `PORT` | HTTP port for health-check endpoint (default: `3000`) |

---

## 10. Source excerpts (`adapters/discord_bot.py` and clients)

```python
class QURLDiscordBot(discord.Client):
    def __init__(self, *, intents: discord.Intents):
        super().__init__(intents=intents)
        self.tree = app_commands.CommandTree(self)

    async def setup_hook(self):
        await self.tree.sync()

    async def on_message(self, message: discord.Message):
        if message.author.bot:
            return
        is_dm = isinstance(message.channel, discord.DMChannel)
        is_mention = self.user and self.user.mentioned_in(message)
        if not (is_dm or is_mention):
            return
        if not is_dm:
            if message.attachments:
                await message.channel.send(upload_channel_disabled_message)
                return
            if _extract_resource_id(preprocess_text(message.content or "", PLATFORM_DISCORD)):
                await _handle_mint_link(self, message)
                return
            await message.channel.send(mint_link_prompt_message)
            return
        if message.attachments:
            if settings.upload_api_url:
                await _handle_file_upload(self, message, is_dm=True)
            return
        await message.channel.send(upload_only_prompt_message)


def run_discord_bot():
    intents = discord.Intents.default()
    intents.message_content = True
    intents.dm_messages = True
    bot = QURLDiscordBot(intents=intents)
    bot.run(settings.discord_token)
```

```python
# services/mint_link_client.py
async def mint_links(resource_id: str, n: int = 1, expires_at: str | None = None) -> MintLinkResult:
    base = (settings.mint_link_api_url or "").rstrip("/")
    url = f"{base}/{resource_id}"
    payload = {}
    if n > 1:
        payload["n"] = min(max(n, 1), 10)
    if expires_at:
        payload["expires_at"] = expires_at
    async with httpx.AsyncClient(timeout=30.0, verify=False) as client:
        resp = await client.post(url, json=payload or {})
```

```python
# services/upload_client.py
async def upload_file(file_bytes: bytes, filename: str, content_type: str | None = None) -> UploadResult:
    base = (settings.upload_api_url or "").rstrip("/")
    url = f"{base}/api/upload"
    files = {"file": (filename, file_bytes, content_type or "application/octet-stream")}
    async with httpx.AsyncClient(timeout=60.0) as client:
        resp = await client.post(url, files=files)
```

---

## 11. Data Flow Summary

```
Owner DMs file
  │
  ▼
Qurl Bot fetches file bytes from Discord CDN
  │
  ▼
POST getqurllink.layerv.ai/upload
  │  (stores file in S3, registers fileviewer.layerv.ai/{id} as resource)
  ▼
← { resource_id, qurl_link, expires_at }
  │
  ├─ Bot stores resource_id → owner in registry
  └─ Bot DMs owner: resource_id + qurl_link

Owner dispatches in channel: @QurlBot #r_abc @alice @bob
  │
  ▼
Bot verifies ownership
  │
  ├─ POST api.layerv.ai/v1/qurls/r_abc/mint_link  (for @alice)
  │    ← { qurl_link_alice }  →  DM to @alice
  │
  └─ POST api.layerv.ai/v1/qurls/r_abc/mint_link  (for @bob)
       ← { qurl_link_bob }    →  DM to @bob

Recipient clicks link
  │
  ▼
QURL resolves token → opens fileviewer.layerv.ai/{file_id}
  → token consumed (one-time-use)
  → file permanently deleted from S3 on expiry
```

---

## 12. Security Notes

- **Bot token never transmitted to LayerV** — used only for Discord API calls within the bot process
- **QURL_API_KEY authenticates the bot to LayerV** — no shared secrets, no HMAC, no cross-language contracts
- **Owner-only dispatch** — the `ownerRegistry` ensures no user can dispatch links for a resource they did not register
- **Per-recipient single-use links** — each `@mention` triggers a separate `mint_link` call; links cannot be reused or forwarded for second access
- **Links never in channels** — all QURL links are delivered exclusively by DM
- **Watermark audit trail** — every minted link carries a unique ID watermarked on the rendered view; leaks are traceable to the exact recipient
- **Full API audit trail** — every minted link carries `label: discord:<user_id>` for per-user attribution in the LayerV dashboard
- **File destruction on expiry** — underlying S3 file is permanently deleted when the QURL expires
- **Rate limiting** — 5 requests/user/minute on both flows

---

## 13. Production Considerations

### 13.1 Owner Registry Persistence
The in-memory `ownerRegistry` is lost on restart. Persist to Redis or Postgres keyed by `resource_id`, and repopulate from storage on bot startup.

### 13.2 True Ephemeral Channel Replies
`msg.reply()` in a guild channel is visible to everyone. Migrate the dispatch trigger to a slash command (`/qurl dispatch`) for true `{ ephemeral: true }` replies.

### 13.3 Rate Limiting at Scale
Replace the in-memory rate limiter with a Redis-backed sliding window for multi-instance production deployments.

### 13.4 Discord Gateway Intents
`MessageContent` is a privileged intent. It must be explicitly enabled in the Discord Developer Portal under **Bot → Privileged Gateway Intents**, or the dispatch trigger will silently never fire.

---

## 14. Discord Developer Portal Setup

1. Create a new application at https://discord.com/developers/applications
2. Under **Bot**: enable **Message Content Intent** and **Server Members Intent**
3. Under **OAuth2 → URL Generator**: select scopes `bot` + `applications.commands`; select bot permissions: `Send Messages`, `Read Message History`, `Use Slash Commands`
4. Invite the bot to your guild using the generated URL
