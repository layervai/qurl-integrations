package ttlcache

import (
	"errors"
	"testing"
	"time"
)

const (
	filledValue = "filled"
	keptValue   = "keep"
	staleValue  = "stale"
)

func TestCacheHitExpiryAndSweep(t *testing.T) {
	at := time.Unix(1800000000, 0)
	var evicted []string
	cache := New[string](Options[string]{
		SweepEvery: time.Minute,
		OnEvict: func(key string, result Result[string]) {
			evicted = append(evicted, key+"="+result.Value)
		},
	})
	cache.Seed("fresh", Result[string]{Value: keptValue}, time.Minute, at)
	cache.Seed("expired", Result[string]{Value: "drop"}, time.Second, at)

	hit := cache.GetOrStart("fresh", at.Add(30*time.Second))
	if !hit.Hit || hit.Result.Value != keptValue {
		t.Fatalf("fresh hit = %#v, want cached keep", hit)
	}
	if _, ok := cache.entries["expired"]; ok {
		t.Fatal("expired entry should be swept during lookup")
	}
	if len(evicted) != 1 || evicted[0] != "expired=drop" {
		t.Fatalf("evicted = %#v, want expired entry", evicted)
	}

	start := cache.GetOrStart("expired", at.Add(30*time.Second))
	if !start.Owner {
		t.Fatalf("expired key start = %#v, want new owner", start)
	}
}

func TestCacheSweepEveryGatesFullMapSweep(t *testing.T) {
	at := time.Unix(1800000000, 0)
	var swept []time.Time
	var evicted []string
	cache := New[string](Options[string]{
		SweepEvery: time.Minute,
		OnEvict: func(key string, result Result[string]) {
			evicted = append(evicted, key+"="+result.Value)
		},
		OnSweep: func(at time.Time) {
			swept = append(swept, at)
		},
	})
	cache.Seed("expired-other-key", Result[string]{Value: "drop-later"}, time.Second, at)
	cache.Seed("fresh", Result[string]{Value: keptValue}, time.Minute, at)

	first := cache.GetOrStart("fresh", at.Add(2*time.Second))
	if !first.Hit || first.Result.Value != keptValue {
		t.Fatalf("first hit = %#v, want cached keep", first)
	}
	if len(swept) != 1 || !swept[0].Equal(at.Add(2*time.Second)) {
		t.Fatalf("swept after first lookup = %#v, want first lookup time", swept)
	}
	if _, ok := cache.entries["expired-other-key"]; ok {
		t.Fatal("expired entry should be removed by the initial full-map sweep")
	}
	if len(evicted) != 1 || evicted[0] != "expired-other-key=drop-later" {
		t.Fatalf("evicted after first sweep = %#v, want expired other key", evicted)
	}

	cache.Seed("expired-gated-key", Result[string]{Value: "keep-until-next-sweep"}, time.Second, at)
	second := cache.GetOrStart("fresh", at.Add(30*time.Second))
	if !second.Hit || second.Result.Value != keptValue {
		t.Fatalf("second hit = %#v, want cached keep", second)
	}
	if len(swept) != 1 {
		t.Fatalf("sweep count after gated lookup = %d, want 1", len(swept))
	}
	if _, ok := cache.entries["expired-gated-key"]; !ok {
		t.Fatal("expired other-key entry should survive while SweepEvery gates the full-map sweep")
	}

	perKey := cache.GetOrStart("expired-gated-key", at.Add(30*time.Second))
	if !perKey.Owner {
		t.Fatalf("per-key expired lookup = %#v, want new owner", perKey)
	}
	if _, ok := cache.entries["expired-gated-key"]; ok {
		t.Fatal("per-key lookup should evict its own expired entry even when full-map sweep is gated")
	}
	if len(swept) != 1 {
		t.Fatalf("per-key eviction should not run OnSweep; sweep count = %d, want 1", len(swept))
	}
}

func TestCacheCoalescesAndReleasesWaiters(t *testing.T) {
	cache := New[string](Options[string]{})
	at := time.Unix(1800000000, 0)

	owner := cache.GetOrStart("k", at)
	if !owner.Owner || owner.Call == nil {
		t.Fatalf("first start = %#v, want owner", owner)
	}
	waiter := cache.GetOrStart("k", at)
	if waiter.Owner || waiter.Call != owner.Call {
		t.Fatalf("second start = %#v, want waiter on owner call", waiter)
	}

	if cached := cache.Finish("k", owner.Call, Result[string]{Value: filledValue}, time.Minute, at, owner.Generation); !cached {
		t.Fatal("successful fill should be cached")
	}
	select {
	case <-waiter.Call.Done():
	default:
		t.Fatal("waiter was not released")
	}
	if got := waiter.Call.Result(); got.Value != filledValue || got.Err != nil {
		t.Fatalf("waiter result = %#v, want filled", got)
	}

	hit := cache.GetOrStart("k", at.Add(time.Second))
	if !hit.Hit || hit.Result.Value != filledValue {
		t.Fatalf("post-fill hit = %#v, want cached filled", hit)
	}
}

func TestCacheInvalidationDetachesStaleOwner(t *testing.T) {
	cache := New[string](Options[string]{})
	at := time.Unix(1800000000, 0)

	owner := cache.GetOrStart("k", at)
	waiter := cache.GetOrStart("k", at)
	cache.Invalidate("k")
	if cached := cache.Finish("k", owner.Call, Result[string]{Value: staleValue}, time.Minute, at, owner.Generation); cached {
		t.Fatal("stale owner result must not be cached after invalidation")
	}

	<-waiter.Call.Done()
	if got := waiter.Call.Result(); got.Value != staleValue || got.Err != nil {
		t.Fatalf("detached waiter result = %#v, want stale owner result", got)
	}
	next := cache.GetOrStart("k", at.Add(time.Second))
	if !next.Owner {
		t.Fatalf("stale owner result should not be cached; start = %#v", next)
	}
}

func TestCacheSeedDetachesInFlightAndCachesSeed(t *testing.T) {
	cache := New[string](Options[string]{})
	at := time.Unix(1800000000, 0)

	owner := cache.GetOrStart("k", at)
	cache.Seed("k", Result[string]{Value: "seeded"}, time.Minute, at)
	if cached := cache.Finish("k", owner.Call, Result[string]{Value: staleValue}, time.Minute, at, owner.Generation); cached {
		t.Fatal("stale owner result must not be cached after seed")
	}

	hit := cache.GetOrStart("k", at.Add(time.Second))
	if !hit.Hit || hit.Result.Value != "seeded" {
		t.Fatalf("seeded hit = %#v, want seeded value", hit)
	}
}

func TestCacheFinishWithErrorReleasesInFlightWithoutCaching(t *testing.T) {
	cache := New[string](Options[string]{})
	at := time.Unix(1800000000, 0)
	lookupErr := errors.New("lookup panicked")

	owner := cache.GetOrStart("k", at)
	if cached := cache.Finish("k", owner.Call, Result[string]{Err: lookupErr}, 0, at, owner.Generation); cached {
		t.Fatal("zero-TTL error result must not be cached")
	}
	<-owner.Call.Done()
	if !errors.Is(owner.Call.Result().Err, lookupErr) {
		t.Fatalf("owner result err = %v, want %v", owner.Call.Result().Err, lookupErr)
	}

	next := cache.GetOrStart("k", at.Add(time.Second))
	if !next.Owner {
		t.Fatalf("error result should not be cached; start = %#v", next)
	}
}

func TestCacheWithLockHooksShareSidecarSynchronization(t *testing.T) {
	at := time.Unix(1800000000, 0)
	sidecar := map[string]bool{}
	cache := New[string](Options[string]{
		OnEvict: func(key string, _ Result[string]) {
			delete(sidecar, key)
		},
	})

	cache.Seed("k", Result[string]{Value: "old"}, time.Minute, at)
	WithLock(cache, func() {
		sidecar["k"] = true
	})
	cache.Seed("k", Result[string]{Value: "new"}, time.Minute, at.Add(time.Second))

	if sidecar["k"] {
		t.Fatal("overwriting a cached entry should run sidecar eviction")
	}

	InvalidateWith(cache, "k", func() {
		sidecar["k"] = false
	})
	if sidecar["k"] {
		t.Fatal("InvalidateWith sidecar hook did not run under the cache lock")
	}
}
