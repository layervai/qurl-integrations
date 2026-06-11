package internal

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// channelMembershipTTL bounds how long a resolved (channel, user) membership decision is
	// reused (and caps conversations.members to one bounded lookup per (channel, user) per
	// TTL). It mirrors channelNameTTL's value, but note the difference in what's at stake:
	// this gates access, so a stale decision lags by up to one TTL. A removed member keeps a
	// scoped pane (the security-relevant case: an accepted revocation lag, standard for cached
	// auth, and strictly weaker than a never-member, who is denied immediately). The reverse —
	// a cached "false" for a REAL member, whether they joined after it was cached or they sit
	// beyond the bounded scan (membershipPageLimit × maxMembershipPages) — keeps them DM-scoped
	// for up to one TTL, re-checked only when the entry expires. That's benign (fails safe,
	// usability-only); the beyond-bound case is the most likely degradation in a very large
	// channel. (Errors, by contrast, are never cached — see resolveChannelMembership.)
	channelMembershipTTL = 30 * time.Minute
	// channelMembershipTimeout bounds a single membership lookup so a slow Slack response
	// can't eat the turn budget. On timeout the pane stays UN-scoped (fail-closed). This
	// 3s ctx is the binding deadline; the seam's HTTP client carries a looser timeout.
	channelMembershipTimeout = 3 * time.Second
)

// channelMembershipCache memoizes (channel, user) membership decisions with a TTL,
// mirroring channelNameCache. Only a SEAM-RETURNED answer (member true/false, no error) is
// cached; an error of any kind is not (see resolveChannelMembership), so a transient blip
// can't lock the scope out for the whole TTL. Nil-receiver-safe (a Handler built without
// NewHandler just doesn't cache). Its key space — (channel, user) — is larger than
// channelNameCache's (channel only), and like that cache it has no active reaper (expired
// entries are overwritten on the next miss, never swept); it stays bounded because it's keyed
// by ACTUAL pane usage (members who open a pane from a channel), not the team×channel×user
// cartesian product. A reaper (or the cache unification in #697) can revisit if very large
// workspaces prove it necessary.
type channelMembershipCache struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]channelMembershipEntry
}

type channelMembershipEntry struct {
	member    bool
	expiresAt time.Time
}

func newChannelMembershipCache(ttl time.Duration) *channelMembershipCache {
	return &channelMembershipCache{ttl: ttl, now: time.Now, m: map[string]channelMembershipEntry{}}
}

func (c *channelMembershipCache) get(key string) (member, ok bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || c.now().After(e.expiresAt) {
		return false, false
	}
	return e.member, true
}

func (c *channelMembershipCache) put(key string, member bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = channelMembershipEntry{member: member, expiresAt: c.now().Add(c.ttl)}
}

// resolveChannelMembership reports whether userID is a member of channelID — the
// authorization gate for whether an assistant-pane turn may scope its reads to the channel
// the user opened the pane from. It is the PRIMARY check for that surface: the read backends
// key only on the channel id with no per-user check, so whoever's channel lands in the
// TurnContext sees its full qURL topology. It therefore FAILS CLOSED — a nil seam, an empty
// id, an error, or a timeout all return false (don't scope → the un-scoped DM), so a
// non-member (e.g. someone previewing a public channel) can never enumerate that channel's
// topology through the pane. The same fail-closed default also denies a LEGITIMATE member of
// a private channel the bot isn't in (conversations.members errors there) — acceptable, since
// that channel's qURL data is likely empty without the bot present anyway. The membership
// DETERMINATION is bounded/best-effort: the seam
// scans only a bounded slice of the membership, so a member of a very large channel beyond
// that bound also reads as not-confirmed and gets the un-scoped pane — the degradation is in
// the safe direction (deny, never grant), so it's acceptable, not a leak.
//
// The decision is memoized per (channel, user) for channelMembershipTTL. Only a definitive
// seam answer (member true/false, no error) is cached — an error of any kind is NOT, so it
// re-checks next turn rather than locking a member out of scope for the whole TTL after a
// single blip (the seam returns an error, not (false, nil), for a Slack ok:false, so even a
// stable missing_scope re-checks — see the error branch for that tradeoff). A lookup error
// is logged at debug and swallowed.
func (h *Handler) resolveChannelMembership(ctx context.Context, log *slog.Logger, teamID, enterpriseID, channelID, userID string) bool {
	if h.cfg.ChannelMembership == nil || channelID == "" || userID == "" {
		return false
	}
	// enterpriseID is omitted from the key: Slack team/channel/user ids are globally unique,
	// so (team, channel, user) already identifies the decision (matches channelNames).
	key := teamID + ":" + channelID + ":" + userID
	if member, ok := h.channelMembers.get(key); ok {
		return member
	}
	rctx, cancel := context.WithTimeout(ctx, channelMembershipTimeout)
	defer cancel()
	member, err := h.cfg.ChannelMembership(rctx, teamID, enterpriseID, channelID, userID)
	if err != nil {
		// Fail closed, and do NOT cache the error. Unlike resolveChannelName (where a stale
		// negative-cache is cosmetic — a bare id in the prompt), a wrongly-cached "not a
		// member" locks a genuine member out of pane scope for the whole TTL. That's too
		// costly to risk on a transient 5xx / network blip, so any error simply re-checks next
		// turn. A stable missing_scope therefore re-hits Slack per turn — bounded by the
		// per-turn rate cap + the 3s budget, and rare (the read scopes are present once the
		// pane is enabled), which is the right tradeoff for an access gate.
		log.Debug("agent: channel-membership check failed; pane stays un-scoped", "channel_id", channelID, "error", err)
		return false
	}
	h.channelMembers.put(key, member)
	return member
}

// paneContextChannel returns the channel an assistant-pane (DM) turn should scope its reads
// to — the channel the user opened the pane from, persisted by the container events (Slice
// 3a) — or "" to fall back to the un-scoped DM. It returns "" when membership can't be
// checked (no ChannelMembership seam), when there's no stored context, when the store read
// fails, or when the user is not a confirmed member of the context channel
// (resolveChannelMembership is fail-closed). On a hit it refreshes the context's TTL so
// channel-awareness lives as long as the conversation stays active (SaveConversation
// similarly bumps the transcript each turn), then returns the channel. Only meaningful for
// im turns; the caller gates on that. The store key/partition match Slice 3a's write
// exactly (agentEventThreadKey delegates to agentThreadKey; partition is the team id).
func (h *Handler) paneContextChannel(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) string {
	if h.cfg.ChannelMembership == nil {
		return "" // can't confirm membership → never scope; skip the context read entirely
	}
	partition, key := env.TeamID, agentEventThreadKey(env)
	c, found, err := h.cfg.AgentStore.GetThreadContext(ctx, partition, key)
	if err != nil {
		log.Warn("agent: read pane context failed; using DM scope", "error", err)
		return ""
	}
	if !found || c == "" {
		return ""
	}
	if !h.resolveChannelMembership(ctx, log, env.TeamID, env.EnterpriseID, c, env.Event.User) {
		log.Info("agent: pane opener not a confirmed member of context channel; using DM scope", "context_channel", c)
		return ""
	}
	// Refresh the context TTL on this ATTEMPT (before the turn runs), so an active thread keeps
	// channel-awareness even if this turn then fails transiently — it tracks activity, not success.
	if err := h.cfg.AgentStore.PutThreadContext(ctx, partition, key, c); err != nil {
		log.Warn("agent: refresh pane context TTL failed", "error", err)
	}
	return c
}
