# Slack qURL Mint Rate Limit

## Decision

Slack `/qurl get` mint attempts use a DynamoDB-backed per-user fixed-window counter item in the `channel_policies` table.

The gate allows 30 channel-authorized mint attempts per Slack user per workspace per hour. A request only counts after the alias resolves to a channel-authorized resource, so typos and unauthorized aliases do not burn quota. Once the request reaches qurl-service, the attempt is already counted; an upstream mint failure is not refunded.

The quota is intentionally per `(workspace, Slack user)`, not a global cross-workspace human quota. Issue #400 is about restoring the Slack bot's per-user mint backstop after the pivot while keeping qurl-service integration-agnostic; using the Slack team partition matches that workspace-scoped ownership and keeps the qurl-service API-key quota as a separate workspace-level backstop.

## Why DynamoDB

The issue considered three strategies:

- In-memory token bucket per Fargate task.
- DynamoDB counter on workspace state.
- Lambda Function URL with EventBridge throttle policy.

DynamoDB is the chosen strategy because it preserves enforcement across task restarts and across multiple Fargate tasks while keeping the bot code and infrastructure surface small. The existing `channel_policies` table already has the Slack team partition and a sort key, so it can hold counter items without adding a table or coupling mint enforcement to the admin workspace row. The qurl-service API-key quota remains a workspace-level backstop, but it is not a replacement for this Slack-user gate.

## Shape

For each Slack user in a workspace, the bot writes one counter item:

```text
slack_team_id = <team_id>
slack_channel_id = rate_limit#<first 16 hex chars of sha256(slack_user_id)>
mint_window_start = <hour_window_start_unix>
mint_count = <number>
ttl = <epoch seconds for inactive counter cleanup>
```

The truncated hash is 64 bits. That keeps raw Slack user IDs out of the key while making collision risk negligible at Slack-workspace user counts; a collision would share quota between those users. This is key obfuscation, not a privacy boundary: `slack_team_id` remains stored as the plaintext partition key.

Once a counter item exists for the current window, normal in-window requests use a single conditional `UpdateItem`:

```text
ADD mint_count :one
condition: mint_window_start is current window AND mint_count < 30
```

The first request for a brand-new or stale window takes the slower path: a conditional increment misses, a consistent `GetItem` disambiguates the state, and a reset writes the same item with `mint_count = 1` under `attribute_not_exists(mint_window_start) OR mint_window_start < :window`. If the current-window counter is at the limit, the handler renders the normal rate-limit copy with a retry hint for the remaining window. Same-window post-cap attempts take a conditional increment miss plus a consistent `GetItem`, and the handler logs the denial so operators can see cap pressure.

Every counter write refreshes `ttl` to two hours after the counter window starts. Current Terraform has not enabled native TTL on `channel_policies` yet, but the writer contract is in place so layervai/qurl-integrations-infra#1225 can be a Terraform-only fast-follow.

Future-window races follow the already-written future window instead of resetting the item backward. A future-window cap costs the initial conditional miss plus the consistent `GetItem`; an under-limit future-window read whose follow-up increment loses a race adds that second conditional increment. Both future-window denial paths compute the retry hint from that future window's end, so it can exceed one hour. If DynamoDB fails, the command fails closed with the existing generic mint-failure copy.

## Tradeoffs

This is a fixed-window limiter rather than a true sliding-window token bucket. The pre-pivot enforcement was also hour-window shaped, and the operational property that matters for GA is cross-task persistence. The sharp edge is the normal fixed-window boundary burst: a user can mint up to 30 times near the end of one hour and up to 30 more at the start of the next. The gate counts attempts, not successful upstream creates, because the atomic check-and-consume happens before the mint call.

The 30/hour limit and one-hour window are compile-time constants. That is deliberate for this release because they preserve the pre-pivot policy without adding a runtime tuning surface; changing them requires a redeploy.

Counter rows share the `channel_policies` team partition with channel policy rows. `ChannelsForResource` filters them out, but until Terraform enables TTL cleanup, the `/qurl list` Edit-modal prefill query and `/qurl revoke` channel-binding sweep still page over dormant counter rows. Storage remains bounded to one row per `(workspace, user)`, not one row per hour. Native TTL cleanup for inactive counter rows is tracked as the fast-follow layervai/qurl-integrations-infra#1225.
