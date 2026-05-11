# Zero-downtime upgrades for the qURL Discord bot

**Status:** design — ratified, implementation in progress
**Tracking:** `qurl-integrations-infra#122` (deploy outage), `qurl-integrations#TBD` (this PR)
**Owners:** posey + reviewers

## Summary

Every deploy of `bot_gateway` today causes a 30–90 s outage. Three independent
user-visible surfaces break: slash command initiation, in-flight `/qurl send`
flows (button clicks, file drops, modals), and the bot's presence indicator. To
meet an enterprise availability bar, all three must survive a deploy with no
user-observable impact.

The redesign collapses to one architectural decision (event-shipper split) plus
three implementation pillars. After this work, deploying business logic is a
standard rolling deploy with zero user impact, and the rarely-deployed gateway
tier uses Discord's own session-resume primitive plus a hot-standby with direct
push-handoff to keep its own outage sub-second.

## Why the current design can't get there

Three independent layers break during a deploy of the current `bot_gateway`
service (singleton, `desired_count=1`, `deployment_minimum_healthy_percent=0`):

1. **Slash command initiation.** Discord enforces a 3 s ACK deadline on every
   interaction. While the gateway task is being replaced, slash commands
   return "interaction failed" in the user's Discord client.
2. **In-flight `/qurl send` flows.** The send flow uses `awaitMessageComponent`,
   `awaitMessages` (file drop in DM), and `awaitModalSubmit` in nine distinct
   sites across `apps/discord/src/commands.js`. Each is a Promise resolved by
   an event the Gateway delivers to *this process*. SIGTERM = those Promises
   never resolve = the user's flow silently dies. This is a bug today even
   outside of deploys (any process crash mid-flow drops the user).
3. **Bot presence.** The green dot in Discord member lists flips off for the
   same window.

Two Discord constraints make these hard:

- **One active Gateway WebSocket per bot token.** A second `IDENTIFY` on the
  same token invalidates the first session. Rules out the naive
  `desired_count=2` rolling deploy. Already enforced in
  `qurl-integrations-infra/qurl-bot-discord/terraform/variables.tf` via a
  validation block rejecting `gateway_desired_count != 1`.
- **`MESSAGE_CREATE` is Gateway-only.** Discord's HTTPS Interactions endpoint
  (an alternative interaction delivery surface) carries interaction events but
  *not* regular DM messages. Users dropping a file as a DM message is the
  load-bearing step of `/qurl send`; switching to HTTPS Interactions would
  break that step. We considered and rejected this approach.

## The redesign at a glance

Two tiers:

```
                     Discord Gateway WS
                            │
                            ▼
      ┌─────────────────────────────────────────┐
      │  GATEWAY TIER (bot_gateway)             │
      │  - Maintains Discord Gateway WebSocket  │
      │  - Forwards every dispatch to SQS       │
      │  - Zero business logic                  │
      │  - Hot-standby + leader-election lock   │
      │  - Cross-process RESUME via DDB         │
      │  - Deployed rarely (Node/discord.js     │
      │    version bumps, sharding changes)     │
      └────────────────┬────────────────────────┘
                       │
              SQS FIFO Queue (per-guild
              ordering, DLQ, autoscaling
              signal for workers)
                       │
                       ▼
      ┌─────────────────────────────────────────┐
      │  WORKER TIER (extends today's bot_http) │
      │  - Stateless queue consumer             │
      │  - All /qurl command logic              │
      │  - All flow-state transitions in DDB    │
      │  - Discord REST for outbound responses  │
      │  - Standard rolling deploys (min=100,   │
      │    max=200, ALB drain) — 0-downtime     │
      │    trivially because stateless          │
      │  - Auto-scales on queue depth           │
      └─────────────────────────────────────────┘
```

The user-visible deploy story becomes:

- **Worker tier deploy (the common case — every PR):** rolling deploy, standard
  ECS `deployment_minimum_healthy_percent = 100`, `maximum = 200`. New tasks
  reach steady state before old tasks stop. ALB target-group `deregistration_delay = 90`
  matches `idle_timeout` so in-flight HTTP requests drain. Workers are stateless
  so there's no transferable per-process state to worry about. **0 downtime.**
- **Gateway tier deploy (rare):** hot-standby pair + DDB leader-election lock.
  Only the lock holder calls `WebSocketManager.connect()`. SIGTERM on the
  active replica pushes a "you're up" notification to the standby over an
  internal HTTP control channel, then releases the lock and closes the WS with
  code 1000. The standby was pre-booted (Node up, code loaded, DDB connected,
  control listener bound, just no Gateway connection) so handoff is hundreds
  of milliseconds. The standby reads session state from DDB and `RESUME`s on
  Discord's gateway-resume URL — Discord buffers events on the previous
  session for ~60 s and replays them on resume, so no events are lost across
  the handoff. **Sub-second slash command and presence gap; no event loss.**

## The three pillars

### Pillar 1 — Persistent flow state (replaces in-memory `await*`)

Every in-memory `await*` site becomes a transition on a row in a new DDB table
`qurl_bot_flow_state`. The schema is shard-aware from day one (the `shard_id`
prefix is `"0:1"` while we have a single shard; the schema generalizes when
sharding lands later).

| Attribute | Type | Notes |
|---|---|---|
| `flow_id` (PK) | string | `<shard_id>#<guild_id>#<channel_id>#<user_id>` |
| `stage` | string | `awaiting_button`, `awaiting_file`, `awaiting_modal`, … |
| `payload` | map | flow-specific data (send_nonce, attachment metadata, recipient list) |
| `version` | number | optimistic concurrency control |
| `created_at` | number | epoch seconds |
| `expires_at` | number | DDB TTL; matches the longest existing `await*` timeout |

The state-machine harness `apps/discord/src/flow-state.js` (PR 4) provides
`createFlow`, `loadFlow`, `transitionFlow` (DDB conditional `UpdateItem` with
`version = :expected` for OCC), and `deleteFlow`. Every transition emits a
metric `qurl_bot_flow_transition_total{stage_from,stage_to,result}` (PR 3).

**Application-level concurrency rule:** a user with an existing non-expired
flow row in a given `(channel_id)` cannot start a second flow there. The second
`/qurl send` returns `"You already have a /qurl send in progress — finish or
cancel it first."` This avoids needing a GSI on the flow table and keeps the
state model simple.

**MESSAGE_CREATE handler.** Today the file-drop step uses
`captureChannel.awaitMessages({...})` on a DM channel. Under the redesign,
**workers** subscribe to a `MESSAGE_CREATE` event class from the queue. The
handler:

1. Reads message metadata: `(channel_id, author_id, attachments)`.
2. Constructs `flow_id` candidate keys from the metadata.
3. Looks up the flow row in DDB. With ~thousands of guilds and ~tens of DMs/s
   peak, this is a cheap lookup. To keep DDB reads off the per-message hot
   path for non-flow DMs (the common case), we cache **positive** flow
   memberships per worker: when worker A creates a flow for user X, A writes
   to DDB and *also* publishes "user X is in flow Y" to a short-TTL in-memory
   table on every worker (via the existing SQS dispatch — workers see flow-
   create events too). Misses always re-read DDB. We **never** cache absence,
   because a user dropping a file is a single irreversible action — a stale
   "no flow" cache entry on the worker that gets the message would silently
   drop the user's upload. Read-through on miss costs one DDB GetItem per
   non-flow DM, which we accept; the optimization is for DMs *during* an
   active flow, not against them.
4. If matched, advances the flow via OCC. If two events race (e.g., user
   uploads twice quickly), one OCC retry path wins, the other gets a clear
   `"flow already advanced"` error.

### Pillar 2 — Cross-process Gateway session resume

`@discordjs/ws` (the underlying library that `discord.js` wraps) exposes
`retrieveSessionInfo(shardId)` and `updateSessionInfo(shardId, info)` as
first-class supported options on `WebSocketManager` (see
`@discordjs/ws@1.2.3` types: `SessionInfo` interface, `RequiredWebSocketManagerOptions`).
These hooks let us back the session state with DDB instead of the default
in-memory `WebSocketShard.sessionInfo` (discord.js's default; see
`discord.js@14.25.1/src/client/websocket/WebSocketManager.js:156-159`).

Under the event-shipper split, the gateway tier doesn't need `discord.js` at
all — it only needs the raw WS to forward events. So the gateway process uses
`@discordjs/ws` directly:

```js
const { WebSocketManager } = require('@discordjs/ws');
const { REST } = require('@discordjs/rest');

const rest = new REST().setToken(token);
const manager = new WebSocketManager({
  token,
  intents: GATEWAY_INTENTS_BITFIELD,
  rest,
  retrieveSessionInfo: async (shardId) => ddb.getSession(shardId),
  updateSessionInfo: async (shardId, info) => ddb.putSession(shardId, info),
});

manager.on('dispatch', ({ data, shardId }) => forwardToQueue(data, shardId));
await manager.connect();
```

The DDB row for a shard (in the new `qurl_bot_gateway_session` table — PR 12):

| Attribute | Type | Notes |
|---|---|---|
| `shard_id` (PK) | string | `"0:1"` while single-shard; generalizes to `"k:n"` |
| `session_id` | string | from `READY` event |
| `resume_url` | string | from `READY.resume_gateway_url` |
| `sequence` | number | last received dispatch sequence |
| `updated_at` | number | epoch ms |

Writes are throttled: `updateSessionInfo` fires on every dispatch (high rate),
but we only write to DDB on `READY` (rare) and at most once per second for
sequence updates. The sequence is conservatively flushed final-time on
SIGTERM. The 60 s Discord buffer is more than enough headroom for a worst-case
write latency of a few seconds.

On boot, the standby's `retrieveSessionInfo` returns the persisted row.
`@discordjs/ws` issues `RESUME` (op 6) instead of `IDENTIFY` (op 2). If
Discord rejects the resume (session expired, version skew, etc.), `@discordjs/ws`
falls back to `IDENTIFY` automatically — and `updateSessionInfo(shardId, null)`
fires so we know the resume failed. Operationally we count both paths
separately as SLIs.

### Pillar 3 — Hot-standby with direct push-handoff

Two gateway replicas. A DDB-conditional-write lock primitive (table
`qurl_bot_gateway_lock` — PR 12) provides single-active enforcement:

- Lock row: `PK = shard_id`, attrs `lock_holder`, `expires_at`, `instance_id`, `version`.
- `acquireLock`: conditional `PutItem` where `lock_holder IS NULL OR expires_at < now`.
- `renewLock`: heartbeat every 5 s (TTL 15 s), conditional on `instance_id = self`.
- `releaseLock`: conditional delete on `instance_id = self`.
- Only the lock holder calls `WebSocketManager.connect()`. The non-holder boots
  fully (DDB clients open, control-channel listener bound, `/health` returning
  200) but skips the gateway connection.

**Standby discovery.** Each replica writes its container IP + control-channel
port to its own row in a small DDB heartbeat table (PK = `instance_id`, TTL
30 s, refreshed every 5 s alongside the leader-lock renewal). The active reads
peer rows from that table — its own row excluded — and uses the freshest one
as the handoff target. This avoids ECS Service Connect / Cloud Map, which
resolves to *all* healthy gateway tasks (including the active itself) and
would need a self-exclusion filter anyway.

**SIGTERM handoff sequence on the active replica:**

```
1. Receive SIGTERM
2. Stop accepting new dispatches into the queue (drain)
3. Persist final sequence to DDB session row
4. Read peer-heartbeat row from DDB (excluding self) → get standby IP+port
5. POST /control/yours to that IP
6. Wait for ACK or 200 ms timeout
7. releaseLock()
8. WebSocketManager.destroy({ code: 1000 })  ← clean close, Discord buffers
9. Exit
```

If step 4 returns no peer (standby still booting, or just-died), step 5
becomes a no-op and the deploy falls back to the cold-start path: standby
acquires the lock when its own heartbeat starts, RESUMEs from DDB. The
handoff window degrades from sub-second to a few seconds, but doesn't fail
outright.

**Standby on `POST /control/yours`:**

```
1. Verify origin (mTLS or shared HMAC secret)
2. acquireLock()       ← should be immediate since active just released
3. WebSocketManager.connect()   ← @discordjs/ws calls retrieveSessionInfo,
                                  finds the row, sends RESUME
4. ACK control request
```

Sub-second handoff because both replicas are warm and the active *tells* the
standby instead of the standby polling. We measure the actual distribution in
the chaos suite (PR 15).

**Why direct push beats short-polling:** polling at 100 ms costs 10 reads/s
per standby per shard. At one shard that's negligible, but doesn't compose to
the sharded future. Push-handoff is constant cost and lower latency. DDB
Streams was considered and rejected — up to ~3 s replication lag, doesn't meet
the sub-second target.

## Why not other approaches

We evaluated three alternatives and rejected each:

- **Discord HTTPS Interactions endpoint** (have Discord POST interactions to
  the ALB instead of delivering via the Gateway). Rejected because file drops
  arrive as regular `MESSAGE_CREATE` events, which the HTTPS surface does not
  carry. Switching would break the `/qurl send` file-capture step entirely
  without a separate UX redesign.
- **`min_healthy=100/max=200` on the gateway service alone.** Rejected because
  Discord's one-active-WS-per-token rule means two simultaneously-IDENTIFY'd
  replicas cause session-identity flap (the second IDENTIFY invalidates the
  first; the first's reconnect loop then invalidates the second; both keep
  flapping).
- **Pre-warmed gateway pool on ECS-EC2 instead of Fargate** (so `IDENTIFY` is
  fast enough to be acceptable as the no-resume fallback). Rejected because it
  adds significant infra complexity (AMI pipeline, instance lifecycle,
  autoscaling) that doesn't compose into the event-shipper architecture, which
  bounds the gateway-outage blast radius regardless of `IDENTIFY` speed.

## Rollback plan

Each phase has an independent rollback path:

| Phase | Rollback |
|---|---|
| Flow state | Revert command-file PRs. DDB flow rows expire on their TTL. |
| Event shipper | App-side feature flag `ENABLE_EVENT_SHIPPER=false` falls back to in-process dispatch on the gateway role. Worker role's SQS consumer remains running but receives nothing. |
| Cross-process RESUME | App-side feature flag `ENABLE_GATEWAY_RESUME=false` falls back to in-memory session state (Discord IDENTIFY-only every boot). |
| Hot-standby | Set `gateway_desired_count = 1` in tfvars. The lock primitive sees no peer and stays leader. |

## SLI / SLO definitions

The "zero downtime" target makes two distinct claims; each gets its own SLI:

| SLI | Definition | SLO |
|---|---|---|
| Slash command availability | `1 - (failed_interactions / total_interactions)` over 5-min windows | 99.95% (≈ 22 m / month error budget) |
| Flow continuity | `1 - (silently_dropped_flows / total_flows)` over 1-day windows | 99.99% |
| Presence | Active WS time / wall time | 99.9% (≈ 43 m / month) |
| Resume success rate | `RESUME_OK / (RESUME_OK + RESUME_FAIL)` per deploy | tracked, not SLO'd directly — surfaced as a deploy-time gauge |

The Resume Success Rate is the load-bearing dependency for the gateway-tier
deploy story. It's not SLO'd because we don't control it (Discord does), but
we alert if it drops below 99% over a 24 h window — that signals a Discord-side
change or our session-state staleness, both of which need investigation.

## Open questions, deliberately deferred

These do not block the current design but should be revisited:

- **Sharding inflection (~2,500 guilds).** Schemas are shard-aware, but the
  leader-election lock and queue consumer dispatch will need a per-shard
  generalization. Tracked separately; revisit when guild count crosses 1,500
  as a leading indicator.
- **Worker-tier autoscaling tuning.** Initial alarm thresholds in PR 1 are
  conservative. We'll tune in the first month of real traffic.
- **Cross-region failover.** Out of scope. The bot's qURL API dependency and
  the existing infra both pin us to us-east-2.

## Spike validation

This PR ships a working spike (`apps/discord/scripts/gateway-resume-spike.js`)
that demonstrates the cross-process resume mechanism against a real Discord
bot token. The spike isn't wired into the production code path; it's a
proof-of-mechanism that lets a reviewer run two Node processes against the
sandbox bot and verify Discord accepts the `RESUME` from the second process
using session state captured by the first.

See `scripts/gateway-resume-spike.js` for the runbook.
