package slackdata

import (
	"context"
	"sync"
	"sync/atomic"
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

// TestCheckRateLimit_ConcurrentSingleUserNeverExceedsBurst fences the
// mutex contract: with the clock frozen (no refill), many goroutines
// minting for ONE user must see EXACTLY burst allows in total. The
// refill-check-consume is a read-modify-write on the user's bucket; if any
// part escaped mintBucketsMu, two goroutines could both observe tokens >= 1
// and overspend. The serial tests assert correctness by construction — this
// is the only one that exercises real contention (run with -race, as CI
// does, to also surface the data race directly).
func TestCheckRateLimit_ConcurrentSingleUserNeverExceedsBurst(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	const goroutines = 64
	const perGoroutine = 4 // 256 attempts, far over the burst of 30.
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				if ok, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != int64(burst) {
		t.Errorf("concurrent single-user allows = %d, want exactly %d — burst breached under contention", got, burst)
	}
}

// TestCheckRateLimit_FractionalAccrualAcrossSubIntervals pins that refill
// is LOSSLESS across gaps shorter than one refill interval. Tokens are
// fractional, so crediting elapsed/refill on each call must accumulate:
// four gaps of refill/4 (each crediting 0.25 of a token — none enough on
// its own) sum to one whole token. An integer/truncating refill would floor
// each sub-interval credit to zero and never reopen the bucket. Quarters
// are exact in IEEE-754, so the boundary is deterministic.
func TestCheckRateLimit_FractionalAccrualAcrossSubIntervals(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	s := newRateLimitStore(&clk)

	// Drain the full bucket at the frozen clock.
	for i := 0; i < burst; i++ {
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
			t.Fatalf("mint %d within burst denied; want allowed", i+1)
		}
	}

	// Three sub-interval gaps accrue 0.25 + 0.25 + 0.25 = 0.75 of a token —
	// under one whole token, so each is still denied.
	quarter := refillInterval / 4
	for i := 0; i < 3; i++ {
		clk = clk.Add(quarter)
		if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); allowed {
			t.Fatalf("sub-interval mint %d allowed at %d%% of a token; fractional credit must not mint early", i+1, (i+1)*25)
		}
	}
	// The fourth quarter completes one whole token — allowed, proving the
	// fractional credits accumulated rather than flooring away.
	clk = clk.Add(quarter)
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
		t.Error("mint after four refill/4 gaps denied; fractional accrual was lost to rounding")
	}
}
