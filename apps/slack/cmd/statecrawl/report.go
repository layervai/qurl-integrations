package main

import (
	"context"
	"log/slog"
	"sort"
)

// purger is the single mutation the apply path performs: the bot's
// PurgeResourceFromChannel cascade. *slackdata.Store satisfies it; tests inject
// a fake to assert the dry-run path never calls it and the apply path calls it
// with the right (team, channel, resource) triples.
type purger interface {
	PurgeResourceFromChannel(ctx context.Context, teamID, channelID, resourceID string) ([]string, error)
}

// findingKind discriminates the categories the crawl surfaces. The two purge
// targets (#654 backfill) are findingOrphanAlias and findingOrphanAllowedID;
// everything else is informational and never mutated.
type findingKind string

const (
	findingOrphanAlias       findingKind = "orphan-alias"
	findingOrphanAllowedID   findingKind = "orphan-allowed-id"
	findingAliasNameMismatch findingKind = "alias-name-mismatch"
	findingAliasURLTarget    findingKind = "alias-url-target"
	findingLegacyAlias       findingKind = "legacy-alias"
	findingIndeterminate     findingKind = "indeterminate"
)

// finding is a single observation about one reference on one channel row.
type finding struct {
	teamID     string
	channelID  string
	alias      string
	resourceID string
	kind       findingKind
	detail     string
}

// isPurgeTarget reports whether this finding is a confirmed orphan that a
// mutating run should clear via the bot's PurgeResourceFromChannel verb.
func (f *finding) isPurgeTarget() bool {
	return f.kind == findingOrphanAlias || f.kind == findingOrphanAllowedID
}

// logAttrs renders the finding as slog key/value pairs, omitting the empty ones
// (an allowed_resource_ids finding carries no alias).
func (f *finding) logAttrs() []any {
	attrs := []any{"kind", string(f.kind), "team_id", f.teamID, "channel_id", f.channelID}
	if f.alias != "" {
		attrs = append(attrs, "alias", "$"+f.alias)
	}
	if f.resourceID != "" {
		attrs = append(attrs, "resource_id", f.resourceID)
	}
	return append(attrs, "detail", f.detail)
}

// report accumulates findings and the running counter set.
type report struct {
	f        *flags
	findings []finding
	stats    *Stats
}

func newReport(f *flags) *report { return &report{f: f, stats: &Stats{}} }

// add records a finding and bumps the counter for its kind in one place, so the
// summary totals can't drift from what was emitted. Takes a pointer (the
// finding struct is heavy) but stores a copy.
func (r *report) add(f *finding) {
	r.findings = append(r.findings, *f)
	switch f.kind {
	case findingOrphanAlias:
		r.stats.OrphanAliases.Add(1)
	case findingOrphanAllowedID:
		r.stats.OrphanAllowedIDs.Add(1)
	case findingAliasNameMismatch:
		r.stats.AliasNameMismatch.Add(1)
	case findingAliasURLTarget:
		r.stats.AliasURLTargets.Add(1)
	case findingLegacyAlias:
		r.stats.LegacyAliases.Add(1)
	case findingIndeterminate:
		r.stats.Indeterminate.Add(1)
	}
}

// emitFindings logs every finding as a structured record, sorted (purge targets
// first) so two runs over unchanged data produce a diffable log.
func (r *report) emitFindings(logger *slog.Logger) {
	sort.Slice(r.findings, func(i, j int) bool { return lessFinding(&r.findings[i], &r.findings[j]) })
	for i := range r.findings {
		logger.Info("finding", r.findings[i].logAttrs()...)
	}
}

// settle is the terminal step: in a dry run it records and logs the purge PLAN
// (what would be cleared) without mutating; otherwise it applies the purge.
func (r *report) settle(ctx context.Context, store purger, logger *slog.Logger) error {
	targets := r.purgeTargets()
	if r.f.dryRun {
		r.stats.DryRunWouldPurge.Store(int64(len(targets)))
		for _, t := range targets {
			logger.Info("would purge orphan (dry-run)", "team_id", t.teamID, "channel_id", t.channelID, "resource_id", t.resourceID)
		}
		return nil
	}
	return r.applyPurge(ctx, store, logger)
}

// applyPurge clears every confirmed orphan via slackdata.Store.PurgeResourceFromChannel —
// the exact verb the bot's revoke cascade runs — so the manual backfill is
// behaviorally identical to the live #654 path. Each purge is an auditable log
// record; a failure is counted (ALERTABLE) and logged but does not abort the
// sweep, since the purge is idempotent and a re-run retries cleanly.
func (r *report) applyPurge(ctx context.Context, store purger, logger *slog.Logger) error {
	targets := r.purgeTargets()
	for _, t := range targets {
		unbound, err := store.PurgeResourceFromChannel(ctx, t.teamID, t.channelID, t.resourceID)
		if err != nil {
			r.stats.PurgeErrors.Add(1)
			logger.Error("purge failed; an orphan may remain", "team_id", t.teamID, "channel_id", t.channelID, "resource_id", t.resourceID, "error", err)
			continue
		}
		r.stats.Purged.Add(1)
		r.stats.AliasesUnbound.Add(int64(len(unbound)))
		logger.Info("purged orphan", "team_id", t.teamID, "channel_id", t.channelID, "resource_id", t.resourceID, "unbound_aliases", unbound)
	}
	return nil
}

// purgeTarget is a single (team, channel, resource id) the apply path purges.
type purgeTarget struct {
	teamID     string
	channelID  string
	resourceID string
}

// purgeTargets returns the de-duplicated, sorted set of orphaned ids to purge.
// One PurgeResourceFromChannel call clears the id from allowed_resource_ids AND
// every alias key pointing at it, so one target per (team, channel, id) suffices
// even when several aliases share that dead id.
func (r *report) purgeTargets() []purgeTarget {
	seen := make(map[purgeTarget]struct{})
	out := make([]purgeTarget, 0, len(r.findings))
	for _, f := range r.findings {
		if !f.isPurgeTarget() {
			continue
		}
		t := purgeTarget{teamID: f.teamID, channelID: f.channelID, resourceID: f.resourceID}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].teamID != out[j].teamID {
			return out[i].teamID < out[j].teamID
		}
		if out[i].channelID != out[j].channelID {
			return out[i].channelID < out[j].channelID
		}
		return out[i].resourceID < out[j].resourceID
	})
	return out
}

// lessFinding orders findings for stable output: purge targets first (kind
// order), then by team, channel, alias, resource.
func lessFinding(a, b *finding) bool {
	if a.kind != b.kind {
		return kindRank(a.kind) < kindRank(b.kind)
	}
	if a.teamID != b.teamID {
		return a.teamID < b.teamID
	}
	if a.channelID != b.channelID {
		return a.channelID < b.channelID
	}
	if a.alias != b.alias {
		return a.alias < b.alias
	}
	return a.resourceID < b.resourceID
}

// kindRank gives each finding kind a stable sort position, purge targets first.
func kindRank(k findingKind) int {
	switch k {
	case findingOrphanAlias:
		return 0
	case findingOrphanAllowedID:
		return 1
	case findingAliasNameMismatch:
		return 2
	case findingAliasURLTarget:
		return 3
	case findingLegacyAlias:
		return 4
	case findingIndeterminate:
		return 5
	default:
		return 6
	}
}
