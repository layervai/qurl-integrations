# Slack qURL Mint Rate Limit

## Decision

Slack `/qurl get` mint attempts use a DynamoDB-backed per-user fixed-window counter item in the `channel_policies` table.

The gate allows 30 channel-authorized mint attempts per Slack user per workspace per hour. A request only counts after the alias resolves to a channel-authorized resource, so typos and unauthorized aliases do not burn quota. Once the request reaches qurl-service, the attempt is already counted; an upstream mint failure is not refunded.

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
```

The truncated hash is 64 bits. That keeps raw Slack user IDs out of the key while making collision risk negligible at Slack-workspace user counts; a collision would share quota between those users.

Normal in-window requests use a single conditional `UpdateItem`:

```text
ADD mint_count :one
condition: mint_window_start is current window AND mint_count < 30
```

The first request in a new window resets the same item to `mint_count = 1` under a stale-window condition. If the current-window counter is at the limit, the handler renders the normal rate-limit copy with a retry hint for the remaining window. If DynamoDB fails, the command fails closed with the existing generic mint-failure copy.

## Tradeoffs

This is a fixed-window limiter rather than a true sliding-window token bucket. The pre-pivot enforcement was also hour-window shaped, and the operational property that matters for GA is cross-task persistence. The gate counts attempts, not successful upstream creates, because the atomic check-and-consume happens before the mint call.

Counter rows share the `channel_policies` team partition with channel policy rows. `ChannelsForResource` filters them out, but until TTL cleanup lands, the `/qurl list` Edit-modal prefill query still pages over dormant counter rows. Storage remains bounded to one row per `(workspace, user)`, not one row per hour, and native TTL cleanup for inactive counter rows is tracked in layervai/qurl-integrations-infra#1225.
