package main

import "sync/atomic"

// Stats is the live counter set for one crawl, mirroring the atomic-counter +
// Snapshot idiom qurl-service's operational CLIs use (cmd/qurl-scanner,
// cmd/qurl-bucket-backfill). Counters are bumped as findings are recorded and
// as purges run; Snapshot takes an eventually-consistent read for the final
// structured summary line. PurgeErrors is ALERTABLE — a non-zero value means a
// purge UpdateItem failed and an orphan may remain.
type Stats struct {
	ChannelsScanned    atomic.Int64
	TeamsResolved      atomic.Int64
	TeamsIndeterminate atomic.Int64

	OrphanAliases     atomic.Int64 // $alias bound to a revoked/deleted resource (#654)
	OrphanAllowedIDs  atomic.Int64 // allowed_resource_ids member that is revoked/deleted (#654)
	AliasNameMismatch atomic.Int64 // live tunnel reachable by an alias whose name != slug (#669)
	AliasURLTargets   atomic.Int64 // $alias bound to a live URL resource
	LegacyAliases     atomic.Int64 // $alias whose value is a non-r_ legacy raw-URL binding
	Indeterminate     atomic.Int64 // references on a team whose liveness couldn't be verified

	DryRunWouldPurge atomic.Int64 // orphaned ids that WOULD be purged (dry-run)
	Purged           atomic.Int64 // orphaned ids purged (apply)
	AliasesUnbound   atomic.Int64 // alias keys removed across all purges (apply)
	PurgeErrors      atomic.Int64 // ALERTABLE: purge UpdateItem failures
}

// Snapshot is a plain-int read of the counters, suitable for the final summary
// log line (no atomics in the rendered fields).
type Snapshot struct {
	ChannelsScanned    int64
	TeamsResolved      int64
	TeamsIndeterminate int64

	OrphanAliases     int64
	OrphanAllowedIDs  int64
	AliasNameMismatch int64
	AliasURLTargets   int64
	LegacyAliases     int64
	Indeterminate     int64

	DryRunWouldPurge int64
	Purged           int64
	AliasesUnbound   int64
	PurgeErrors      int64
}

// Snapshot reads every counter once. Eventually-consistent by construction;
// this tool is single-threaded today, but the shape matches qurl-service so the
// summary call site is identical if a parallel segment model is added later.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		ChannelsScanned:    s.ChannelsScanned.Load(),
		TeamsResolved:      s.TeamsResolved.Load(),
		TeamsIndeterminate: s.TeamsIndeterminate.Load(),
		OrphanAliases:      s.OrphanAliases.Load(),
		OrphanAllowedIDs:   s.OrphanAllowedIDs.Load(),
		AliasNameMismatch:  s.AliasNameMismatch.Load(),
		AliasURLTargets:    s.AliasURLTargets.Load(),
		LegacyAliases:      s.LegacyAliases.Load(),
		Indeterminate:      s.Indeterminate.Load(),
		DryRunWouldPurge:   s.DryRunWouldPurge.Load(),
		Purged:             s.Purged.Load(),
		AliasesUnbound:     s.AliasesUnbound.Load(),
		PurgeErrors:        s.PurgeErrors.Load(),
	}
}

// logAttrs renders the snapshot as slog key/value pairs for the summary line.
func (s Snapshot) logAttrs() []any {
	return []any{
		"channels_scanned", s.ChannelsScanned,
		"teams_resolved", s.TeamsResolved,
		"teams_indeterminate", s.TeamsIndeterminate,
		"orphan_aliases", s.OrphanAliases,
		"orphan_allowed_ids", s.OrphanAllowedIDs,
		"alias_name_mismatch", s.AliasNameMismatch,
		"alias_url_targets", s.AliasURLTargets,
		"legacy_aliases", s.LegacyAliases,
		"indeterminate", s.Indeterminate,
		"dry_run_would_purge", s.DryRunWouldPurge,
		"purged", s.Purged,
		"aliases_unbound", s.AliasesUnbound,
		"purge_errors", s.PurgeErrors,
	}
}
