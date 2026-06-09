package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

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

// isPurgeTarget reports whether this finding is a confirmed orphan that -apply
// should clear via the bot's PurgeResourceFromChannel verb.
func (f finding) isPurgeTarget() bool {
	return f.kind == findingOrphanAlias || f.kind == findingOrphanAllowedID
}

// report accumulates findings and renders them.
type report struct {
	cfg      config
	findings []finding
}

func newReport(cfg config) *report { return &report{cfg: cfg} }

func (r *report) add(f finding) { r.findings = append(r.findings, f) }

// printSummary renders the per-finding detail (sorted for stable output) and a
// tally by kind, with a header naming the crawled deployment.
func (r *report) printSummary() {
	fmt.Println("=== qURL Slack bot channel_policies state crawl ===")
	fmt.Println("deployment: " + r.cfg.envLabel)
	fmt.Println("channel_policies table: " + r.cfg.channelPoliciesTable)
	fmt.Println("mode: " + r.mode())
	fmt.Println()

	if len(r.findings) == 0 {
		fmt.Println("No findings: every channel_policies reference resolves to a live resource.")
		return
	}

	sorted := slices.Clone(r.findings)
	sort.Slice(sorted, func(i, j int) bool { return lessFinding(sorted[i], sorted[j]) })
	for _, f := range sorted {
		fmt.Println(formatFinding(f))
	}

	fmt.Println()
	r.printTally()
}

func (r *report) mode() string {
	if r.cfg.apply {
		return "APPLY (will purge confirmed orphans)"
	}
	return "dry-run (read-only)"
}

// printTally prints a count per finding kind in a fixed order so the operator
// sees the purge-target totals first.
func (r *report) printTally() {
	counts := make(map[findingKind]int)
	for _, f := range r.findings {
		counts[f.kind]++
	}
	fmt.Println("summary by kind:")
	for _, k := range []findingKind{
		findingOrphanAlias, findingOrphanAllowedID,
		findingAliasNameMismatch, findingAliasURLTarget,
		findingLegacyAlias, findingIndeterminate,
	} {
		if counts[k] > 0 {
			fmt.Println("  " + string(k) + ": " + itoa(counts[k]))
		}
	}
}

// printDryRunFooter tells the operator how to act on what the dry run found.
func (r *report) printDryRunFooter() {
	purgeable := r.purgeTargetCount()
	fmt.Println()
	if purgeable == 0 {
		fmt.Println("Dry run complete. No orphans to purge.")
		return
	}
	fmt.Println("Dry run complete. " + itoa(purgeable) +
		" orphaned reference(s) would be purged. Re-run with -apply to clear them.")
}

func (r *report) purgeTargetCount() int {
	n := 0
	for _, f := range r.findings {
		if f.isPurgeTarget() {
			n++
		}
	}
	return n
}

// applyPurge clears every confirmed orphan via slackdata.Store.PurgeResourceFromChannel —
// the exact verb the bot's revoke cascade runs — so the manual backfill is
// behaviorally identical to the live #654 path. It dedups by (team, channel,
// resource id): one Purge call removes the id from allowed_resource_ids AND
// every alias key pointing at it, so calling it once per orphaned id is
// sufficient even when several aliases share that id.
func (r *report) applyPurge(ctx context.Context, store *slackdata.Store) error {
	targets := r.purgeTargets()
	if len(targets) == 0 {
		fmt.Println("\nAPPLY: nothing to purge.")
		return nil
	}

	fmt.Println("\n!!! APPLY MODE — mutating " + r.cfg.envLabel + " channel_policies (" +
		itoa(len(targets)) + " orphaned id(s)) !!!")
	var failures int
	for _, t := range targets {
		unbound, err := store.PurgeResourceFromChannel(ctx, t.teamID, t.channelID, t.resourceID)
		if err != nil {
			failures++
			fmt.Println("  FAILED " + t.label() + ": " + err.Error())
			continue
		}
		suffix := ""
		if len(unbound) > 0 {
			suffix = " (unbound aliases: " + strings.Join(unbound, ", ") + ")"
		}
		fmt.Println("  purged " + t.label() + suffix)
	}
	if failures > 0 {
		return fmt.Errorf("apply finished with %d purge failure(s); re-run to retry (purge is idempotent)", failures)
	}
	fmt.Println("APPLY complete: " + itoa(len(targets)) + " orphaned id(s) purged.")
	return nil
}

// purgeTarget is a single (team, channel, resource id) the apply path purges.
type purgeTarget struct {
	teamID     string
	channelID  string
	resourceID string
}

func (t purgeTarget) label() string {
	return "team=" + t.teamID + " channel=" + t.channelID + " resource=" + t.resourceID
}

// purgeTargets returns the de-duplicated, sorted set of orphaned ids to purge.
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

// formatFinding renders one finding as a single grep-friendly line.
func formatFinding(f finding) string {
	var b strings.Builder
	b.WriteString("[" + string(f.kind) + "] team=" + f.teamID + " channel=" + f.channelID)
	if f.alias != "" {
		b.WriteString(" alias=$" + f.alias)
	}
	if f.resourceID != "" {
		b.WriteString(" resource=" + f.resourceID)
	}
	b.WriteString(" — " + f.detail)
	return b.String()
}

// lessFinding orders findings for stable output: purge targets first (kind
// order), then by team, channel, alias, resource.
func lessFinding(a, b finding) bool {
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

// quote wraps a value in double quotes for report copy.
func quote(s string) string { return "\"" + s + "\"" }
