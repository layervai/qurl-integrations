package main

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// liveness is the authoritative view of one workspace's resources, used to
// decide whether a referenced resource id is live. resolved is false when the
// workspace has no API key or its resource list couldn't be fully read — in
// that case every reference is reported as indeterminate and NEVER purged
// (we must not delete a binding we couldn't verify as dead).
type liveness struct {
	resolved bool
	reason   string
	byID     map[string]client.Resource
}

// resolveLiveness fetches the per-workspace API key, lists every resource the
// owner has (paginating to exhaustion so the liveness check is authoritative,
// not bounded by the bot's first-page scan), and indexes them by resource id.
// A missing key (ErrWorkspaceNotConfigured) or any read error degrades the team
// to unresolved with a human reason rather than aborting the whole crawl.
//
// OWNERSHIP INVARIANT: liveness is built from THIS workspace's own API key, so
// the purge path is safe only while every channel_policies reference is owned
// by the same workspace whose key drives this lookup. That holds today — the
// bot creates every resource with the workspace key. If qURL ever lets a
// resource owned by a DIFFERENT workspace be granted into a channel, that live
// resource would be absent from this list and the apply path would wrongly
// classify it as an orphan and purge it. Any cross-workspace/org resource
// sharing MUST revisit this function (e.g. resolve liveness per owning
// workspace) before it can turn into a data-loss bug.
func resolveLiveness(ctx context.Context, keys auth.Provider, f *flags, teamID string) liveness {
	apiKey, err := keys.APIKey(ctx, teamID)
	if err != nil {
		if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
			return liveness{reason: "workspace has no qURL API key (setup never completed or was revoked)"}
		}
		return liveness{reason: "API key lookup failed: " + err.Error()}
	}

	c := newClient(f.qurlEndpoint, apiKey)
	resources, err := listAllResources(ctx, c, pageLimitOrDefault(f.pageLimit))
	if err != nil {
		return liveness{reason: "resource list failed: " + err.Error()}
	}

	byID := make(map[string]client.Resource, len(resources))
	for i := range resources {
		byID[resources[i].ResourceID] = resources[i]
	}
	return liveness{resolved: true, byID: byID}
}

// maxResourceList caps the TOTAL resources accumulated while paginating, so the
// fail-safe is independent of the -page-limit tuning knob (a page-count cap
// would shrink the effective ceiling 10× under -page-limit=10 and abort a
// legitimately large workspace). 100k is far beyond any real workspace.
const maxResourceList = 100_000

// listAllResources pages GET /v1/resources to completion. The bot deliberately
// scans only the first page (latency budget); a backfill reconciler must be
// exhaustive so it never misclassifies a live resource on a later page as an
// orphan. The empty-cursor / HasMore checks terminate normally; the two
// progress guards below are fail-safes so a server bug that keeps returning
// HasMore=true errors out (→ team reported indeterminate, never purged)
// instead of looping forever.
func listAllResources(ctx context.Context, c *client.Client, pageLimit int) ([]client.Resource, error) {
	var all []client.Resource
	cursor := ""
	for {
		out, err := c.ListResources(ctx, client.ListResourcesInput{Limit: pageLimit, Cursor: cursor})
		if err != nil {
			return nil, err //nolint:wrapcheck // caller annotates with the team context.
		}
		all = append(all, out.Resources...)
		if !out.HasMore || out.NextCursor == "" {
			return all, nil
		}
		// Every continued iteration must have made progress and stay under the
		// ceiling — together these guarantee termination on a stuck cursor.
		if len(out.Resources) == 0 {
			return nil, errors.New("resource list returned an empty page with has_more=true; aborting (treated as unverifiable, never purged)")
		}
		if len(all) >= maxResourceList {
			return nil, errors.New("resource list exceeded " + strconv.Itoa(maxResourceList) + " resources; aborting (treated as unverifiable, never purged)")
		}
		cursor = out.NextCursor
	}
}

// resourceStatus classifies a referenced resource id against the workspace's
// live resources. It returns a kind discriminator the report and apply paths
// branch on.
type resourceStatus int

const (
	// statusLiveTunnel: an active type=tunnel resource (the healthy case).
	statusLiveTunnel resourceStatus = iota
	// statusLiveURL: an active type=url resource — live, but display-name verbs
	// can't target it (informational, never purged).
	statusLiveURL
	// statusOrphan: absent from the owner's resources, or present but revoked.
	// This is the #654 backfill target.
	statusOrphan
)

// classifyResource reports the resourceStatus of rid plus the live resource's
// slug (empty unless it's a live tunnel). Caller must only invoke this for a
// resolved team.
//
// STATUS-SET ASSUMPTION (sibling of resolveLiveness's OWNERSHIP INVARIANT):
// "not StatusActive ⇒ orphan" is exact only while the resource tier is
// two-state (active|revoked — see client.Resource.Status). If qurl-service ever
// adds an intermediate status (e.g. pending/suspended), this must enumerate the
// dead statuses explicitly or those live-ish bindings would be purged.
func classifyResource(live liveness, rid string) (status resourceStatus, slug string) {
	r, ok := live.byID[rid]
	if !ok || r.Status != client.StatusActive {
		return statusOrphan, ""
	}
	if r.Type == client.ResourceTypeTunnel {
		return statusLiveTunnel, r.Slug
	}
	return statusLiveURL, ""
}

// storedResourceReferenceKind describes the resource-reference formats that
// can exist in channel policy rows across qURL API generations. Public
// resource IDs are intentionally opaque; only the two retired formats are
// recognized explicitly so the crawler never mistakes them for purgeable
// resource IDs.
type storedResourceReferenceKind uint8

const (
	storedReferenceInvalid storedResourceReferenceKind = iota
	storedReferenceLegacyURL
	storedReferenceLegacyInternalID
	storedReferenceOpaqueID
)

func classifyStoredResourceReference(s string) storedResourceReferenceKind {
	s = strings.TrimSpace(s)
	if s == "" {
		return storedReferenceInvalid
	}
	if strings.HasPrefix(s, "r_") {
		return storedReferenceLegacyInternalID
	}
	u, err := url.Parse(s)
	if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		return storedReferenceLegacyURL
	}
	return storedReferenceOpaqueID
}

// classifyRow walks one policy row's alias bindings and allowed-id set,
// recording every finding into the report. For an unresolved team it records
// each reference as indeterminate so the operator sees the row but no purge is
// ever proposed.
func classifyRow(row policyRow, live liveness, rep *report) {
	if !live.resolved {
		recordIndeterminate(row, live.reason, rep)
		return
	}
	classifyAliasBindings(row, live, rep)
	classifyAllowedIDs(row, live, rep)
}

// classifyAliasBindings classifies each alias->resource binding on the row.
func classifyAliasBindings(row policyRow, live liveness, rep *report) {
	for alias, rid := range row.aliasBindings {
		f := finding{teamID: row.teamID, channelID: row.channelID, alias: alias, resourceID: rid}
		switch classifyStoredResourceReference(rid) {
		case storedReferenceLegacyInternalID:
			f.kind = findingLegacyResourceID
			f.detail = "alias uses a pre-public-ID resource reference; migrate it before normal reconciliation"
			rep.add(&f)
			continue
		case storedReferenceLegacyURL, storedReferenceInvalid:
			f.kind = findingLegacyAlias
			f.detail = "alias points at a non-resource target (legacy raw-URL binding); set-alias to re-bind"
			rep.add(&f)
			continue
		case storedReferenceOpaqueID:
			// Continue below and classify the opaque id against the live API.
		}
		switch status, slug := classifyResource(live, rid); status {
		case statusOrphan:
			f.kind = findingOrphanAlias
			f.detail = "alias bound to a revoked/deleted resource — #654 purge target"
			rep.add(&f)
		case statusLiveURL:
			f.kind = findingAliasURLTarget
			f.detail = "alias bound to a live URL resource (not display-name targetable)"
			rep.add(&f)
		case statusLiveTunnel:
			if slug != alias {
				f.detail = "live tunnel reachable by alias whose name differs from slug " + quote(slug) + " — #669 makes this display-name targetable"
				f.kind = findingAliasNameMismatch
				rep.add(&f)
			}
			// alias == slug: the healthy install-default case; nothing to report.
		}
	}
}

// classifyAllowedIDs classifies each allowed_resource_ids member on the row.
// Members carried only in the SS (no alias) can still orphan when the resource
// is deleted — #654 clears them too.
func classifyAllowedIDs(row policyRow, live liveness, rep *report) {
	for _, rid := range row.allowedResourceIDs {
		switch classifyStoredResourceReference(rid) {
		case storedReferenceLegacyInternalID:
			rep.add(&finding{
				teamID: row.teamID, channelID: row.channelID, resourceID: rid,
				kind:   findingLegacyResourceID,
				detail: "allowed_resource_ids contains a pre-public-ID reference; migrate it before normal reconciliation",
			})
			continue
		case storedReferenceLegacyURL, storedReferenceInvalid:
			// Malformed historical members require manual repair. They are never
			// classified as orphans because the live API cannot verify them.
			continue
		case storedReferenceOpaqueID:
			// Continue below and classify the opaque id against the live API.
		}
		if status, _ := classifyResource(live, rid); status == statusOrphan {
			rep.add(&finding{
				teamID:     row.teamID,
				channelID:  row.channelID,
				resourceID: rid,
				kind:       findingOrphanAllowedID,
				detail:     "allowed_resource_ids member is a revoked/deleted resource — #654 purge target",
			})
			continue
		}
	}
}

// recordIndeterminate logs every reference on a row whose team couldn't be
// verified, so the operator sees coverage gaps without any purge being proposed.
func recordIndeterminate(row policyRow, reason string, rep *report) {
	for alias, rid := range row.aliasBindings {
		rep.add(&finding{
			teamID: row.teamID, channelID: row.channelID, alias: alias, resourceID: rid,
			kind: findingIndeterminate, detail: reason,
		})
	}
	for _, rid := range row.allowedResourceIDs {
		rep.add(&finding{
			teamID: row.teamID, channelID: row.channelID, resourceID: rid,
			kind: findingIndeterminate, detail: reason,
		})
	}
}

// quote wraps a value in double quotes for a finding detail string.
func quote(s string) string { return "\"" + s + "\"" }
