package internal

import (
	"sync"
	"time"
)

const (
	setupLinkRateLimitWindow = 5 * time.Minute
	setupLinkRateLimitMax    = 3
)

type setupLinkRateLimiter struct {
	mu        sync.Mutex
	entries   map[string]setupLinkRateLimitEntry
	lastSweep time.Time
}

type setupLinkRateLimitEntry struct {
	windowStart time.Time
	count       int
}

func newSetupLinkRateLimiter() *setupLinkRateLimiter {
	return &setupLinkRateLimiter{entries: make(map[string]setupLinkRateLimitEntry)}
}

func (l *setupLinkRateLimiter) allow(teamID, userID string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	key := teamID + "\x00" + userID

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	entry := l.entries[key]
	if entry.windowStart.IsZero() || !now.Before(entry.windowStart.Add(setupLinkRateLimitWindow)) {
		l.entries[key] = setupLinkRateLimitEntry{windowStart: now, count: 1}
		return true, 0
	}
	if entry.count >= setupLinkRateLimitMax {
		retry := entry.windowStart.Add(setupLinkRateLimitWindow).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return false, retry
	}
	entry.count++
	l.entries[key] = entry
	return true, 0
}

func (l *setupLinkRateLimiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < setupLinkRateLimitWindow {
		return
	}
	for key, entry := range l.entries {
		if !now.Before(entry.windowStart.Add(setupLinkRateLimitWindow)) {
			delete(l.entries, key)
		}
	}
	l.lastSweep = now
}
