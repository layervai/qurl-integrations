package internal

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// Common keys for the qurl-service response envelope. Lifted to
// constants in tests because goconst would otherwise flag the 4+
// duplications across fixture builders.
const (
	testKeyData       = "data"
	testKeyError      = "error"
	testKeyResourceID = "resource_id"
	testKeyTitle      = "title"
	testKeyType       = "type"
	testKeyTargetURL  = "target_url"
	testResourceIDFix = "r_prod_db" // canonical test resource_id
	// mintByTestResourcePath is the resource-scoped mint endpoint
	// that `client.Create` hits when given a ResourceID (alias-form
	// /qurl get). Lifted so the alias-form tests register their
	// httptest mock at the same path the bot actually calls.
	mintByTestResourcePath = "/v1/resources/" + testResourceIDFix + "/qurls"
	testCmdSlash           = "/qurl"
	testFieldCallbackID    = "callback_id"
)

// writeResourceFixtureWithTarget writes the resource envelope used
// by the aliases tests — same shape as writeResourceFixture but
// includes the target_url.
func writeResourceFixtureWithTarget(t *testing.T, w http.ResponseWriter, resourceID, alias, target string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		testKeyData: map[string]any{
			testKeyResourceID: resourceID,
			"alias":           alias,
			"target_url":      target,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// slogTestLogger returns a logger that discards output so test
// fixtures can pass a slog.Logger without polluting -v output.
func slogTestLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
