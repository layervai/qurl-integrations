// Package ttlcache provides a small keyed TTL cache with per-key
// singleflight fills and generation-guarded invalidation.
package ttlcache

import (
	"sync"
	"time"
)

// Result is the value/error pair shared with waiters when an in-flight fill
// finishes. Callers choose which results are worth caching by passing a
// positive TTL to Finish.
type Result[V any] struct {
	Value V
	Err   error
}

// Options configures a Cache.
type Options[V any] struct {
	// SweepEvery gates full-map expiry sweeps. A zero or negative value sweeps
	// on every GetOrStart call.
	SweepEvery time.Duration

	// OnEvict runs while the cache mutex is held after a cached entry is
	// removed or overwritten. It is intended for same-key sidecar cleanup; it
	// must not call back into the same Cache.
	OnEvict func(key string, result Result[V])

	// OnSweep runs while the cache mutex is held after each full-map expiry
	// sweep. It is intended for cache-adjacent sidecar sweeps; it must not call
	// back into the same Cache.
	OnSweep func(at time.Time)
}

type entry[V any] struct {
	result    Result[V]
	expiresAt time.Time
}

// Call represents one in-flight fill for a key.
type Call[V any] struct {
	done   chan struct{}
	result Result[V]
}

// Done closes when the fill owner has published its result.
func (c *Call[V]) Done() <-chan struct{} {
	return c.done
}

// Result returns the fill result. Call it after Done is closed.
func (c *Call[V]) Result() Result[V] {
	return c.result
}

// Start describes whether a caller hit cache, owns a new fill, or should wait
// on an existing fill.
type Start[V any] struct {
	Result     Result[V]
	Hit        bool
	Call       *Call[V]
	Owner      bool
	Generation uint64
}

// Cache is safe for concurrent use. It intentionally uses one mutex: fills run
// outside the lock, so the lock only serializes short map operations while
// avoiding the extra complexity of keyed or sharded locks. Per-key generation
// counters are retained for the process lifetime; invalidation can detach an
// old fill owner, and retaining the advanced generation prevents that owner
// from caching stale data if it finishes later.
type Cache[V any] struct {
	mu         sync.Mutex
	entries    map[string]entry[V]
	inFlight   map[string]*Call[V]
	generation map[string]uint64
	lastSweep  time.Time
	sweepEvery time.Duration
	onEvict    func(key string, result Result[V])
	onSweep    func(at time.Time)
}

// New constructs an empty cache. The zero value of Cache is also usable when no
// options are needed.
func New[V any](opts Options[V]) *Cache[V] {
	return &Cache[V]{
		sweepEvery: opts.SweepEvery,
		onEvict:    opts.OnEvict,
		onSweep:    opts.OnSweep,
	}
}

// GetOrStart returns a fresh cached result, an existing in-flight call, or a new
// owner call for key.
func (c *Cache[V]) GetOrStart(key string, at time.Time) Start[V] {
	start, _ := GetOrStartWith[V, struct{}](c, key, at, nil)
	return start
}

// GetOrStartWith is GetOrStart plus an under-lock hook for callers with
// cache-adjacent side state that must be observed atomically with cache lookup.
func GetOrStartWith[V any, D any](c *Cache[V], key string, at time.Time, underLock func() D) (start Start[V], data D) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initLocked()
	c.sweepExpiredLocked(at)
	if cached, ok := c.entries[key]; ok {
		if at.Before(cached.expiresAt) {
			return Start[V]{Result: cached.result, Hit: true}, data
		}
		c.evictLocked(key, cached)
	}

	if underLock != nil {
		data = underLock()
	}

	generation := c.generation[key]
	if call, ok := c.inFlight[key]; ok {
		return Start[V]{Call: call}, data
	}

	call := &Call[V]{done: make(chan struct{})}
	c.inFlight[key] = call
	return Start[V]{Call: call, Owner: true, Generation: generation}, data
}

// Finish publishes an in-flight fill result, releases waiters, and returns
// whether the result was cached. Results are cached only when ttl is positive
// and the generation still matches.
func (c *Cache[V]) Finish(key string, call *Call[V], result Result[V], ttl time.Duration, at time.Time, generation uint64) (cached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initLocked()
	if ttl > 0 && c.generation[key] == generation {
		c.storeLocked(key, result, at.Add(ttl))
		cached = true
	}
	call.result = result
	if c.inFlight[key] == call {
		delete(c.inFlight, key)
	}
	close(call.done)
	return cached
}

// Invalidate removes cached and in-flight state for key and advances its
// generation so detached fill owners cannot repopulate stale data.
func (c *Cache[V]) Invalidate(key string) {
	InvalidateWith(c, key, nil)
}

// InvalidateWith is Invalidate plus an under-lock hook for sidecar state.
func InvalidateWith[V any](c *Cache[V], key string, underLock func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initLocked()
	if cached, ok := c.entries[key]; ok {
		c.evictLocked(key, cached)
	}
	delete(c.inFlight, key)
	c.generation[key]++
	if underLock != nil {
		underLock()
	}
}

// Seed replaces key with a cached result and detaches any in-flight owner.
func (c *Cache[V]) Seed(key string, result Result[V], ttl time.Duration, at time.Time) {
	SeedWith(c, key, result, ttl, at, nil)
}

// SeedWith is Seed plus an under-lock hook for sidecar state.
func SeedWith[V any](c *Cache[V], key string, result Result[V], ttl time.Duration, at time.Time, underLock func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initLocked()
	if cached, ok := c.entries[key]; ok {
		c.evictLocked(key, cached)
	}
	delete(c.inFlight, key)
	c.generation[key]++
	if ttl > 0 {
		c.storeLocked(key, result, at.Add(ttl))
	}
	if underLock != nil {
		underLock()
	}
}

// WithLock runs fn while holding the cache mutex. It is for cache-adjacent
// sidecars that must share this cache's synchronization.
func WithLock[V any](c *Cache[V], fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initLocked()
	if fn != nil {
		fn()
	}
}

func (c *Cache[V]) initLocked() {
	if c.entries == nil {
		c.entries = map[string]entry[V]{}
	}
	if c.inFlight == nil {
		c.inFlight = map[string]*Call[V]{}
	}
	if c.generation == nil {
		c.generation = map[string]uint64{}
	}
}

func (c *Cache[V]) sweepExpiredLocked(at time.Time) {
	if c.sweepEvery > 0 && !c.lastSweep.IsZero() && at.Sub(c.lastSweep) < c.sweepEvery {
		return
	}
	for key, cached := range c.entries {
		if !at.Before(cached.expiresAt) {
			c.evictLocked(key, cached)
		}
	}
	if c.onSweep != nil {
		c.onSweep(at)
	}
	c.lastSweep = at
}

func (c *Cache[V]) storeLocked(key string, result Result[V], expiresAt time.Time) {
	if cached, ok := c.entries[key]; ok {
		c.evictLocked(key, cached)
	}
	c.entries[key] = entry[V]{
		result:    result,
		expiresAt: expiresAt,
	}
}

func (c *Cache[V]) evictLocked(key string, cached entry[V]) {
	delete(c.entries, key)
	if c.onEvict != nil {
		c.onEvict(key, cached.result)
	}
}
