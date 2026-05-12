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
              SQS Standard Queue (DLQ,
              autoscaling signal for
              workers; ordering enforced
              at the DDB layer)
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
metric `qurl_bot_flow_transition_total{stage_from,stage_to,result,terminal}`
(materialized by the event-shipper observability Phase 1.0 PR's audit-event
reservation plus the matching CloudWatch metric filter in
`qurl-integrations-infra`).

**Application-level concurrency rule:** a user with an existing non-expired
flow row in a given `(channel_id)` cannot start a second flow there. The second
`/qurl send` returns `"You already have a /qurl send in progress — finish or
cancel it first."` This avoids needing a GSI on the flow table and keeps the
state model simple.

**Per-command override matrix.** The block-second-flow rule above is the
default. Two commands carry intentional exceptions because their UX shape
makes blocking the wrong call:

| Command | Behavior on existing flow | Rationale |
|---|---|---|
| `/qurl revoke` | **Supersede** (admin_cleanup + recreate) | The menu is a stateless listing of recent sends; an admin who can't remember whether they cancelled the prior dropdown shouldn't be told "finish your existing dropdown first." Re-running shows a fresh menu and the orphan menu's selection lands on `loadFlow → null → "superseded"`. |
| `/qurl setup` | Block (default) | Multi-step OAuth / API-key paste flow with in-progress state; the user should finish or cancel the active step. |
| `/qurl send` | Block (default) | Multi-step send flow (recipient picker, attachment capture, confirm); abandoning mid-flow because a duplicate `/qurl send` slipped through would lose user input. |

The supersede semantics are **enforced at the harness level** by `deleteFlow`'s
conditional `stage = :expected` gate (see `apps/discord/src/flow-state.js`).
A revoke command can only supersede a revoke flow; it cannot accidentally
admin_cleanup a sibling setup or send flow that happens to share the same
`flow_id` keying.

**MESSAGE_CREATE handler.** Today the file-drop step uses
`captureChannel.awaitMessages({...})` on a DM channel. Under the redesign,
**workers** subscribe to a `MESSAGE_CREATE` event class from the queue. The
handler:

1. Reads message metadata: `(channel_id, author_id, attachments)`.
2. Constructs `flow_id` candidate keys from the metadata.
3. Looks up the flow row in DDB. **Every** `MESSAGE_CREATE` does a single
   `GetItem` against the flow table by composite key. No in-process cache —
   the earlier draft had a per-worker positive cache that would only have
   been populated on whichever worker handled the matching flow-create
   event, leaving every other worker with stale-no-flow assumptions. With
   SQS Standard, each message is delivered to one worker; there's no
   natural affinity between flow-create and the user's subsequent
   file-drop landing on the same worker. A coherent cache would require
   a separate broadcast channel (Redis pubsub or DDB Streams), which is
   more infrastructure for marginal cost savings.
   The volume math says the DDB cost is bounded: at ~thousands of guilds
   and the bot's invite-only-beta DM patterns, GetItem-on-every-non-flow-
   DM is tens-to-hundreds of reads per second, fully within DDB's
   on-demand pricing without provisioned capacity tuning. Revisit only if
   the volume crosses an order of magnitude.
4. If matched, advances the flow via OCC. If two events race (e.g., user
   uploads twice quickly), one OCC retry path wins, the other gets a clear
   `"flow already advanced"` error.

### Queue: SQS Standard + application-layer idempotency

The queue between the gateway tier and the worker tier is SQS Standard, not
FIFO. The rationale is that the ordering FIFO would enforce isn't load-
bearing — the source of truth for "did this flow advance" is the DDB flow
row, not the order in which events were processed — and FIFO's per-queue
throughput ceiling (300 TPS, 3000 with high-throughput mode) is a real
cliff at sharding scale that Standard doesn't have.

Standard's at-least-once delivery means duplicate events. Application-layer
idempotency covers it:

- **`INTERACTION_CREATE` duplicates** are naturally idempotent at Discord:
  `/callback` returns "Unknown interaction" on the second call because the
  interaction token has already been ACK'd. The worker's only real cost on
  a duplicate is a wasted REST call and a logged 4xx.
- **`MESSAGE_CREATE` duplicates** are blocked by OCC at `transitionFlow`:
  the second arrival reads the same `version`, attempts the same transition,
  and the conditional `UpdateItem` fails with `ConditionalCheckFailedException`.
  The handler treats this as "another worker already advanced this flow"
  and exits without acting.
- **`GUILD_CREATE` / `GUILD_DELETE` duplicates** are audit-only; duplicate
  log lines are tolerable.

Producer-side dedup hint: the gateway sets `MessageAttributes.event_id`
from Discord's per-dispatch `s` (sequence) field as a defense-in-depth —
workers log when they see an `event_id` they processed within the last
5 minutes (in-process LRU, ~10k entries) so a real-world dup rate can
be quantified. The LRU is best-effort; OCC is the correctness primitive.

Sharding caveat: Discord's `s` is **per-shard** monotonic, not global.
At single-shard (today) it's a usable event_id. At the sharding
inflection (~2,500 guilds), `s` alone will collide across shards and
the LRU will incorrectly drop legitimate events from sibling shards.
Migrate the producer-side `event_id` to `${shard_id}:${s}` in the
sharding PR; the worker-side LRU shape doesn't change.

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

**Contract gotcha: `retrieveSessionInfo` MUST respect the null clear.** When
`updateSessionInfo(shardId, null)` fires, future `retrieveSessionInfo` calls
for that shard must return `null` until a new `READY` produces a fresh
session. Returning the (now-dead) session again produces an infinite
RESUME-reject loop: Discord rejects, library clears, we hand the same
session back, Discord rejects again, every ~200 ms. The first run of
`scripts/gateway-resume-spike.js` against the sandbox token surfaced this
exact loop because the spike returned a captured-at-startup value
unconditionally. Production code maintains a mirror of the
@discordjs/ws-visible session state that updateSessionInfo writes into;
retrieveSessionInfo reads from the mirror, not from the original DDB
read. The mirror is hydrated from DDB once at boot, then updated by the
callback.

A second related guard: cap consecutive failed `IDENTIFY` attempts. Discord's
per-bot identify budget is 1000 per 24 h; an unexpected churn loop (e.g.,
another process contending for the same token) can blow through it. The
production code aborts the shard after N consecutive identifies without a
successful READY and falls back to a controlled process exit + ECS
restart — same shape as the spike's `MAX_IDENTIFY_ATTEMPTS` guard.

**Contract gotcha: don't call `manager.destroy()` if you want the session to
survive into the next process.** Reading `@discordjs/ws@1.2.3`'s `destroy()`
implementation (`dist/index.js` around line 733): unless you pass
`recover: Resume`, it calls `updateSessionInfo(shardId, null)` AND sends a
close-1000 frame, both of which invalidate the session. The `recover: Resume`
path sends close-4200 (which Discord treats as resumable) but also triggers
`internalConnect()` to resume *back into the same process* — not what we
want for cross-process handoff.

The right shape for the production gateway's SIGTERM handler is therefore:

1. Persist final sequence to the DDB session row.
2. Push "you're up" to the standby's control channel; await ACK.
3. **Exit without closing the WS.** TCP drops without a close frame; Discord
   treats it as an unexpected network disconnect and preserves the session
   in its resume buffer for ~60 s.
4. The standby's `retrieveSessionInfo` returns the persisted row, the
   library issues `RESUME` on the resume URL, Discord replays buffered
   events, no event loss.

The spike's phase1 validates this exact sequence: persist + `process.exit(0)`
without a `destroy()` call → phase2 in a fresh process picks up the session
and gets `RESUMED dispatch received`. Validated 2026-05-10 against the
sandbox bot token (with the contending ECS task scaled to zero for the
duration).

### Pillar 3 — Hot-standby with direct push-handoff

Two gateway replicas. A DDB-conditional-write lock primitive (table
`qurl_bot_gateway_lock` — PR 12) provides single-active enforcement:

- Lock row: `PK = shard_id`, attrs `lock_holder`, `expires_at`, `instance_id`, `version`.
- `acquireLock`: conditional `PutItem` where `attribute_not_exists(lock_holder)
  OR expires_at < :now` (with `:now` from the caller's wall clock; see clock-
  skew note below).
- `renewLock`: heartbeat every **2 s** (TTL **6 s**), conditional on
  `instance_id = self AND version = :expected`. Three missed renewals = lock
  becomes acquirable by a peer.
- `transferLock(self → peer_instance_id)`: atomic `UpdateItem` with condition
  `instance_id = :self AND version = :expected` and set `instance_id =
  :peer, expires_at = :now + ttl, version = :version + 1`. Used by the
  SIGTERM handoff path so the active hands ownership over in one DDB op,
  with no lock-released-but-not-acquired-yet window.
- Only the current lock holder calls `WebSocketManager.connect()`. The non-
  holder boots fully (DDB clients open, control-channel listener bound,
  `/health` returning 200) but skips the gateway connection.

**Heartbeat / TTL choice.** 2 s / 6 s instead of the original 5 s / 15 s.
The original numbers were paced for low-frequency lock churn — but the
*recovery floor* matters here: the no-peer SIGTERM path means standby
must wait for the lock TTL to expire before it can acquire. At 2 s / 6 s
the worst-case no-peer cold-fallback floor is ~6 s + RESUME RTT
(~1 s) ≈ 7 s, not the original ~15 s. The cost is **2.5× more DDB
writes** (0.2/s → 0.5/s, one heartbeat per shard per 2 s instead of
per 5 s). At DDB on-demand write pricing (~$1.25/M), one shard runs
~$1.62/month total for the heartbeat — trivially worth the tighter
recovery floor.

**Clock skew on `expires_at < :now`.** The `:now` parameter is evaluated
on the caller's wall clock. Fargate task clocks are normally fine
(chrony-equivalent via the host) but the dependency is load-bearing.
The real correctness primitive isn't the timing math — it's the
`version` attribute on `transferLock` and `renewLock`. The conditional
write fails if `version` doesn't match the expected value, so even a
clock-skewed peer that *thinks* the lock has expired cannot actually
take it while the legitimate holder is still heartbeating: their
write hits an `ConditionalCheckFailedException`, they back off, and
re-evaluate on the next renewal. The TTL is best-effort recovery for
the case where the legitimate holder has genuinely died; `version` is
the safety net everywhere else.

The HMAC freshness window (5 s) caps skew tolerance on the handoff
path: a peer with > ±2 s of skew may have its HMAC body rejected
as stale even though the lock-side `version` check would absorb the
underlying take-over attempt. We therefore split skew into three
tiers:

- **≤ ±1 s**: zero impact. Heartbeat (2 s) ≫ skew; HMAC freshness
  window (5 s) ≫ skew. Engineering target.
- **±1 s to ±2 s**: degraded. Handoff HMAC bodies near the freshness
  edge may be rejected and re-sent (active retries the POST); lock
  `version` keeps the take-over invariant safe regardless.
- **> ±2 s**: incident. Both the freshness window and the
  heartbeat-vs-TTL margin start failing. Treated as a chrony
  failure — paging on `clock_skew_seconds` metric in the gateway-
  health canary (PR 13's observability hooks). Recovery is
  operational (replace the task), not application-side.

**Why no per-replica polling.** The lock primitive alone doesn't tell the
standby *when* to act; the standby only needs the lock when it's
becoming active. The push-handoff (below) is the primary signaling
mechanism. The standby reads the lock row only as a sanity check after
`transferLock` returns success, and on the cold-start path (active died
without push-handoff completing).

**Standby discovery.** Each replica writes its container IP + control-channel
port to its own row in a small DDB heartbeat table (PK = `instance_id`,
attrs include `updated_at`, refreshed every 2 s alongside the leader-lock
renewal). When the active reads candidate peer rows, it filters by
`updated_at > now - 6s` rather than relying on DDB TTL — DDB's TTL
deletion is eventual (AWS-documented worst case up to 48 h), so a stale
row past its TTL might still be visible. The freshness filter at read
time is the correctness guarantee; TTL is hygiene.

**Control-channel auth: shared HMAC secret.** The control channel listens
on the container's task ENI inside the private subnet; it's reachable by
anything in the VPC. Without auth, any compromised pod could push
handoff messages and trigger a session takeover. The auth surface is
narrow (one endpoint, fixed payload shape) so a 32-byte HMAC secret
shared via the existing SSM-`SecureString`-via-task-def pattern is
sufficient — same trust class as `DISCORD_TOKEN`. mTLS was considered
and rejected because the cert-rotation pipeline is more infra than the
problem warrants for an in-VPC reachability surface. The secret is named
`/qurl-bot-discord/GATEWAY_HANDOFF_HMAC`, generated via
`openssl rand -hex 32`.

**HMAC payload includes a nonce + timestamp + recipient** to defeat
replay. The signed body is
`{active_instance_id, peer_instance_id, expected_version, nonce, ts}`
where `nonce` is `crypto.randomBytes(16).toString('hex')` and `ts` is
the active's monotonic epoch-ms at send time. The standby:

1. Rejects requests with `|ts - now| > 5_000ms` (clock-skew tolerant
   replay-window cap).
2. Rejects requests where `peer_instance_id != self.instance_id`
   (the body must be addressed to *this* standby). This binds
   intra-cluster — at sharding inflection, a body captured from
   shard 0's handoff can't be replayed against shard 5's standby.
3. Maintains a small in-memory LRU of seen nonces (~1k entries,
   eviction on size), rejects duplicates. LRU correctness is tied
   to the freshness cap, not the size: at 5 s freshness, a 1k-entry
   LRU absorbs up to ~200 handoffs/sec before still-fresh nonces
   start evicting (which would allow replay within the 5 s window).
   Real-world handoff rate is one-per-deploy, so the size is
   ~3 orders of magnitude over-provisioned; revisit the size if
   freshness or handoff rate changes.

Without these, the `expected_version` value in a captured body could
be replayed — OCC on `transferLock` would catch the *second* replay
(version moved) but the *first* replay during a real handoff could
move the lock at an unfortunate moment.

**Secret rotation:** dual-secret-accept window via rolling redeploy.
The SSM parameter holds a JSON object
`{"current": "...", "previous": "..."}` rather than a raw secret;
both pods load it at boot. The active always signs with `current`;
the standby accepts either.

The bot does NOT hot-reload SSM at runtime — `current`/`previous` are
captured at boot and held in process memory for the task lifetime.
This matches every other SSM-backed secret on the bot
(`DISCORD_TOKEN`, `KEY_ENCRYPTION_KEY`, etc.), all of which are
boot-time loads. Rotation procedure is therefore a rolling redeploy:

1. Write `{"current": "<new>", "previous": "<old>"}` to the SSM
   parameter.
2. Trigger a rolling redeploy of `bot_gateway`. With
   `gateway_desired_count=2` and the deployment invariant that
   serializes replacements (PR 14 pins
   `minimumHealthyPercent=50 / maximumPercent=100`), the **standby
   is always replaced first** — ECS stops the standby, which has no
   active WS to disrupt, then waits for the new standby to be
   healthy before moving on to the active. The new standby boots
   with both `<new>` and `<old>` in memory and accepts either. Only
   then does ECS replace the active, which boots with both and
   starts signing with `<new>`. There is no window where the active
   signs with `<new>` while a standby only knows `<old>` —
   verified by the serial-replacement invariant.
3. After at least 24 h of stable operation (drains any in-flight
   handoff messages signed under `<old>`), write
   `{"current": "<new>", "previous": "<new>"}` (or scrub
   `previous` to `null`) and redeploy again to retire the old
   secret.

Cadence: annually, or on suspected compromise. Compromise rotation
skips step 3's 24 h wait and writes `previous = null` immediately.

This deliberately couples secret rotation to a deploy cadence. The
alternative (hot-reloading SSM with a TTL cache, e.g., 60 s) was
considered and rejected: the bot already gates SSM reads on the
task-execution-role IAM grant evaluated once at task startup, so a
hot-reload path would need new IAM scope, an explicit refresh loop,
and a story for "what if the refresh fails mid-handoff." The rolling-
redeploy path reuses existing infrastructure (ECS deployment
machinery, the task-def secrets block) and matches the bot's
existing operational shape.

**SIGTERM handoff sequence on the active replica:**

```
1. Receive SIGTERM
2. Stop forwarding new dispatches to SQS (drain)
3. Persist final sequence to the DDB session row
4. Read peer-heartbeat row from DDB (excluding self, filtered by
   updated_at > now - 6s) → get standby instance_id + IP + port
5. If no peer: skip the push path; fall through to cold-start (step 9)
6. POST /control/yours to peer with body {active_instance_id, expected_version}
   signed with HMAC. Standby's handler runs transferLock atomically
   (active → standby), then WebSocketManager.connect(); standby ACKs.
7. Wait for ACK or 200 ms timeout. ACK means standby has IDENTIFY'd
   or RESUMEd successfully (standby's handler doesn't ACK until
   @discordjs/ws's connect() resolves).
8. (No releaseLock — transferLock did it atomically in step 6.)
9. Exit. TCP drops; Discord sees a network disconnect; if step 7 ACK'd
   on time, standby's WS is already open and Discord's events flow there.
```

**Why no `manager.destroy()` in step 9.** Per the Pillar 2 contract
gotcha: `destroy()` invalidates the session at the @discordjs/ws layer
and sends a Discord close frame that ends the session. We need the
session to stay alive on Discord's side in case the standby's RESUME
hasn't completed by the time the active exits — TCP drop is the safe
shape.

**If step 4 returns no peer** (standby still booting, or just-died), the
deploy falls back to the cold-start path: standby acquires the lock when
its own next heartbeat fires (within 2 s) AND the TTL has elapsed
(within 6 s of the active's last heartbeat). Floor math:

- **Best case ~7 s**: standby's heartbeat fires immediately after the
  active dies. 6 s TTL wait + ~1 s RESUME RTT.
- **Worst case ~9 s**: standby just renewed its own heartbeat row
  before the active died, so it discovers the dead-active state up
  to ~2 s later (one full heartbeat cycle later). 2 s heartbeat wait
  + 6 s TTL wait + ~1 s RESUME RTT.

Both are well below the original ~15 s+ ceiling and bound the
degraded-mode window. Still degraded vs the push-handoff sub-second
path, but predictable.

**Standby on `POST /control/yours`:**

```
1. Verify HMAC signature on the request body (+ ts freshness + nonce
   not-seen)
2. Verify body.active_instance_id matches a known peer (avoids accidental
   takeovers from a stale stopped task)
3. transferLock(body.active_instance_id → self_instance_id)
   ← atomic single-DDB-op; no acquireLock race
4. WebSocketManager.connect()
   ← @discordjs/ws calls retrieveSessionInfo, finds the row, sends RESUME
5. ACK control request (only after connect() resolves; ACK = "I'm live")
```

**What if step 3 succeeds but step 4 fails** (Discord rate limit,
network blip, RESUME rejection requiring fresh IDENTIFY which is then
also rejected, etc.)? The standby now holds the lock but isn't
connected to Discord. Without recovery the active will time out on
ACK after 200 ms and exit; the standby will sit lock-held-but-no-WS
indefinitely.

The standby runs a **connection-watchdog** loop separately from the
POST handler that closes this gap:

```
every 1 s, if heldLock && !manager.isConnected:
  attempts += 1
  try { await manager.connect() }
  catch (err) {
    if (attempts >= 5):                   ← bounded retry
      releaseLock()                       ← give it back; ECS may replace us
      log.error('connect retries exhausted, releasing lock')
      process.exit(1)
    // Exponential backoff: 200 ms, 400 ms, 800 ms, 1.6 s.
    // (Max attempt count = 5 → max attempts before exhaustion = 4
    // backoffs, so the 5 s ceiling is unreachable at this attempt
    // cap — kept as dead code in case someone bumps MAX_ATTEMPTS
    // and forgets to revisit the cap.)
    backoff: sleep(min(2^attempts * 100ms, 5s))
  }
```

The watchdog runs unconditionally — it covers both the "succeeded
transferLock but failed connect" path here *and* the cold-start
no-peer path (where standby acquires the lock via its own heartbeat
after TTL expires, and then needs to connect). PR 15's chaos suite
explicitly exercises the retries-exhausted branch (kill the Discord
gateway endpoint resolution on the standby, assert the watchdog
gives up after 5 attempts, releases the lock, and exits 1 so ECS
replaces the task). Without that test, the watchdog's failure
ladder is "documented but never executed."

If step 4 fails *inside* the POST handler, the standby returns a
non-2xx response to the active. The active observes the error and
exits anyway (it has nothing better to do — SIGTERM is in flight),
falling through to the cold-start path on the standby's side. The
~7 s cold-fallback floor applies.

**Why direct push beats short-polling:** polling at 100 ms costs 10 reads/s
per standby per shard. At one shard that's negligible, but doesn't compose to
the sharded future. Push-handoff is constant cost and lower latency. DDB
Streams was considered and rejected — up to ~3 s replication lag, doesn't meet
the sub-second target.

**ECS deployment-strategy invariant.** The push-handoff design assumes
exactly one replica is being replaced at a time. A simultaneous-both-
replaced state defeats the whole hot-standby premise — no peer to push
to, both new tasks cold-start, hit the ~7 s floor each, and the second
one might race the first into IDENTIFY-collision. This must be enforced
at the tfvars layer: `bot_gateway`'s deployment config must set
`minimumHealthyPercent = 50` with `maximumPercent = 100` (forces serial
replacement when desired_count = 2) OR use a deployment controller that
serializes replacements explicitly. PR 14 implements this; the
`deployment_circuit_breaker` block on `bot_gateway` in `greenfield.tf`
stays.

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
| Event shipper | App-side feature flag `ENABLE_EVENT_SHIPPER=false` falls back to in-process dispatch on the gateway role. **Valid only through PR 10** — see cliff note below. |
| Cross-process RESUME | App-side feature flag `ENABLE_GATEWAY_RESUME=false` falls back to in-memory session state (Discord IDENTIFY-only every boot). |
| Hot-standby | Set `gateway_desired_count = 1` in tfvars. The standby-discovery path sees no peer and the active becomes a single-replica gateway. |

**Rollback cliff: `ENABLE_EVENT_SHIPPER`.** The flag-based rollback works
only while PR 10 (gateway strip-down) is gated — i.e., the gateway role
still has in-process command handlers wired to fall back to. The moment
PR 10 lands without the flag (the dual-path scaffolding is removed),
flipping `ENABLE_EVENT_SHIPPER=false` has nothing to fall back to and
the gateway boots into a partially-wired state. After that point,
rollback is `git revert` on PR 10 + emergency redeploy, not a flag flip.
The cliff is a deliberate trade-off: keeping the dual-path scaffolding
forever bloats the gateway image and obscures whose code path is live.
PR 10's description must explicitly call out the soak window before
removing the flag (recommendation: 1 week of clean prod traffic on the
event-shipper path before deleting the in-process fallback).

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

### Flow continuity — computation contract

The `silently_dropped_flows` numerator is computed as a difference, not
emitted as a direct event:

```
total_flows            = count(FLOW_CREATED)
completed_flows        = count(FLOW_DELETED)
silently_dropped_flows = total_flows - completed_flows
```

Every `createFlow()` emits `FLOW_CREATED`; every explicit `deleteFlow()`
(terminal stage, abort, admin cleanup) emits `FLOW_DELETED`. TTL-reaped
flows — the silent-drop case the SLI is designed to catch — emit no event
by design, because DDB TTL deletion is asynchronous and a synchronous
"reaped" signal would require a separate sweeper. Counting the difference
captures every unclean drop: process crash mid-flow, worker hang, TTL reap
of an abandoned flow.

The event-shipper observability Phase 1.0 PR reserves the
`FLOW_CREATED` / `FLOW_TRANSITION` / `FLOW_DELETED` audit-event names in
`apps/discord/src/constants.js`. The state-machine harness PR wires the
emissions. The paired CloudWatch metric filters land in
`qurl-integrations-infra` once the harness is producing events in
sandbox.

If a future change adds a "delete-on-TTL-reap" sweeper, it MUST emit a
distinct event (e.g. `FLOW_REAPED`) — not `FLOW_DELETED` — to preserve
the asymmetry the SLI math relies on.

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
