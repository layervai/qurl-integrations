package internal

import (
	"sync"
	"testing"
	"time"
)

func TestSetupLinkRateLimiterAllow_NilReceiverAllows(t *testing.T) {
	var limiter *setupLinkRateLimiter

	ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", fixedNow)

	if !ok {
		t.Fatal("nil setup-link limiter should allow")
	}
	if retry != 0 {
		t.Fatalf("nil setup-link limiter retry = %s, want 0", retry)
	}
}

func TestSetupLinkRateLimiterAllow_RetryDuration(t *testing.T) {
	limiter := newSetupLinkRateLimiter()
	now := fixedNow
	for i := 0; i < setupLinkRateLimitMax; i++ {
		if ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", now); !ok || retry != 0 {
			t.Fatalf("attempt %d: allow = %v, retry = %s; want allowed with retry 0", i+1, ok, retry)
		}
	}

	ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", now.Add(2*time.Minute))

	if ok {
		t.Fatal("attempt over setup-link limit should be refused")
	}
	if want := 3 * time.Minute; retry != want {
		t.Fatalf("retry = %s, want %s", retry, want)
	}
}

func TestSetupLinkRateLimiterAllow_WindowBoundaryResets(t *testing.T) {
	limiter := newSetupLinkRateLimiter()
	now := fixedNow
	for i := 0; i < setupLinkRateLimitMax; i++ {
		if ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", now); !ok || retry != 0 {
			t.Fatalf("attempt %d: allow = %v, retry = %s; want allowed with retry 0", i+1, ok, retry)
		}
	}

	ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", now.Add(setupLinkRateLimitWindow))

	if !ok {
		t.Fatal("attempt exactly at setup-link window boundary should be allowed")
	}
	if retry != 0 {
		t.Fatalf("retry at setup-link window boundary = %s, want 0", retry)
	}
}

func TestSetupLinkRateLimiterAllow_ConcurrentCap(t *testing.T) {
	limiter := newSetupLinkRateLimiter()
	results := make(chan bool, setupLinkRateLimitMax*4)
	var wg sync.WaitGroup

	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", fixedNow)
			if !ok && retry != setupLinkRateLimitWindow {
				t.Errorf("retry for concurrent refused attempt = %s, want %s", retry, setupLinkRateLimitWindow)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != setupLinkRateLimitMax {
		t.Fatalf("concurrent allowed attempts = %d, want %d", allowed, setupLinkRateLimitMax)
	}
}

func TestSetupLinkRateLimiterAllow_SweepsExpiredEntries(t *testing.T) {
	limiter := newSetupLinkRateLimiter()
	if ok, retry := limiter.allow("T123ABCDEF", "U123ABCDEF", fixedNow); !ok || retry != 0 {
		t.Fatalf("initial allow = %v, retry = %s; want allowed with retry 0", ok, retry)
	}

	if ok, retry := limiter.allow("T999ABCDEF", "U999ABCDEF", fixedNow.Add(setupLinkRateLimitWindow)); !ok || retry != 0 {
		t.Fatalf("post-window allow = %v, retry = %s; want allowed with retry 0", ok, retry)
	}

	if _, ok := limiter.entries["T123ABCDEF\x00U123ABCDEF"]; ok {
		t.Fatal("expired setup-link limiter entry was not swept")
	}
	if _, ok := limiter.entries["T999ABCDEF\x00U999ABCDEF"]; !ok {
		t.Fatal("current setup-link limiter entry missing after sweep")
	}
}
