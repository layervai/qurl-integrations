package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/shared/client"
)

// TestScanPolicyRows_PaginatesAndSkipsKeylessRows confirms the Scan paginator
// is followed across pages (via LastEvaluatedKey) and that a row missing the
// team/channel keys is dropped rather than aborting or half-parsing the crawl.
func TestScanPolicyRows_PaginatesAndSkipsKeylessRows(t *testing.T) {
	keyless := map[string]ddbtypes.AttributeValue{
		attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{}},
	}
	page1 := &dynamodb.ScanOutput{
		Items: []map[string]ddbtypes.AttributeValue{
			policyItem("T1", "C1", map[string]string{"a": "r_1"}, nil),
			keyless,
		},
		LastEvaluatedKey: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T1"},
		},
	}
	page2 := &dynamodb.ScanOutput{
		Items: []map[string]ddbtypes.AttributeValue{
			policyItem("T2", "C2", nil, []string{"r_2"}),
		},
	}
	fake := &fakeDDB{scanPages: []*dynamodb.ScanOutput{page1, page2}}

	rows, err := scanPolicyRows(context.Background(), fake, "cp", "")
	if err != nil {
		t.Fatalf("scanPolicyRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (keyless row skipped, both pages read): %+v", len(rows), rows)
	}
	if rows[0].teamID != "T1" || rows[1].teamID != "T2" {
		t.Errorf("rows out of order or wrong: %+v", rows)
	}
	if rows[0].aliasBindings["a"] != "r_1" {
		t.Errorf("alias binding not parsed: %+v", rows[0])
	}
}

// TestScanPolicyRows_SingleTeamUsesQueryNotScan confirms the -team fast path
// reads via Query on the partition key and never falls back to a full-table
// Scan (the perf guarantee for the "unblock a customer fast" workflow).
func TestScanPolicyRows_SingleTeamUsesQueryNotScan(t *testing.T) {
	fake := &fakeDDB{
		queryPages: []*dynamodb.QueryOutput{{
			Items: []map[string]ddbtypes.AttributeValue{
				policyItem("T1", "C1", map[string]string{"a": "r_1"}, nil),
			},
		}},
		// A non-empty Scan page proves we did NOT read it: if the code Scanned,
		// we'd see this row instead of (or alongside) the queried one.
		scanPages: []*dynamodb.ScanOutput{{Items: []map[string]ddbtypes.AttributeValue{
			policyItem("T9", "C9", nil, []string{"r_9"}),
		}}},
	}

	rows, err := scanPolicyRows(context.Background(), fake, "cp", "T1")
	if err != nil {
		t.Fatalf("scanPolicyRows: %v", err)
	}
	if fake.scanCalls != 0 {
		t.Errorf("single-team path issued %d Scan calls, want 0 (must Query the partition key)", fake.scanCalls)
	}
	if len(rows) != 1 || rows[0].teamID != "T1" {
		t.Fatalf("rows = %+v, want exactly the queried T1 row", rows)
	}
}

// TestResolveLiveness_PaginatesAllResources proves the liveness check pages the
// owner's resources to exhaustion (not just the first page the bot scans), so a
// resource on a later page is never misclassified as an orphan.
func TestResolveLiveness_PaginatesAllResources(t *testing.T) {
	srv := paginatedQURLServer(t, map[string][]map[string]any{
		"":        {qurlResource("r_p1", client.ResourceTypeTunnel, "p1", client.StatusActive)},
		"cursor2": {qurlResource("r_p2", client.ResourceTypeTunnel, "p2", client.StatusActive)},
	}, map[string]string{"": "cursor2"})
	provider := fakeProvider{keys: map[string]string{"T1": "key"}}

	live := resolveLiveness(context.Background(), provider, crawlFlags(srv.URL, true), "T1")
	if !live.resolved {
		t.Fatalf("liveness unresolved: %q", live.reason)
	}
	if _, ok := live.byID["r_p1"]; !ok {
		t.Error("first-page resource missing")
	}
	if _, ok := live.byID["r_p2"]; !ok {
		t.Error("second-page resource missing — pagination stopped early")
	}
}

// TestResolveLiveness_NotConfigured maps a missing key to an unresolved,
// never-purged team with a human reason.
func TestResolveLiveness_NotConfigured(t *testing.T) {
	live := resolveLiveness(context.Background(), fakeProvider{}, crawlFlags("http://unused", true), "T1")
	if live.resolved {
		t.Fatal("expected unresolved for a team with no API key")
	}
	if live.reason == "" {
		t.Error("unresolved liveness must carry a reason")
	}
}

// TestResolveLiveness_ListErrorUnresolved confirms a failing resource list
// degrades to unresolved (never purged) rather than treating an outage as
// "no resources" (which would mark every binding an orphan).
func TestResolveLiveness_ListErrorUnresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"title":"boom","status":500}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	provider := fakeProvider{keys: map[string]string{"T1": "key"}}

	live := resolveLiveness(context.Background(), provider, crawlFlags(srv.URL, true), "T1")
	if live.resolved {
		t.Fatal("a resource-list failure must NOT resolve (else live bindings look orphaned)")
	}
}

// paginatedQURLServer serves GET /v1/resources by the request's cursor query
// param: pages maps cursor -> resources, next maps cursor -> the next cursor
// (absent => has_more=false).
func paginatedQURLServer(t *testing.T, pages map[string][]map[string]any, next map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		data, ok := pages[cursor]
		if !ok {
			t.Errorf("unexpected cursor %q", cursor)
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
			return
		}
		nextCursor, hasMore := next[cursor]
		writeEnvelope(t, w, data, nextCursor, hasMore)
	}))
	t.Cleanup(srv.Close)
	return srv
}
