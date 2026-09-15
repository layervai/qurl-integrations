package internal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const testChannelName = "general"

func TestChannelNameCache(t *testing.T) {
	now := fixedNow
	c := newChannelNameCache(10 * time.Minute)
	c.now = func() time.Time { return now }

	c.put("k", testChannelName)
	if name, ok := c.get("k"); !ok || name != testChannelName {
		t.Fatalf("fresh entry: got %q ok=%v", name, ok)
	}
	// A negative entry (empty name from a failed resolve) is still a hit, so the
	// caller skips re-resolving until the TTL lapses.
	c.put("neg", "")
	if name, ok := c.get("neg"); !ok || name != "" {
		t.Fatalf("negative entry should be a hit: got %q ok=%v", name, ok)
	}
	// Expiry.
	now = now.Add(11 * time.Minute)
	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry should miss")
	}
	// Nil receiver is safe (a Handler built without NewHandler just doesn't cache).
	var nc *channelNameCache
	if _, ok := nc.get("k"); ok {
		t.Fatal("nil cache get should miss")
	}
	nc.put("k", "v") // must not panic
}

func TestResolveChannelName(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()

	// No seam wired → empty (describeChannel falls back to the id).
	if got := NewHandler(Config{}).resolveChannelName(ctx, log, "T1", "", "C1"); got != "" {
		t.Fatalf("nil seam should yield empty name, got %q", got)
	}
	// Empty channel id → empty, no seam call.
	if got := NewHandler(Config{ResolveChannelName: func(context.Context, string, string, string) (string, error) {
		t.Fatal("empty channel id must not call the seam")
		return "", nil
	}}).resolveChannelName(ctx, log, "T1", "", ""); got != "" {
		t.Fatalf("empty channel id should yield empty, got %q", got)
	}

	// Success: resolves once, then served from the per-turn-and-beyond cache.
	var calls int
	h := NewHandler(Config{ResolveChannelName: func(context.Context, string, string, string) (string, error) {
		calls++
		return testChannelName, nil
	}})
	if got := h.resolveChannelName(ctx, log, "T1", "", "C1"); got != testChannelName {
		t.Fatalf("resolve = %q, want general", got)
	}
	if got := h.resolveChannelName(ctx, log, "T1", "", "C1"); got != testChannelName || calls != 1 {
		t.Fatalf("second resolve should be cached: got %q calls=%d", got, calls)
	}

	// Answered failure (missing_scope): negative-cached (one call), falls back to empty
	// for the TTL.
	var errCalls int
	he := NewHandler(Config{ResolveChannelName: func(context.Context, string, string, string) (string, error) {
		errCalls++
		return "", errors.New("missing_scope")
	}})
	if got := he.resolveChannelName(ctx, log, "T1", "", "C9"); got != "" {
		t.Fatalf("error resolve should yield empty, got %q", got)
	}
	if got := he.resolveChannelName(ctx, log, "T1", "", "C9"); got != "" || errCalls != 1 {
		t.Fatalf("answered failure should be negative-cached: got %q calls=%d", got, errCalls)
	}

	// Transient ctx timeout/cancel is NOT cached — the next turn re-attempts (only an
	// answered failure takes the long TTL, so one slow response can't suppress the name
	// for the whole window).
	var transientCalls int
	htr := NewHandler(Config{ResolveChannelName: func(context.Context, string, string, string) (string, error) {
		transientCalls++
		return "", context.DeadlineExceeded
	}})
	if got := htr.resolveChannelName(ctx, log, "T1", "", "C7"); got != "" {
		t.Fatalf("transient resolve should yield empty, got %q", got)
	}
	if got := htr.resolveChannelName(ctx, log, "T1", "", "C7"); got != "" || transientCalls != 2 {
		t.Fatalf("a transient ctx error must not be cached (re-attempt each turn), got %q calls=%d", got, transientCalls)
	}

	// A too-long name is bounded before it reaches the prompt.
	long := strings.Repeat("x", maxChannelNameLen+10)
	ht := NewHandler(Config{ResolveChannelName: func(context.Context, string, string, string) (string, error) {
		return long, nil
	}})
	if got := ht.resolveChannelName(ctx, log, "T1", "", "C2"); len(got) != maxChannelNameLen {
		t.Fatalf("name should be truncated to %d, got len %d", maxChannelNameLen, len(got))
	}
}
