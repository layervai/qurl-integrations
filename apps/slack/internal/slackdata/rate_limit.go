package slackdata

import (
	"context"
	"time"
)

// Per-user mint rate limiting — strategy and tradeoffs.
//
// CheckRateLimit is the in-bot per-Slack-user mint-rate gate for
// `/qurl get`. Pre-pivot this was an HTTP call to qurl-service
// `/internal/v1/admin/rate-limit/check`; post-pivot (Justin's
// 2026-05-12 review on qurl-integrations-infra#523) qurl-service is
// integration-agnostic and doesn't track per-Slack-user mint counts,
// so the rate-limit surface stays in-bot.
//
// IMPLEMENTATION: an in-memory token bucket per Slack user, held on
// the Store and guarded by a Mutex. Each slack_user_id gets a budget
// of [Store.MintRatePerHour] tokens that refill continuously at one
// token per refill interval (MintRatePerHour tokens per hour). A
// request that resolves to a real, channel-authorized mint spends one
// token; when the bucket is empty the request is denied and the caller
// is told how long until the next token frees up. The rate defaults to
// mintRatePerHour (the pre-pivot enforcement value) and is a Store
// field rather than an env knob so the policy lives in code.
//
// TRADEOFFS (deliberate, see apps/slack/docs/operating.md):
//   - Per-Fargate-task, not global. The bucket lives in the task's
//     memory, so with N tasks behind the load balancer a single user's
//     effective ceiling is ~N×MintRatePerHour/hr (Slack's slash-command
//     routing isn't sticky per user). This is an abuse-rate backstop,
//     not a billing-grade quota — qurl-service's customer-level API-key
//     quota remains the hard cross-task ceiling underneath it.
//   - Counters reset on redeploy/restart. A deploy hands every user a
//     fresh full bucket. Acceptable: the goal is to blunt runaway
//     loops/abuse within a task's lifetime, not to persist a rolling
//     hourly count across deploys.
//
// FUTURE UPGRADE PATH: if a globally-consistent cross-task limit is
// ever required, move the counter to DynamoDB — an atomic conditional
// UpdateItem (ADD on a token-count + last-refill attribute) on the
// channel_policies / workspace row keyed by slack_user_id. That buys
// durability and cross-task consistency at the cost of one extra DDB
// write per mint and added latency on the hot path; it's intentionally
// not done here because the in-memory bucket is sufficient for the
// abuse-backstop goal and adds no per-mint I/O.

// mintRatePerHour is the default per-slack_user_id mint budget per
// hour — both the steady-state rate and the burst capacity. 30/hr is
// the pre-pivot enforcement value the original HTTP gate applied.
// [NewStore] copies this into [Store.MintRatePerHour]; callers can
// override the field to tune the policy.
const mintRatePerHour = 30

// mintBucket is one user's token-bucket state. tokens is fractional so
// partial accrual between calls isn't lost to rounding; last is the
// clock reading at the most recent refill.
type mintBucket struct {
	tokens float64
	last   time.Time
}

// mintBurst is the bucket capacity for this Store — equal to the
// configured hourly rate, so a user idle for an hour accrues a full
// hour's worth of budget but no more. Falls back to the package
// default when the field is unset/non-positive (e.g. a Store built
// field-by-field that didn't set MintRatePerHour).
func (s *Store) mintBurst() float64 {
	if s.MintRatePerHour > 0 {
		return float64(s.MintRatePerHour)
	}
	return mintRatePerHour
}

// mintRefillInterval is the wall-clock time to accrue one token: one
// hour divided across the configured hourly rate.
func (s *Store) mintRefillInterval() time.Duration {
	if s.MintRatePerHour > 0 {
		return time.Hour / time.Duration(s.MintRatePerHour)
	}
	return time.Hour / mintRatePerHour
}

// CheckRateLimit reports whether slackUserID may mint another link
// right now. On denial it returns the wall-clock time until the next
// token is available so the caller can tell the user when to retry.
// teamID is accepted for signature compatibility but is not part of
// the key — the pre-pivot budget was per slack_user_id, cross-team.
func (s *Store) CheckRateLimit(_ context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	_ = teamID

	burst := s.mintBurst()
	refill := s.mintRefillInterval()

	s.mintBucketsMu.Lock()
	defer s.mintBucketsMu.Unlock()

	now := s.nowOrDefault()

	b, ok := s.mintBuckets[slackUserID]
	if !ok {
		// First mint this task has seen from the user: start with a
		// full bucket, then spend one below.
		b = &mintBucket{tokens: burst, last: now}
		if s.mintBuckets == nil {
			// Defensive: NewStore initializes the map, but a Store
			// built field-by-field in a test might not.
			s.mintBuckets = make(map[string]*mintBucket)
		}
		s.mintBuckets[slackUserID] = b
	} else {
		// Continuous refill: credit the tokens accrued since the last
		// observation, capped at the burst capacity.
		elapsed := now.Sub(b.last)
		if elapsed > 0 {
			b.tokens += elapsed.Seconds() / refill.Seconds()
			if b.tokens > burst {
				b.tokens = burst
			}
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0, nil
	}

	// Over budget: time until the fractional balance reaches a whole
	// token. Always > 0 here since tokens < 1.
	deficit := 1 - b.tokens
	retry = time.Duration(deficit * float64(refill))
	return false, retry, nil
}
