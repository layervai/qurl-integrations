package slackdata

import (
	"context"
	"testing"
	"time"
)

// burst is the default bucket capacity (== default hourly rate), and
// refillInterval is the time to accrue one token. Both mirror the
// values [Store.CheckRateLimit] derives from the default
// MintRatePerHour, so the assertions track the production policy.
const (
	burst          = mintRatePerHour
	refillInterval = time.Hour / mintRatePerHour
)

// newRateLimitStore returns a Store whose clock is driven by *clk, so
// tests can advance time deterministically without sleeping. It reuses
// the in-package stubDDB/newStore helpers from store_test.go — the
// rate limiter never touches DynamoDB, so the stub's defaults suffice.
// MintRatePerHour is left unset to exercise CheckRateLimit's fallback
// to the package default.
func newRateLimitStore(clk *time.Time) *Store {
	s := newStore(&stubDDB{})
	s.Now = func() time.Time { return *clk }
	return s
}

// TestCheckRateLimit_FirstMintAllowed pins that a user the task has
// never seen starts with a full bucket: the first mint is allowed with
// no retry hint.
func TestCheckRateLimit_FirstMintAllowed(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("first mint denied; want allowed")
	}
	if retry != 0 {
		t.Errorf("retry = %v on an allowed mint, want 0", retry)
	}
}

// TestCheckRateLimit_BurstThenDeny pins the bucket capacity: with the
// clock frozen, exactly burst mints succeed back-to-back and the next
// is denied with a retry of one refill interval (the deficit is a
// whole token).
func TestCheckRateLimit_BurstThenDeny(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	for i := 0; i < burst; i++ {
		allowed, _, err := s.CheckRateLimit(context.Background(), "U1", "T1")
		if err != nil {
			t.Fatalf("mint %d: unexpected error: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("mint %d/%d within burst denied; want allowed", i+1, burst)
		}
	}

	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("mint %d allowed; want denied (over burst)", burst+1)
	}
	if retry != refillInterval {
		t.Errorf("retry = %v, want %v (one full token deficit)", retry, refillInterval)
	}
}

// TestCheckRateLimit_RefillsOverTime pins continuous refill: after the
// bucket is drained, advancing the clock by one refill interval frees
// exactly one token — enough for a single subsequent mint, after which
// the user is denied again.
func TestCheckRateLimit_RefillsOverTime(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	// Drain the full bucket.
	for i := 0; i < burst; i++ {
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
			t.Fatalf("mint %d within burst denied; want allowed", i+1)
		}
	}

	// One refill interval passes → one token back.
	clk = clk.Add(refillInterval)
	allowed, _, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("mint after one refill interval denied; want allowed")
	}

	// The single refilled token is now spent — next is denied again.
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); allowed {
		t.Errorf("second mint after one refill interval allowed; want denied")
	}
}

// TestCheckRateLimit_RefillCapsAtBurst pins that idle time can't
// accrue more than the burst capacity: after draining and idling far
// longer than a full window, the user gets back exactly burst mints —
// not more.
func TestCheckRateLimit_RefillCapsAtBurst(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	for i := 0; i < burst; i++ {
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
			t.Fatalf("mint %d within burst denied; want allowed", i+1)
		}
	}

	// Idle for ten full windows — refill must still cap at the burst.
	clk = clk.Add(10 * burst * refillInterval)
	for i := 0; i < burst; i++ {
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
			t.Fatalf("post-idle mint %d/%d denied; want allowed", i+1, burst)
		}
	}
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); allowed {
		t.Errorf("post-idle mint %d allowed; refill exceeded burst cap", burst+1)
	}
}

// TestCheckRateLimit_PerUserIsolation pins that buckets are keyed per
// slack_user_id: draining one user leaves another user's budget
// untouched.
func TestCheckRateLimit_PerUserIsolation(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	for i := 0; i < burst+5; i++ {
		_, _, _ = s.CheckRateLimit(context.Background(), "U1", "T1")
	}
	// U1 is now over budget; U2 must still be allowed.
	allowed, _, err := s.CheckRateLimit(context.Background(), "U2", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("U2's first mint denied after U1 drained their own bucket; buckets are not per-user")
	}
}

// TestCheckRateLimit_MintRatePerHourOverride pins that the
// MintRatePerHour field overrides the default budget — both burst and
// refill interval derive from it.
func TestCheckRateLimit_MintRatePerHourOverride(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)
	s.MintRatePerHour = 2 // tiny budget: 2/hr → refill every 30m.

	// Two mints allowed, third denied with a 30-minute retry.
	for i := 0; i < 2; i++ {
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
			t.Fatalf("mint %d/2 denied under 2/hr budget; want allowed", i+1)
		}
	}
	allowed, retry, _ := s.CheckRateLimit(context.Background(), "U1", "T1")
	if allowed {
		t.Errorf("third mint allowed under a 2/hr budget; want denied")
	}
	if want := time.Hour / 2; retry != want {
		t.Errorf("retry = %v, want %v (one token at 2/hr)", retry, want)
	}
}
