# Discord dispatch probe (#1383)

`ENABLE_DISPATCH_PROBE=true` enables one expected Discord reply per active gateway process per minute. The default is false. It does not use ambient guild traffic or send messages.

The gateway waits 60 seconds after startup or an observed disconnect. It sends Gateway opcode 8 with a fresh nonce and only the bot's own user ID in one guild. A pure RESUMED boot fetches the current bot and one guild through REST. The existing GuildMembers intent is required. See [Discord's Request Guild Members contract](https://docs.discord.com/developers/events/gateway-events#request-guild-members).

In combined mode the normal Discord SDK member-chunk event records the result. In event-shipping mode only the matching expected chunk enters the existing SQS publisher. The worker loads the current probe row, checks its guild, bot, nonce and deadline, then calls the pinned Discord SDK chunk handler. It writes an ACK only after the SDK emits the expected public event. A raw gateway reply alone cannot pass the queue/worker probe. Ordinary member chunks stay off the queue.

One row per gateway process goes in `${DDB_TABLE_PREFIX}dispatch-probe`. It contains a random process UUID, nonce, bot and guild IDs, start/deadline/ACK times, and `expires_at`. There are no participant IDs, messages, tokens, scans, or history. The deadline is 30 seconds. Logical expiry is checked in code; DynamoDB's 120-second TTL is asynchronous cleanup and can retain expired rows for days. Infra also requires point-in-time backup, set to the minimum one-day recovery window for this table. Duplicate ACKs preserve the first time and cannot extend TTL. A new nonce replaces the row at the next probe. DDB calls and WebSocket submission each have a 3-second bound.

The gateway emits four audit events without IDs or nonce dimensions:

- `gateway_dispatch_probe_ok`: expected SDK result returned before the deadline; includes `latency_ms`.
- `gateway_dispatch_probe_missed`: an issued probe has no timely result. This is the only paging input.
- `gateway_dispatch_probe_unavailable`: no eligible guild. This is unknown coverage, not proof of a healthy dispatch path.
- `gateway_dispatch_probe_error`: request, storage, or SDK work failed. A pending expected probe still expires and can page.

The existing ACK-age heartbeat and silence alarms stay in place. This probe does not alter HTTP readiness or force task restarts. The infra alarm requires missed probes in two of three one-minute periods. Missing data is not breaching, so a quiet guild or standby does not page.

## Rollout and incident owner

The Discord integration operator owns the probe and its alarm. Source changes are not deployment proof. Keep issue #1383 open until both deployment modes and the alarm have approved live receipts.

1. Review the short-lived operational row fields, one-day backup window, and TTL cleanup limit. Apply the paired infra bootstrap table first, then refresh the bot task IAM from bootstrap outputs. The existing table-scoped data-plane policy supplies GetItem/PutItem/UpdateItem; no Scan or new control port is needed.
2. Deploy probe-aware worker images before gateway images. Keep the flag false during mixed worker versions: an old worker deletes an unknown member-chunk envelope and cannot ACK it.
3. Enable the shared Terraform flag `dispatch_probe_enabled` for the gateway and workers. The plan rejects an absent probe table. The alarm uses the existing Phase 1 SNS route; enabling it can notify people and requires the normal approval.
4. In an approved test guild, confirm successful quiet-period probes in combined and shipping modes. Confirm a worker/queue block causes misses while heartbeat ACKs stay healthy. Confirm duplicate delivery and startup/standby grace. Restore the path and confirm recovery. Do not post or run Discord requests from a local fixture test.
5. On a page, check gateway connectivity and the four probe events, SQS age/backlog, worker errors and DDB permissions. A timely raw reply without a worker ACK narrows the fault to publishing, queuing or processing. Do not page on `activity_age_ms` or enable a restart loop as a substitute.
6. To roll back, disable the shared flag and alarm actions before downgrading workers. Wait for probe envelopes to drain before old workers resume. Retain the table until old processes are gone; its logical deadlines reject stale results.
