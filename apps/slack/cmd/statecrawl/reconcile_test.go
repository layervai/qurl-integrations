package main

import (
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestIsResourceID(t *testing.T) {
	cases := map[string]bool{
		"r_abc123":               true,
		"r_":                     false, // prefix only, no id body
		"https://legacy.example": false,
		"":                       false,
		"resource_id":            false,
	}
	for in, want := range cases {
		if got := isResourceID(in); got != want {
			t.Errorf("isResourceID(%q) = %v, want %v", in, got, want)
		}
	}
}

// liveFixture builds a resolved liveness with one active tunnel (slug differs
// from the alias under test), one active URL resource, and one revoked tunnel.
func liveFixture() liveness {
	return liveness{
		resolved: true,
		byID: map[string]client.Resource{
			"r_tunnel": {ResourceID: "r_tunnel", Type: client.ResourceTypeTunnel, Slug: "stats-connector", Status: client.StatusActive},
			"r_url":    {ResourceID: "r_url", Type: client.ResourceTypeURL, Status: client.StatusActive},
			"r_dead":   {ResourceID: "r_dead", Type: client.ResourceTypeTunnel, Slug: "gone", Status: client.StatusRevoked},
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
		{"live tunnel", "r_tunnel", statusLiveTunnel, "stats-connector"},
		{"live url", "r_url", statusLiveURL, ""},
		{"revoked is orphan", "r_dead", statusOrphan, ""},
		{"absent is orphan", "r_missing", statusOrphan, ""},
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
			"dashboard":       "r_tunnel", // alias name != slug -> #669 mismatch
			"stats-connector": "r_tunnel", // alias name == slug -> healthy, no finding
			"ghost":           "r_dead",   // revoked target -> orphan
			"docs":            "r_url",    // live URL -> informational
			"legacy":          "http://x", // non-r_ -> legacy
		},
		allowedResourceIDs: []string{"r_tunnel", "r_dead"}, // r_dead -> orphan SS member
	}
	rep := newReport(&flags{})
	classifyRow(row, live, rep)

	got := countByKind(rep.findings)
	want := map[findingKind]int{
		findingAliasNameMismatch: 1,
		findingOrphanAlias:       1,
		findingAliasURLTarget:    1,
		findingLegacyAlias:       1,
		findingOrphanAllowedID:   1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("kind %s: got %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if len(rep.findings) != 5 {
		t.Errorf("total findings = %d, want 5 (healthy alias==slug must be silent): %v", len(rep.findings), rep.findings)
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
	rep.add(finding{teamID: "T1", channelID: "C1", alias: "a", resourceID: "r_dead", kind: findingOrphanAlias})
	rep.add(finding{teamID: "T1", channelID: "C1", alias: "b", resourceID: "r_dead", kind: findingOrphanAlias})
	rep.add(finding{teamID: "T1", channelID: "C1", resourceID: "r_dead", kind: findingOrphanAllowedID})
	rep.add(finding{teamID: "T1", channelID: "C1", alias: "c", resourceID: "r_tunnel", kind: findingAliasNameMismatch})
	rep.add(finding{teamID: "T1", channelID: "C1", alias: "d", resourceID: "r_url", kind: findingAliasURLTarget})

	targets := rep.purgeTargets()
	if len(targets) != 1 {
		t.Fatalf("purgeTargets = %d, want 1 (dedup by triple, exclude non-orphans): %+v", len(targets), targets)
	}
	if targets[0].resourceID != "r_dead" {
		t.Errorf("target resource = %q, want r_dead", targets[0].resourceID)
	}
}

func countByKind(findings []finding) map[findingKind]int {
	out := make(map[findingKind]int)
	for _, f := range findings {
		out[f.kind]++
	}
	return out
}
