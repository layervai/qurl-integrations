# Zero-downtime upgrades for the qURL Discord bot

## Summary

The bot separates interaction state from Gateway connection state. The current
`/qurl send` takes its attachment as a slash-command option; `/qurl map` takes a
location. Their confirmation interactions, setup and revoke menus use the
persisted flow dispatcher. They do not wait for a later DM file drop or depend
on a wizard Promise stored in the Gateway process.

A process restart can still interrupt work already in progress, miss an
interaction ACK deadline or break a Gateway session. Persisted flow state makes
later interactions resumable; it does not make arbitrary HTTP work, child-link
minting or DM delivery exactly once. Those paths need their own conditional
writes, compensation and delivery records.

This document describes the source contracts and the Gateway RESUME/handoff
design. It does not establish which flags are enabled in a managed environment
or promise a measured outage duration. Deployment, replay, cleanup and rollback
checks must establish that behavior on the selected image and task bindings.

## What a deploy must preserve

1. **Interaction admission.** An incoming slash command, component or modal must
   reach a handler and be acknowledged within its deadline.
2. **Flow continuity.** The next interaction must load the surviving row and
   validate its stage, identity and expiry; handler transitions then use the
   loaded version as a condition.
3. **Gateway continuity.** The new active process must recover the session and
   dispatch checkpoint, or use the explicit fresh-session fallback. Presence and
   replay behavior need a real handoff check.

The flow-state work addresses the second requirement. The event queue separates
business handlers from the Gateway connection. The remaining session and
leadership requirements are covered by Pillars 2 and 3 below.

## Process roles

With the event-shipper split enabled:

```text
Discord Gateway
    │
    ▼
Gateway process ── SQS Standard ──► worker process
    │                                  │
    │ session/checkpoint and lease      │ flow/recipient state
    ▼                                  ▼
DynamoDB                            DynamoDB
                                       │
                                       ▼
                                Discord REST replies
```

The producer in [`event-publisher.js`](../src/event-publisher.js) emits
`0:<sequence>` today. Its shard component is still the literal `0`, with an
open `TODO(sharding-pr)` to supply the real per-shard value. The consumer in
[`event-consumer.js`](../src/event-consumer.js) currently reconstructs
`INTERACTION_CREATE` through discord.js and the shared interaction listener.
Other event types are logged and deleted; there is no DM-file-capture consumer.
A Gateway connection is still needed for the selected transport and presence.

Worker rolling replacement must admit the replacement before stopping the old
worker and allow bounded in-flight work to drain. Gateway replacement also
needs the lease, session snapshot, dispatch checkpoint and handoff rules below.
The Gateway-to-DynamoDB edge applies only when the corresponding RESUME and
hot-standby switches are enabled. A warm replacement alone does not transfer
those states. Configuration and
normal CI determine the deployed roles and rollout order.

## The three pillars

### Pillar 1 — Persistent interaction state

[`flow-state.js`](../src/flow-state.js) provides `createFlow`, `loadFlow`,
`transitionFlow`, `deleteFlow` and `supersedeOrCreate`. [`flow-id.js`](../src/flow-id.js) builds and
parses the canonical `<shard_id>#<guild_id>#<channel_id>#<user_id>` key.
[`flow-dispatch.js`](../src/flow-dispatch.js) resolves component/modal routing
from the interaction context and loads the matching row before invoking a
registered handler. A custom ID selects a route; it is not an authorization
identity.

| Attribute | Purpose |
|---|---|
| `flow_id` | Canonical shard, guild, channel and user key |
| `stage` | Registered handler stage, such as `awaiting_send_confirm` |
| `payload` | Encrypted flow data needed by a later interaction |
| `version` | Expected-version condition for a transition |
| `created_at` | Creation time |
| `expires_at` | Application expiry and asynchronous DynamoDB TTL cleanup |

The reader enforces expiry; TTL removal is not the authorization boundary.
Payload encryption requires the configured encryption key. A transition must
use the loaded version, and a conditional failure must be handled before
sending a message or doing another external action. A row can survive process
replacement while the interaction token and any in-flight request still have
their own deadlines.

The current command registrations use `awaiting_send_confirm`,
`awaiting_revoke_select`, `awaiting_setup_button` and `awaiting_setup_modal`.
Send/map confirmation state replaces the old attachment-capture wizard. Do not
restore an `awaiting_file` flow or route regular DM messages to that removed
front half.

| Command | Existing-flow policy |
|---|---|
| `/qurl revoke` | Supersede the prior revoke menu through the guarded delete/recreate path. |
| `/qurl setup` | Supersede the pre-modal button stage; preserve an active setup modal. |
| Send/map confirmation | Supersede a prior `awaiting_send_confirm` row through its stage/version-guarded claim; return a surviving sibling stage without deleting it. |

`supersedeOrCreate` tries creation, reads a conflicting row, conditionally
claims the same-stage version and retries creation. This is a sequence of
writes, not one transaction; a lost race returns the surviving row. Send/map
confirmation uses this helper and the registered dispatcher handlers. The deleted multi-step send wizard is not a separate command or a
second flow contract. Stage/version guards prevent one command from deleting a
sibling command's surviving row.

### Queue delivery and side effects

SQS Standard can redeliver an event. The consumer's recent-event LRU is a
telemetry hint, not a correctness gate: a repeated event may be the first
attempt that can finish. The event ID format is `<shard>:<sequence>`, but the publisher still supplies
literal `0`. Sharding must replace that placeholder; flow keys separately use
the configured `k:n` shard format. These values are not compared with each other.

The worker tracks the asynchronous handler after discord.js reconstructs the
interaction. Queue deletion and shutdown draining follow that handler lifecycle;
a synchronous event emission is not completion. See the executable consumer
tests for dispatch failures, stale messages, duplicate observations and drain
behavior.

Flow OCC, guarded recipient writes and endpoint-specific handling must protect
side effects. A second interaction callback rejection alone does not establish
exactly-once minting or delivery. A crash after a remote effect but before local
confirmation remains a path that needs an explicit recovery check.

#### Trust boundary

The worker trusts the queue payload's user, member permissions and channel
fields as Discord-authenticated when it reconstructs the interaction. It does
not authenticate that identity independently. This contract is safe only while
`sqs:SendMessage` is limited to the intended Gateway task role. Consumption and
deletion must be limited to the intended worker role.

Verify the selected queue policy and task bindings; source layout alone does
not establish those live grants. Keep this contract aligned with the
infrastructure `qurl-bot-events` module README. Adding another publisher requires
envelope validation and authenticity verification before its messages can reach
`client.actions.InteractionCreate.handle`. Review the identity and permission
trust boundary before granting that publish access.

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
| `session_shard_id` | number | shard index from `@discordjs/ws` `SessionInfo` |
| `session_shard_count` | number | shard count from `@discordjs/ws` `SessionInfo` |
| `updated_at` | number | epoch ms |

Writes are throttled: `updateSessionInfo` fires on every dispatch (high rate),
but we only write to DDB on `READY` (rare) and at most once per second for
sequence updates. The sequence is conservatively flushed final-time on
SIGTERM. The 60 s Discord buffer is more than enough headroom for a worst-case
write latency of a few seconds.

On boot, the standby's `retrieveSessionInfo` returns the persisted row,
including `shardId` and `shardCount`. The pinned `@discordjs/ws@1.2.3`
implementation requires the stored `shardCount` to equal the manager's shard
count before it chooses the RESUME path. Legacy rows without the explicit shard
columns derive both values from the existing `shard_id = "k:n"` primary key.
`@discordjs/ws` then issues `RESUME` (op 6) instead of `IDENTIFY` (op 2). If
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
production code counts grants at `@discordjs/ws`'s identify-throttler boundary,
fails readiness when the N+1th consecutive identify exceeds the cap without a
successful READY or RESUMED,
blocks the over-budget grant until shard abort, and starts a bounded process
exit so ECS replaces the task. Session retrieval is deliberately not a proxy
for IDENTIFY: the library also reads session state during normal heartbeat and
dispatch processing.

The current cap is one grant between READY/RESUMED events. This deliberately
trades a task replacement after even one benign pre-READY session rejection
for the smallest quota burn during token contention. ECS replacement/backoff
and the gateway lease bound recovery; increasing the cap requires revisiting
that quota tradeoff rather than silently adding retries.

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
6. Run transferLock atomically (active → standby), then POST
   /control/yours to the peer with body {active_instance_id,
   expected_version} signed with HMAC. The standby adopts the new
   lock version and starts or accepts the one active WebSocket lifecycle.
7. Wait for ACK or 200 ms timeout. ACK means the standby accepted the
   lock and owns the connection path. Usually connect() has resolved.
   If @discordjs/ws was already recovering, the ACK means that recovery
   remains authoritative and the watchdog is enforcing its time limit.
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

The IDENTIFY-budget fatal path deliberately does not push a handoff: the task
has not observed a successful `READY` or `RESUMED`, so it has no confirmed live
session to transfer. It stops dispatch and exits, leaving the peer to acquire
after the same 6 s lock TTL. The resulting takeover remains bounded by the
same ~7 s best-case / ~9 s worst-case cold-fallback window above.

**Standby on `POST /control/yours`:**

```
1. Verify HMAC signature on the request body (+ ts freshness + nonce
   not-seen)
2. Verify body.active_instance_id matches a known peer (avoids accidental
   takeovers from a stale stopped task)
3. adoptLockFromHandoff(body.expected_version)
   ← initializes the local version cursor for the lock already transferred
4. If @discordjs/ws is recovering, keep that single recovery path.
   Otherwise call WebSocketManager.connect(); it retrieves the saved
   session and sends RESUME.
5. ACK the control request after connect() resolves, or after the existing
   automatic recovery is accepted as the authoritative connection path.
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
every 1 s:
  if manager.isConnected():
    if !heldLock:
      holder = stronglyReadCurrentHolder()
      if holder belongs to a peer and its lease is not expired:
        deleteOwnHeartbeatRow(); process.exit(1)   ← confirmed split brain
      return                                        ← leader can reacquire absent row
    return                                          ← healthy active steady state
  if manager.isRecovering():                      ← checked on both replicas
    wait without calling connect()                ← avoid duplicate shard
    if recovery elapsed time >= 20 s:
      if heldLock: releaseLock()
      deleteOwnHeartbeatRow(); process.exit(1)     ← replace stuck process
    return
  else if heldLock:
    attempts += 1
    try { await manager.connect() }
    catch (err) {
      if (attempts >= 5):                 ← bounded retry
        releaseLock()                     ← give it back; ECS may replace us
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

An expired peer-owned row is not proof of a live owner. The leader can replace
it under the lock CAS contract. If that peer is still connected, its watchdog
then sees the new live lease and exits that losing replica. The 20 s automatic-
recovery stand-down also means the normal ~7 s cold-fallback floor does not
apply to a shard that stays inside the library recovery state. This bounded
dark window is intentional: racing the library would recreate the duplicate-
delivery incident. Promotion and lock reacquisition do not reset the timer, so
20 s is the process-wide bound for one continuous recovery episode.

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

The current design makes these transport and deployment choices:

- **Discord HTTPS Interactions endpoint.** The removed DM file-drop wizard
  no longer rules this out. It is a separate transport choice that needs review
  of signature validation, ACK handling, command registration and presence. This
  design retains Gateway delivery and does not implement that migration.
- **Rolling overlap on the gateway service alone.** A replacement task does not
  by itself transfer lease ownership, the session or the dispatch checkpoint.
  Keep explicit leadership and RESUME/handoff rather than assuming that two
  ready tasks provide continuity.
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
| Event shipper | App-side feature flag `ENABLE_EVENT_SHIPPER=false` falls back to in-process dispatch on the gateway role. **Valid through the gated phase** — see cliff note below. |
| Cross-process RESUME | App-side feature flag `ENABLE_GATEWAY_RESUME=false` falls back to in-memory session state (Discord IDENTIFY-only every boot). |
| Hot-standby | Set `gateway_desired_count = 1` in tfvars. The standby-discovery path sees no peer and the active becomes a single-replica gateway. |

**Rollback cliff: `ENABLE_EVENT_SHIPPER`.** PR 10 ships the producer +
worker-side dispatch path under the `ENABLE_EVENT_SHIPPER` flag,
keeping the legacy in-process gateway dispatch as fall-back. While
both code paths coexist, `ENABLE_EVENT_SHIPPER=false` is a valid
flag-flip rollback. A later cleanup PR will delete the legacy
in-process gateway listener gate `(isGateway && !config.ENABLE_EVENT_SHIPPER)`
and make the producer unconditional. After that cleanup lands,
flipping the flag false has nothing to fall back to and rollback is
`git revert` + emergency redeploy. The cliff is a deliberate trade-off:
keeping the dual-path scaffolding forever bloats the gateway image
and obscures whose code path is live. The cleanup PR's description
must explicitly call out the soak window before removing the flag
(recommendation: 1 week of clean prod traffic on the event-shipper
path before deleting the in-process fallback).

**Unsupported combination:** `PROCESS_ROLE=combined` paired with
`ENABLE_EVENT_SHIPPER=true` is rejected at boot
(`unsupportedRoleShipperCombo` in `src/boot-requirements.js`). In
combined mode the gateway publish hook AND the worker consumer
would arm in one process, double-dispatching every interaction
(in-process WS frame + SQS round-trip). The supported flag-on
shape is the two-process split: `PROCESS_ROLE=gateway` (singleton,
publishes) + `PROCESS_ROLE=http` (consumes, can scale horizontally).
For local dev / sandbox in one process, leave `ENABLE_EVENT_SHIPPER`
unset.

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
the infrastructure repository once the harness is producing events in
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
