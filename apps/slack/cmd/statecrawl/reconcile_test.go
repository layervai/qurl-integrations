package main

import (
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestClassifyStoredResourceReference(t *testing.T) {
	cases := map[string]storedResourceReferenceKind{
		"r_abc123":               storedReferenceLegacyInternalID,
		"r_":                     storedReferenceLegacyInternalID,
		"https://legacy.example": storedReferenceLegacyURL,
		"http://legacy.example":  storedReferenceLegacyURL,
		"":                       storedReferenceInvalid,
		"   ":                    storedReferenceInvalid,
		"public-resource-id":     storedReferenceOpaqueID,
	}
	for in, want := range cases {
		if got := classifyStoredResourceReference(in); got != want {
			t.Errorf("classifyStoredResourceReference(%q) = %v, want %v", in, got, want)
		}
	}
}

// liveFixture builds a resolved liveness with one active tunnel (slug differs
// from the alias under test), one active URL resource, and one revoked tunnel.
func liveFixture() liveness {
	return liveness{
		resolved: true,
		byID: map[string]client.Resource{
			"public-tunnel": {ResourceID: "public-tunnel", Type: client.ResourceTypeTunnel, Slug: "stats-connector", Status: client.StatusActive},
			"public-url":    {ResourceID: "public-url", Type: client.ResourceTypeURL, Status: client.StatusActive},
			"public-dead":   {ResourceID: "public-dead", Type: client.ResourceTypeTunnel, Slug: "gone", Status: client.StatusRevoked},
		},
	}
}

func TestClassifyResource(t *testing.T) {
	live := liveFixture()
	for _, tc := range []struct {
		name     string
		rid      string
		wantStat resourceStatus
		wantSlug string
	}{
		{"live tunnel", "public-tunnel", statusLiveTunnel, "stats-connector"},
		{"live url", "public-url", statusLiveURL, ""},
		{"revoked is orphan", "public-dead", statusOrphan, ""},
		{"absent is orphan", "public-missing", statusOrphan, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotStat, gotSlug := classifyResource(live, tc.rid)
			if gotStat != tc.wantStat || gotSlug != tc.wantSlug {
				t.Errorf("classifyResource(%q) = (%v, %q), want (%v, %q)", tc.rid, gotStat, gotSlug, tc.wantStat, tc.wantSlug)
			}
		})
	}
}

// TestClassifyRow_Resolved checks every finding kind a resolved team can emit,
// and confirms the healthy alias==slug case produces nothing.
func TestClassifyRow_Resolved(t *testing.T) {
	live := liveFixture()
	row := policyRow{
		teamID:    "T1",
		channelID: "C1",
		aliasBindings: map[string]string{
			"dashboard":       "public-tunnel", // alias name != slug -> #669 mismatch
			"stats-connector": "public-tunnel", // alias name == slug -> healthy, no finding
			"ghost":           "public-dead",   // revoked target -> orphan
			"docs":            "public-url",    // live URL -> informational
			"legacy":          "http://x",      // raw URL -> legacy
			"precutover":      "r_internal",    // old internal id -> migration blocker
		},
		allowedResourceIDs: []string{"public-tunnel", "public-dead", "r_internal"},
	}
	rep := newReport(&flags{})
	classifyRow(row, live, rep)

	got := countByKind(rep.findings)
	want := map[findingKind]int{
		findingAliasNameMismatch: 1,
		findingOrphanAlias:       1,
		findingAliasURLTarget:    1,
		findingLegacyAlias:       1,
		findingLegacyResourceID:  2,
		findingOrphanAllowedID:   1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("kind %s: got %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if len(rep.findings) != 7 {
		t.Errorf("total findings = %d, want 7 (healthy alias==slug must be silent): %v", len(rep.findings), rep.findings)
	}
}

// TestClassifyRow_Unresolved confirms an unverifiable team emits only
// indeterminate findings (one per reference) and never a purge target.
func TestClassifyRow_Unresolved(t *testing.T) {
	row := policyRow{
		teamID:             "T1",
		channelID:          "C1",
		aliasBindings:      map[string]string{"dashboard": "r_tunnel"},
		allowedResourceIDs: []string{"r_other"},
	}
	rep := newReport(&flags{})
	classifyRow(row, liveness{reason: "no key"}, rep)

	if len(rep.findings) != 2 {
		t.Fatalf("indeterminate findings = %d, want 2: %v", len(rep.findings), rep.findings)
	}
	for _, f := range rep.findings {
		if f.kind != findingIndeterminate {
			t.Errorf("kind = %s, want indeterminate", f.kind)
		}
		if f.isPurgeTarget() {
			t.Errorf("indeterminate finding must never be a purge target: %+v", f)
		}
	}
}

// TestPurgeTargets_DedupsAndFilters confirms only orphan kinds are purged and a
// (team, channel, resource) triple appears once even if multiple aliases share
// the dead id.
func TestPurgeTargets_DedupsAndFilters(t *testing.T) {
	rep := newReport(&flags{})
	rep.add(&finding{teamID: "T1", channelID: "C1", alias: "a", resourceID: "public-dead", kind: findingOrphanAlias})
	rep.add(&finding{teamID: "T1", channelID: "C1", alias: "b", resourceID: "public-dead", kind: findingOrphanAlias})
	rep.add(&finding{teamID: "T1", channelID: "C1", resourceID: "public-dead", kind: findingOrphanAllowedID})
	rep.add(&finding{teamID: "T1", channelID: "C1", alias: "c", resourceID: "public-tunnel", kind: findingAliasNameMismatch})
	rep.add(&finding{teamID: "T1", channelID: "C1", alias: "d", resourceID: "public-url", kind: findingAliasURLTarget})
	rep.add(&finding{teamID: "T1", channelID: "C1", alias: "e", resourceID: "r_internal", kind: findingLegacyResourceID})

	targets := rep.purgeTargets()
	if len(targets) != 1 {
		t.Fatalf("purgeTargets = %d, want 1 (dedup by triple, exclude non-orphans): %+v", len(targets), targets)
	}
	if targets[0].resourceID != "public-dead" {
		t.Errorf("target resource = %q, want public-dead", targets[0].resourceID)
	}
}

func countByKind(findings []finding) map[findingKind]int {
	out := make(map[findingKind]int)
	for _, f := range findings {
		out[f.kind]++
	}
	return out
}
