# Qurl Discord 机器人 — 设计文档

## 1. 概述

Qurl Bot 是 LayerV 开发的 Discord 机器人，通过将文件包装为安全、限时、单次使用的 QURL 链接来保护用户之间共享的文件。主要有两条流程：

1. **私信上传流程** — 文件所有者直接向 Qurl Bot 私信发送文件。机器人将文件上传到 `getqurllink.layerv.ai`，该服务把文件存入 S3，将其注册为 LayerV 资源并以 `fileviewer.layerv.ai` 作为受控访问目标，然后返回 `resource_id` 与 `qurl_link`。机器人通过私信把这两项转发给所有者。所有者可以直接把 `qurl_link` 分享给他人，也可以通过机器人为每位收件人单独分发链接。

2. **群聊分发流程** — 在服务器频道中，用户 @提及 Qurl Bot，并在消息中包含 `resource_id`（见 §4.2）。机器人**不**校验资源所有者。它向已配置的 mint-link 服务请求链接（`POST {MINT_LINK_API_URL}/{resource_id}`，其中 `n` 等于私信目标人数），按顺序将每条链接对应到被 @ 的用户，并通过私信发送。若未 @ 任何用户，则仅向发送者发送一条链接。频道内不会公开发布 QURL。

**关键特性：**
- **由所有者控制分发** — 只有最初上传文件的用户可以触发链接分发
- **按收件人独立链接** — 每位被 `@提及` 的用户获得各自唯一、单次使用的 QURL；无共享链接，无重放攻击
- **不可见投递** — 链接始终通过私信送达；不会出现在公开或共享频道中
- **稳定文件存储** — 文件经 `getqurllink.layerv.ai` 存入 S3，避免 Discord CDN 链接过期问题
- **受控访问浏览** — `fileviewer.layerv.ai` 为受保护目标；收件人仅在 QURL 解析通过后通过 viewer 访问文件
- **链接自毁** — 链接一旦被点击即被消费并不再可用

---

## 2. 用户场景

### 2.1 寻宝社群

竞技类寻宝小队在 Discord 上实时协作，以图片、手绘地图、带批注的 PDF、密码表等形式分享线索。这些材料天然敏感——线索图泄露给错误队伍、缓存在 Discord 服务器上，或被收件人转发，都会破坏公平性。

对此场景而言，常规 Discord 工作流有两个根本问题：

1. **持久性** — 上传到频道或私信的文件会长期留在 Discord CDN 上。任何参与者（或之后获得频道历史访问权限的人）都可以在「本应可见窗口」之后很久仍重新下载文件。
2. **可缓存性** — Discord 为附件生成持久的 CDN URL。这些 URL 可被第三方工具、浏览器历史、Discord 自身基础设施复制、转发、索引或缓存，完全超出发送者控制。

**Qurl Bot 如何解决：**

组织者将线索图通过私信发给 Qurl Bot。文件安全存放在 LayerV 的 S3 中——**从不**落在 Discord CDN 上——并包在 QURL 访问控制内。组织者会在私信中收到 `resource_id` 与 `qurl_link`。

当需要向某支队伍放出线索时，组织者在群组频道输入 `@QurlBot #r_abc123def @alice @bob`。每位被点名的玩家都会在私信中收到各自唯一、单次使用的 QURL。玩家点击链接后，线索在浏览器中的 `fileviewer.layerv.ai` 内渲染——链接随即被消费并失效，无可转发、缓存或重放。

**一轮具体寻宝可以这样进行：**

> 1. 组织者将线索 PDF 私信发给 Qurl Bot → 收到 `resource_id: r_clue04` 以及所有者预览链接。  
> 2. 回合开始。组织者在 `#hunt-channel` 发帖：`@QurlBot #r_clue04 @team-alpha`。  
> 3. `@team-alpha` 的每位成员在私信中收到各自的一次性链接。频道内不会出现线索内容。  
> 4. 玩家打开链接，在浏览器中查看 PDF。每位玩家的视图都会带有其唯一 mint 链接 ID 的水印——若有人截图外泄，可根据水印追溯到具体收件人。链接在访问后自毁。  
> 5. 十分钟后，迟到者或对手尝试该链接——或在 Discord 历史里寻找——将一无所获，线索已消失。  
> 6. 当 QURL 链接过期后，底层文件会从 S3 **永久且不可恢复地删除**——系统中不再保留任何副本。

该模型使组织者可精确控制「谁、在何时、且恰好一次」看到线索——且文件从不进入 Discord 的永久存储。QURL 过期后，底层文件同样会从 S3 永久销毁，系统中不留副本。

---

## 3. 架构

### 3.1 参与方

| 参与方 | 角色 |
|---|---|
| **文件所有者** | 向 Qurl Bot 私信上传文件、将其登记为受保护资源的 Discord 用户 |
| **收件人** | 收到一次性 QURL 的 Discord 用户（由所有者直接分享或由机器人分发） |
| **Qurl Bot** | Discord 应用（**Python** / **discord.py**）。处理私信上传、资源登记与群聊链接分发 |
| **Discord API** | 供机器人接收附件、校验服务器成员关系、发送私信等 |
| **getqurllink.layerv.ai** | LayerV 上传服务。接收文件、存入 S3、将 `fileviewer.layerv.ai/{file_id}` 注册为 QURL 资源，并返回 `resource_id` + `qurl_link` |
| **fileviewer.layerv.ai** | LayerV 文件查看器。在浏览器中渲染文件（PDF、图片等）；位于 QURL 访问控制之后——仅能通过有效 QURL 令牌访问 |
| **LayerV QURL API** | 位于 `api.layerv.ai` 的既有 Go 服务。机器人通过 `POST /v1/qurls/{resource_id}/mint_link` 为已登记资源额外 mint 按收件人的链接 |
| **所有者注册表** | 持久化的 `resource_id → discord_user_id` 映射，由机器人维护，用于在分发时强制执行所有权 |

### 3.2 服务职责

```
┌─────────────────────────────────────────────────────────────┐
│                      Qurl Bot                               │
│  私信上传 ──► getqurllink.layerv.ai ──► resource_id         │
│                      + qurl_link（所有者副本）               │
│                                                             │
│  分发  ──► api.layerv.ai/mint_link ──► 按用户链接           │
│               （每位 @提及 用户各调用一次）                  │
└─────────────────────────────────────────────────────────────┘

收件人点击 qurl_link
  → QURL 解析令牌
  → 打开 fileviewer.layerv.ai/{file_id}
  → 令牌被消费（一次性）
```

### 3.3 流程 1 — 私信上传

```
文件所有者      Qurl Bot          getqurllink.layerv.ai
     |               |                      |
     |--私信: 文件-->|                      |
     |               |                      |
     |               |--POST /upload------->|
     |               |                      |
     |               |    [文件写入 S3]     |
     |               |    [注册资源]       |
     |               |<--{ resource_id,     |
     |               |    qurl_link,        |
     |               |    expires_at }------|
     |               |                      |
     |               | [保存 resource_id → discord_user_id]
     |               |                      |
     |<--私信: resource_id + qurl_link + expires_at + 说明
```

### 3.4 流程 2 — 群聊分发

```
文件所有者      Qurl Bot         Discord API     api.layerv.ai
     |               |                |                |
     |--@QurlBot #r_abc @alice @bob-->|                |
     |               |                |                |
     |               | [校验发送者 == r_abc 的所有者]
     |               |                |                |
     |               | 对每位 @提及 用户:              |
     |               |--POST /v1/qurls/r_abc/mint_link->|
     |               |  { expires_in: "15m",            |
     |               |    one_time_use: true,            |
     |               |    label: "discord:user_id" }     |
     |               |<--{ qurl_link, expires_at }-------|
     |               |                |                |
     |               |--私信 qurl_link 给 @alice        |
     |               |--私信 qurl_link 给 @bob          |
     |               |                |                |
     |<--ACK（频道）: "已向 2 名收件人发送链接"
```

---

## 4. 机器人命令与触发条件

### 4.1 私信上传触发

**触发：** 用户直接向 Qurl Bot 发送私信，且消息包含文件附件。无需斜杠命令——私信中的附件即为唯一触发条件。

**机器人私信回复示例：**
```
✅ Your file has been protected!

Resource ID:  r_abc123def
Link:         https://qurl.link/at_xyz789abc
Expires:      in 7 days

Share this link directly with one person, or send
individual links to multiple users in a server:

  @QurlBot #r_abc123def @user1 @user2 ...
```

### 4.2 群聊分发命令

**触发：** 在服务器频道中，消息符合：
```
@QurlBot #<resource_id> @user1 [@user2 ...]
```

**示例：**
```
@QurlBot #r_abc123def @alice @bob @charlie
```

**机器人在频道中的确认（频道内所有人可见）：**
```
Links dispatched to 3 users.
```

**收件人私信：**
```
🔗 You've been granted access to a file.

https://qurl.link/at_def456ghi

Single use · expires in 15 minutes
```

**所有者私信：**
```
✅ Dispatch complete for #r_abc123def

• @alice — sent
• @bob — sent
• @charlie — ❌ DMs disabled, could not reach
```

---

## 5. getqurllink.layerv.ai

负责完整的上传与注册管线。机器人以 `multipart/form-data` POST 原始文件字节与元数据；服务将文件存入 S3，在 LayerV QURL API 上将 `fileviewer.layerv.ai/{file_id}` 登记为受保护的 `target_url`，并返回可直接使用的 `resource_id` 与 `qurl_link`。机器人从不直接构造或处理 viewer URL。

**端点：** `POST https://getqurllink.layerv.ai/upload`  
**鉴权：** `Authorization: Bearer <QURL_API_KEY>`  
**请求：** `multipart/form-data`

| 字段 | 类型 | 说明 |
|---|---|---|
| `file` | binary | 从 Discord CDN 拉取的原始文件字节 |
| `filename` | string | Discord 附件原始文件名 |
| `owner_label` | string | `discord:<user_id>` — 用于审计 |
| `content_type` | string | 文件 MIME 类型 |

**响应：**
```json
{
  "resource_id": "r_abc123def",
  "qurl_link":   "https://qurl.link/at_xyz789abc",
  "expires_at":  "2026-12-31T23:59:59Z"
}
```

**文件生命周期：** 当 QURL 链接过期时，`getqurllink.layerv.ai` 会永久删除 S3 中的底层文件。过期后系统中不再保留任何副本。

---

## 6. fileviewer.layerv.ai

基于浏览器的文件查看器，在 QURL 访问控制后渲染受保护文件。收件人仅能通过解析有效 QURL 令牌到达该 viewer——不能通过普通 URL 直接访问。机器人与此服务无直接交互。

**访问控制：** `fileviewer.layerv.ai` 无公网可直达地址，不能从互联网直接访问——唯一入口为 QURL 解析器与 AC 代理链。在到达 viewer 之前，直接访问尝试会被拒绝。

```
公网
─────────────────────────────────────────────────────────────────
收件人 ──qurl_link──► QURL 解析器 ──令牌有效──► AC 代理
    │                         │                              │
    │                    无效 → 拒绝                         │
    │                                                        │
    │        ┌─ 私网 — 不可路由 ────────────────────────────┼──┐
    │        │                                               │  │
    │   ✕ 不可直连                            ┌──────────────▼──┤
    │        │                               │ fileviewer        │
    │        │                               │ .layerv.ai        │
    │        │                               │ 渲染 +            │
    │        │                               │ 水印              │
    │        │                               └──────┬───────────┘
    │        │                                      │ 拉取
    │        │                               S3 存储
    │        └──────────────────────────────────────────────────┘
    │
    └◄── 经代理返回：渲染结果 + 水印视图
```

**水印：** 渲染文件时，`fileviewer.layerv.ai` 将 mint 出的链接 ID（例如 `at_def456ghi`）作为可见水印叠在输出上。因每位收件人获得各自独立 mint、ID 唯一的链接，水印对每位收件人唯一。若收件人截图外泄，水印中的 mint 链接 ID 可追溯到具体泄露者——兼具威慑与取证审计能力。

**支持的文件类型：** 图片（PNG、JPG、GIF、WebP）与 PDF。后续版本将支持更多类型。

---

## 7. LayerV QURL API

### 7.1 mint_link

专用于分发流程：针对已登记资源为每位收件人 mint 链接。

- **端点：** `POST https://api.layerv.ai/v1/qurls/{resource_id}/mint_link`
- **鉴权：** `Authorization: Bearer <QURL_API_KEY>`
- **请求体：**

```json
{
  "expires_in":   "15m",
  "one_time_use": true,
  "label":        "discord:RECIPIENT_USER_ID"
}
```

- **响应：**

```json
{
  "data": {
    "qurl_link":  "https://qurl.link/at_def456ghi",
    "expires_at": "2026-12-31T23:59:59Z"
  }
}
```

---

## 8. 组件

### 8.1 Qurl Bot（Discord 侧）

- **运行时：** Python 3
- **框架：** discord.py
- **触发：**
  - 私信频道 `on_message` 且带附件 → 上传流程
  - 服务器频道 `on_message` 且 @机器人且可解析 `resource_id` → 分发流程（频道内禁止上传附件）
  - 斜杠 `interaction` 处理函数存在于代码中，但默认配置下未注册命令

**Discord Gateway Intents（必需）：**
- `Guilds`
- `GuildMessages`
- `MessageContent` ⚠️ — 特权 intent；须在 Discord 开发者门户中启用
- `DirectMessages`

**OAuth2 机器人权限：**
- `Send Messages`
- `Send Messages in Threads`
- `Read Message History`
- `Use Slash Commands`

### 8.2 所有者注册表

```javascript
// 生产环境：可替换为 Redis 或 Postgres
const ownerRegistry = new Map();
// ownerRegistry.set('r_abc123def', '123456789012345678');
```

---

## 9. 环境变量

| 变量 | 说明 |
|---|---|
| `DISCORD_BOT_TOKEN` | 机器人 OAuth 令牌。仅用于 Discord API 调用——绝不发往 LayerV |
| `DISCORD_CLIENT_ID` | 应用 Client ID，用于启动时注册斜杠命令 |
| `QURL_API_KEY` | LayerV API 密钥（`lv_live_...`）。用于同时向 `getqurllink.layerv.ai` 与 `api.layerv.ai` 鉴权 |
| `PORT` | 健康检查 HTTP 服务端口（默认：`3000`） |

---

## 10. 源码摘录（`adapters/discord_bot.py` 与客户端）

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

## 11. 数据流摘要

```
所有者私信发送文件
  │
  ▼
Qurl Bot 从 Discord CDN 拉取文件字节
  │
  ▼
POST getqurllink.layerv.ai/upload
  │  （写入 S3，将 fileviewer.layerv.ai/{id} 登记为资源）
  ▼
← { resource_id, qurl_link, expires_at }
  │
  ├─ Bot 保存 resource_id → 所有者 到注册表
  └─ Bot 私信所有者：resource_id + qurl_link

所有者在频道分发：@QurlBot #r_abc @alice @bob
  │
  ▼
Bot 校验所有权
  │
  ├─ POST api.layerv.ai/v1/qurls/r_abc/mint_link  （针对 @alice）
  │    ← { qurl_link_alice }  →  私信 @alice
  │
  └─ POST api.layerv.ai/v1/qurls/r_abc/mint_link  （针对 @bob）
       ← { qurl_link_bob }    →  私信 @bob

收件人点击链接
  │
  ▼
QURL 解析令牌 → 打开 fileviewer.layerv.ai/{file_id}
  → 令牌被消费（一次性）
  → 过期时从 S3 永久删除文件
```

---

## 12. 安全说明

- **机器人令牌从不发往 LayerV** — 仅在机器人进程内用于 Discord API
- **QURL_API_KEY 用于向 LayerV 鉴权** — 无共享密钥、无 HMAC、无跨语言契约
- **仅所有者可分发** — `ownerRegistry` 保证用户不能为他人登记的资源触发分发
- **按收件人单次链接** — 每次 `@提及` 触发独立的 `mint_link` 调用；链接不可复用或通过转发二次访问
- **链接永不出现在频道** — 所有 QURL 仅通过私信投递
- **水印审计** — 每次 mint 的链接在渲染视图上带有唯一 ID 水印；泄露可追溯到具体收件人
- **完整 API 审计** — 每次 mint 的链接带有 `label: discord:<user_id>`，可在 LayerV 控制台按用户归因
- **过期即销毁文件** — QURL 过期时底层 S3 文件永久删除
- **限流** — 两条流程均为每用户每分钟 5 次请求

---

## 13. 生产环境考量

### 13.1 所有者注册表持久化
内存中的 `ownerRegistry` 在进程重启后会丢失。应以 `resource_id` 为键持久化到 Redis 或 Postgres，并在机器人启动时从存储恢复。

### 13.2 真正的仅自己可见频道回复
服务器频道中的 `msg.reply()` 对所有人可见。可将分发触发迁移为斜杠命令（如 `/qurl dispatch`），以使用真正的 `{ ephemeral: true }` 回复。

### 13.3 大规模限流
多实例部署时，应将内存限流器替换为基于 Redis 的滑动窗口。

### 13.4 Discord Gateway Intents
`MessageContent` 为特权 intent。必须在 Discord 开发者门户 **Bot → Privileged Gateway Intents** 中显式启用，否则分发触发将**静默无法触发**。

---

## 14. Discord 开发者门户配置

1. 在 https://discord.com/developers/applications 创建新应用  
2. 在 **Bot** 下：启用 **Message Content Intent** 与 **Server Members Intent**  
3. 在 **OAuth2 → URL Generator**：勾选作用域 `bot` + `applications.commands`；勾选机器人权限：`Send Messages`、`Read Message History`、`Use Slash Commands`  
4. 使用生成的 URL 将机器人邀请到你的服务器  
