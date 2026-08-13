package internal

import (
	"io"
	"log/slog"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
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
	// A production-shaped public resource ID. Keeping the canonical get-flow
	// fixture above the old r_ length ensures Slack and the shared client do not
	// accidentally reintroduce the pre-cutover identifier contract.
	testPublicResourceID = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEN4yvBX3yjAvYl9qagkStIWB1ie2gp_LF2Jy0w5AdxXefsTNLn9nrOlA4umKRiIQeGfvad9OFVoWa3PAIxcy4qg"
	testResourceIDFix    = testPublicResourceID
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

// assertSingleConnectorClaim pins the decoded `POST /v1/api-keys` body to
// exactly one Connector claim bound to slug. Every step is type-checked so a
// wrong-shaped body reports a readable failure instead of panicking the test
// binary on a bad assertion.
func assertSingleConnectorClaim(t *testing.T, body map[string]any, slug string) {
	t.Helper()
	raw, ok := body["claims"].([]any)
	if !ok {
		t.Errorf("api key body claims = %v, want a JSON array; body=%+v", body["claims"], body)
		return
	}
	if len(raw) != 1 {
		t.Errorf("api key body has %d claims, want exactly 1; body=%+v", len(raw), body)
		return
	}
	claim, ok := raw[0].(map[string]any)
	if !ok {
		t.Errorf("api key body claims[0] = %v, want a JSON object; body=%+v", raw[0], body)
		return
	}
	if claim[testKeyType] != client.CredentialClaimTypeConnector || claim["id"] != slug {
		t.Errorf("api key body claim = %+v, want {type:%q, id:%q}", claim, client.CredentialClaimTypeConnector, slug)
	}
}

// assertNoRetiredCredentialFields pins the kind-first cutover: the retired
// request fields must never reappear on the wire. They are no longer fields on
// client.CreateAPIKeyInput, so this fires only if someone re-adds one.
func assertNoRetiredCredentialFields(t *testing.T, body map[string]any) {
	t.Helper()
	for _, retired := range []string{testKeyKeyType, testKeyTunnelSlug, "scopes", "purpose"} {
		if _, ok := body[retired]; ok {
			t.Errorf("api key body contained retired %s field: %+v", retired, body)
		}
	}
}
