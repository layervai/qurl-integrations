package main

import "testing"

const (
	testAPIKey        = "test-key"
	testCommandConfig = "config"
	testCreatedAt     = "2026-03-01T00:00:00Z"
	testEndpointFlag  = "--endpoint"
	testExampleURL    = "https://example.com"
	testFieldData     = "data"
	testFieldCreated  = "created_at"
	testFieldMeta     = "meta"
	testFieldRequest  = "request_id"
	testFieldResource = "resource_id"
	testFieldStatus   = "status"
	testFieldTarget   = "target_url"
	testRequestID     = "req_test"
	testStatusActive  = "active"
)

func isolateCLIEnv(t *testing.T, apiKey ...string) string {
	t.Helper()
	home := t.TempDir()
	key := ""
	if len(apiKey) > 1 {
		t.Fatalf("isolateCLIEnv accepts at most one API key override, got %d", len(apiKey))
	}
	if len(apiKey) > 0 {
		key = apiKey[0]
	}

	t.Setenv("HOME", home)
	t.Setenv("QURL_API_KEY", key)
	t.Setenv("QURL_ENDPOINT", "")
	t.Setenv("QURL_PROFILE", "")

	return home
}
