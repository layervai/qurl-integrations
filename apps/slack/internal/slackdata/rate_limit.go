package slackdata

import (
	"context"
	"time"
)

// CheckRateLimit is the in-bot per-user mint-rate gate. Pre-pivot
// this was an HTTP call to qurl-service `/internal/v1/admin/rate-
// limit/check`; post-pivot (Justin's 2026-05-12 review on
// qurl-integrations-infra#523) qurl-service is integration-agnostic
// and doesn't track per-Slack-user mint counts. The rate-limit
// surface stays in-bot.
//
// **STUB IMPLEMENTATION.** This method currently always returns
// (true, 0, nil) — every mint is allowed. The pre-pivot enforcement
// (30 mints per slack_user_id per hour, cross-team) needs a new
// in-bot home; the rollout doc proposes either:
//   - In-memory token bucket on the Fargate task (fast; lost on
//     redeploy; per-task not per-workspace).
//   - DDB-backed counter on the workspace_state row (durable;
//     cross-task; one extra write per mint).
//
// Neither is wired here yet — the call site (handler_get.go's
// `aliasesWork` second-step) still threads `allowed, retry, err`
// through, so when the in-bot enforcer lands the call shape stays
// the same. Today the bot relies on qurl-service's customer-level
// rate-limits (the API key has its own quota) as a coarser
// backstop.
//
// TODO: implement once the in-bot rate-limit strategy is picked
// (see SLACK_QURL_ROLLOUT.md — pending design issue).
func (s *Store) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	_ = ctx
	_ = slackUserID
	_ = teamID
	return true, 0, nil
}
