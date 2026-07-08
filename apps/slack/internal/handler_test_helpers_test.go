package internal

import (
	"io"
	"log/slog"
	"testing"
)

// Common keys for the qurl-service response envelope. Lifted to
// constants in tests because goconst would otherwise flag the 4+
// duplications across fixture builders.
const (
	testKeyData        = "data"
	testKeyError       = "error"
	testKeyAPIKey      = "api_key"
	testKeyExpiresAt   = "expires_at"
	testKeyExpiresIn   = "expires_in"
	testKeyKeyID       = "key_id"
	testKeyKeyType     = "key_type"
	testKeyResourceID  = "resource_id"
	testKeySlug        = "slug"
	testKeyStatus      = "status"
	testKeyTitle       = "title"
	testKeyTunnelSlug  = "tunnel_slug"
	testKeyType        = "type"
	testKeyTargetURL   = "target_url"
	testKeyDescription = "description"
	testResourceIDFix  = "r_prod_db" // canonical test resource_id
	// mintByTestResourcePath is the resource-scoped mint endpoint
	// that `client.Create` hits when given a ResourceID (alias-form
	// /qurl get). Lifted so the alias-form tests register their
	// httptest mock at the same path the bot actually calls.
	mintByTestResourcePath = "/v1/resources/" + testResourceIDFix + "/qurls"
	testCmdSlash           = "/qurl"
	testFieldCallbackID    = "callback_id"
)

// slogTestLogger returns a logger that discards output so test
// fixtures can pass a slog.Logger without polluting -v output.
func slogTestLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
