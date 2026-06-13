package internal

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// channelNameTTL bounds how long a resolved channel name (or a cached resolve
	// failure) is reused. A rename isn't reflected until the entry expires — fine
	// for a readability nicety, and it caps conversations.info to at most one call
	// per channel per TTL. Answered failures are cached too, so a workspace lacking the
	// channels:read / groups:read scope falls back to the id for the TTL rather than
	// re-hitting Slack every turn.
	channelNameTTL = 30 * time.Minute
	// conversationsInfoResolveTimeout bounds a single conversations.info lookup so a slow
	// Slack response can't eat the turn budget. The channel-name path proceeds with the
	// id and negative-caches definitive failures; the confirm-surface path proceeds
	// with clicker-scoped ephemeral delivery and does not cache. This 3s ctx is the
	// BINDING deadline — the seam's HTTP client carries a looser 4s timeout, so the
	// ctx fires first.
	conversationsInfoResolveTimeout = 3 * time.Second
	// maxChannelNameLen bounds the resolved name before it lands in the system
	// prompt. Slack channel names are workspace-trusted and charset-constrained
	// (lowercase letters/digits/hyphens/underscores, no spaces, currently <=80
	// chars), so meaningful prompt injection isn't expressible; this is a defensive
	// length cap in case that ever changes.
	maxChannelNameLen = 80
)

// channelNameCache memoizes channel-name resolutions for the process lifetime with
// a TTL. Hits and answered-failure misses are cached (an answered failure caches an
// empty name; a ctx timeout/cancel is not — see resolveChannelName) so the per-turn
// system-prompt lookup costs at most one conversations.info per
// channel per channelNameTTL once the entry exists. Concurrent turns for the same
// UNcached channel aren't deduped (no singleflight), so a burst can issue a few
// lookups before the first populates the entry — bounded, and fine for a nicety.
// Expired entries are overwritten on the next resolve
// but never actively reaped, so the map's size is bounded by the distinct channels
// the process ever resolves (the channels the bot is mentioned in) — small in
// practice; no sweeper needed for a readability nicety. All methods are safe on a
// nil receiver (a Handler built without NewHandler just doesn't cache).
type channelNameCache struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]channelNameEntry
}

type channelNameEntry struct {
	name      string
	expiresAt time.Time
}

func newChannelNameCache(ttl time.Duration) *channelNameCache {
	return &channelNameCache{ttl: ttl, now: time.Now, m: map[string]channelNameEntry{}}
}

// get returns the cached name and true if a fresh (non-expired) entry exists.
// A cached empty name (a negative-cached resolve failure) still returns ok=true,
// so the caller skips re-resolving until the TTL lapses.
func (c *channelNameCache) get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || c.now().After(e.expiresAt) {
		return "", false
	}
	return e.name, true
}

func (c *channelNameCache) put(key, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = channelNameEntry{name: name, expiresAt: c.now().Add(c.ttl)}
}

// truncateChannelName bounds a resolved name to maxChannelNameLen before it reaches
// the system prompt. Slack names are ASCII (so byte-truncation can't split a rune);
// the cap is defensive belt-and-suspenders for the prompt input.
func truncateChannelName(name string) string {
	if len(name) > maxChannelNameLen {
		return name[:maxChannelNameLen]
	}
	return name
}

// resolveChannelName returns the channel's human name for the system prompt, or ""
// when it can't be resolved (no seam wired, missing scope, DM, or transport error)
// — in which case describeChannel falls back to the channel id. The result is
// memoized per channel for channelNameTTL; failures are negative-cached too. The
// lookup is bounded by conversationsInfoResolveTimeout so it can't stall the turn, and a
// resolve error is logged at debug and swallowed (best-effort, never fails the turn).
func (h *Handler) resolveChannelName(ctx context.Context, log *slog.Logger, teamID, enterpriseID, channelID string) string {
	if h.cfg.ResolveChannelName == nil || channelID == "" {
		return ""
	}
	key := teamID + ":" + channelID
	if name, ok := h.channelNames.get(key); ok {
		return name
	}
	rctx, cancel := context.WithTimeout(ctx, conversationsInfoResolveTimeout)
	defer cancel()
	name, err := h.cfg.ResolveChannelName(rctx, teamID, enterpriseID, channelID)
	if err != nil {
		log.Debug("agent: channel-name resolve failed; using channel id", "channel_id", channelID, "error", err)
		// Only cache a DEFINITIVE answer. A ctx timeout/cancel — the 3s budget tripping
		// because Slack was slow, or the turn being canceled — is "don't know yet": don't
		// poison the entry for the whole channelNameTTL over one slow response; use the id
		// this turn and re-attempt next turn (self-limited by the per-turn rate cap, each
		// bounded at 3s). An ANSWERED failure — Slack's ok:false (missing_scope, not_found)
		// or the rare raw transport error — is stable, so it takes the long negative-cache
		// TTL (the load-bearing case: it stops a scope-less workspace re-hitting Slack
		// every turn).
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			h.channelNames.put(key, "")
		}
		return ""
	}
	name = truncateChannelName(name)
	h.channelNames.put(key, name)
	return name
}
