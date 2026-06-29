package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// discardLogger is a slog logger that drops output — crawl logs heavily and the
// tests assert on counters/mutations, not log text.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeDDB is a single DynamoDB double that satisfies BOTH the SDK's
// ScanAPIClient (for scanPolicyRows) and slackdata.DynamoDBClient (so a real
// slackdata.Store can run PurgeResourceFromChannel against it). It serves Scan
// from preset pages, GetItem from a per-channel item map, and records every
// mutation so a dry run can be proven side-effect-free.
type fakeDDB struct {
	scanPages  []*dynamodb.ScanOutput
	scanErr    error
	queryPages []*dynamodb.QueryOutput
	queryErr   error

	items  map[string]map[string]ddbtypes.AttributeValue // channelID -> row item
	getErr error

	mu          sync.Mutex
	scanIdx     int
	queryIdx    int
	scanCalls   int
	updateItems []*dynamodb.UpdateItemInput
	putCalls    int
	deleteCalls int
}

func (f *fakeDDB) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanCalls++
	if f.scanIdx >= len(f.scanPages) {
		return &dynamodb.ScanOutput{}, nil
	}
	page := f.scanPages[f.scanIdx]
	f.scanIdx++
	return page, nil
}

func (f *fakeDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queryIdx >= len(f.queryPages) {
		return &dynamodb.QueryOutput{}, nil
	}
	page := f.queryPages[f.queryIdx]
	f.queryIdx++
	return page, nil
}

func (f *fakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	channelID := keyString(in.Key, attrSlackChannelID)
	if item, ok := f.items[channelID]; ok {
		return &dynamodb.GetItemOutput{Item: item}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateItems = append(f.updateItems, in)
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *fakeDDB) PutItem(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) DeleteItem(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	return &dynamodb.DeleteItemOutput{}, nil
}

// mutationCount totals the write calls the fake observed — the dry-run guard
// asserts this is zero.
func (f *fakeDDB) mutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updateItems) + f.putCalls + f.deleteCalls
}

// keyString reads a string key attribute from a DDB Key map.
func keyString(key map[string]ddbtypes.AttributeValue, name string) string {
	v, ok := key[name].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}

// policyItem builds a channel_policies row item with the given alias bindings
// and allowed-id set, for both the Scan pages and GetItem lookups.
func policyItem(team, channel string, aliases map[string]string, allowed []string) map[string]ddbtypes.AttributeValue {
	item := map[string]ddbtypes.AttributeValue{
		attrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: team},
		attrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: channel},
	}
	if len(aliases) > 0 {
		m := make(map[string]ddbtypes.AttributeValue, len(aliases))
		for k, v := range aliases {
			m[k] = &ddbtypes.AttributeValueMemberS{Value: v}
		}
		item[attrAliasBindings] = &ddbtypes.AttributeValueMemberM{Value: m}
	}
	if len(allowed) > 0 {
		item[attrAllowedResourceIDs] = &ddbtypes.AttributeValueMemberSS{Value: allowed}
	}
	return item
}

// fakeProvider is an auth.Provider double: a per-team key map, with a missing
// team surfacing ErrWorkspaceNotConfigured (the real provider's contract), and
// an optional per-team error to model an outage.
type fakeProvider struct {
	keys map[string]string
	errs map[string]error
}

func (p fakeProvider) APIKey(_ context.Context, teamID string) (string, error) {
	if err := p.errs[teamID]; err != nil {
		return "", err
	}
	if k, ok := p.keys[teamID]; ok {
		return k, nil
	}
	return "", auth.ErrWorkspaceNotConfigured
}

// qurlResource is a resource entry in the GET /v1/resources test envelope.
func qurlResource(id, typ, slug, status string) map[string]any {
	r := map[string]any{"resource_id": id, "type": typ, "status": status}
	if slug != "" {
		r["slug"] = slug
	}
	return r
}

// qurlServer returns a test server that serves GET /v1/resources as a single
// page of the given resources (has_more=false). Used for the liveness check.
func qurlServer(t *testing.T, resources []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/resources" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		writeEnvelope(t, w, resources, "", false)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeEnvelope writes a qurl-service success envelope (data + meta).
func writeEnvelope(t *testing.T, w http.ResponseWriter, data any, nextCursor string, hasMore bool) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]any{"request_id": "req_test", "has_more": hasMore, "next_cursor": nextCursor},
	}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}
