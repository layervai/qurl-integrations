# Slack qURL Mint Rate Limit

## Decision

Slack `/qurl get` mint attempts use a DynamoDB-backed per-user fixed-window counter item in the `channel_policies` table.

The gate allows 30 successfully resolved mint attempts per Slack user per workspace per hour. A request only counts after the alias resolves to a channel-authorized resource, so typos and unauthorized aliases do not burn quota.

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
slack_channel_id = rate_limit#<sha256(slack_user_id)[:16]>
mint_window_start = <hour_window_start_unix>
mint_count = <number>
```

Normal in-window requests use a single conditional `UpdateItem`:

```text
ADD mint_count :one
condition: mint_window_start is current window AND mint_count < 30
```

The first request in a new window resets the same item to `mint_count = 1` under a stale-window condition. If the current-window counter is at the limit, the handler renders the normal rate-limit copy with a retry hint for the remaining window. If DynamoDB fails, the command fails closed with the existing generic mint-failure copy.

## Tradeoffs

This is a fixed-window limiter rather than a true sliding-window token bucket. The pre-pivot enforcement was also hour-window shaped, and the operational property that matters for GA is cross-task persistence. Counter keys are hashed so raw Slack user IDs are not embedded in DynamoDB keys.
