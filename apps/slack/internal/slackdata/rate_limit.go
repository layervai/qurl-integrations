package slackdata

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// rateLimitStubWarnOnce ensures the "rate-limit gate is a stub" log
// line fires exactly once per process — without this, slog would
// flood with one line per /qurl get mint. Once-only also means the
// alert is visible in dashboards as "this process is degraded"
// rather than "this user just hit the gate".
var rateLimitStubWarnOnce sync.Once

// CheckRateLimit is the in-bot per-user mint-rate gate. Pre-pivot
// this was an HTTP call to qurl-service `/internal/v1/admin/rate-
// limit/check`; post-pivot (Justin's 2026-05-12 review on
// qurl-integrations-infra#523) qurl-service is integration-agnostic
// and doesn't track per-Slack-user mint counts. The rate-limit
// surface stays in-bot.
//
// **STUB IMPLEMENTATION.** This method currently always returns
// (true, 0, nil) — every mint is allowed. The first call emits a
// once-only WARN so operators see the open-gate posture in
// CloudWatch on the first mint after each restart rather than only
// in code review. The pre-pivot enforcement (30 mints per
// slack_user_id per hour, cross-team) needs a new in-bot home; the
// rollout doc proposes either:
//   - In-memory token bucket on the Fargate task (fast; lost on
//     redeploy; per-task not per-workspace).
//   - DDB-backed counter on the workspace_state row (durable;
//     cross-task; one extra write per mint).
//
// Today the bot relies on qurl-service's customer-level rate-limits
// (the API key has its own quota) as a coarser backstop.
//
// TODO: implement once the in-bot rate-limit strategy is picked
// (see SLACK_QURL_ROLLOUT.md — pending design issue).
func (s *Store) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	_ = ctx
	_ = slackUserID
	_ = teamID
	rateLimitStubWarnOnce.Do(func() {
		slog.Warn("slackdata.CheckRateLimit is a no-op stub — every mint is allowed; qurl-service's API-key quota is the only enforcer")
	})
	return true, 0, nil
}
