package internal

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Common keys for the qurl-service response envelope. Lifted to
// constants in tests because goconst would otherwise flag the 4+
// duplications across fixture builders.
const (
	testKeyData               = "data"
	testKeyError              = "error"
	testKeyAPIKey             = "api_key"
	testKeyExpiresAt          = "expires_at"
	testKeyExpiresIn          = "expires_in"
	testKeyKeyID              = "key_id"
	testKeyKeyType            = "key_type"
	testKeyKnockResourceID    = "knock_resource_id"
	testKeyConnectorRoutingID = "connector_routing_id"
	testKeyResourceID         = "resource_id"
	testKeySlug               = "slug"
	testKeyStatus             = "status"
	testKeyTitle              = "title"
	testKeyTunnelSlug         = "tunnel_slug"
	testKeyType               = "type"
	testKeyTargetURL          = "target_url"
	testKeyDescription        = "description"
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

// capturedLogs is a concurrency-safe sink for the default slog logger. The
// install path logs from a pool goroutine, so the buffer is read from a
// different goroutine than the one that wrote it.
type capturedLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLogs) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), substr)
}

func (c *capturedLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// findAuditRecord returns the "audit" group of the first captured JSON log
// record carrying the given event, or nil when none does. Non-JSON lines are
// skipped rather than fatal: the captured sink sees every record the test
// happens to produce, not only the one under assertion.
func findAuditRecord(logs *capturedLogs, event string) map[string]any {
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Audit map[string]any `json:"audit"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Audit != nil && record.Audit["event"] == event {
			return record.Audit
		}
	}
	return nil
}

// captureDefaultSlog redirects the default slog logger for one test and
// restores it on cleanup. Async install work logs through slog.With off the
// default logger, so this is the only seam that sees those records.
func captureDefaultSlog(t *testing.T) *capturedLogs {
	t.Helper()
	logs := &capturedLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// kindFirstRejection is the stable prefix of the log line emitted when a mint
// response does not confirm the kind-first contract and the install is failed
// closed. It is deliberately only the prefix — the emitted line continues with
// a remediation hint that is free to be reworded — and callers substring-match
// it. Shared so the fires/silent assertions cannot drift apart.
const kindFirstRejection = "tunnel install: minted credential did not confirm the kind-first contract"

// assertConnectorEnrollmentKind pins the kind/target pair that makes the
// minted credential a Connector-bound enrollment token rather than an ordinary
// key. Callers with extra per-path expectations (an expires_in, say) assert
// those separately.
func assertConnectorEnrollmentKind(t *testing.T, body map[string]any) {
	t.Helper()
	if body["kind"] != client.CredentialKindEnrollmentToken || body["target"] != client.CredentialTargetAgent {
		t.Errorf("api key body = %+v, want kind=%q target=%q",
			body, client.CredentialKindEnrollmentToken, client.CredentialTargetAgent)
	}
}

// assertNoConnectorClaims pins the decoded `POST /v1/api-keys` body to an
// agent-target enrollment token: the daemon enrolls its device identity with
// it, so it must not carry a connector claim (the resource is bound through
// the rendered share.yaml, not the credential).
func assertNoConnectorClaims(t *testing.T, body map[string]any) {
	t.Helper()
	if raw, ok := body["claims"]; ok && raw != nil {
		if arr, isArr := raw.([]any); !isArr || len(arr) != 0 {
			t.Errorf("api key body claims = %v, want none for an agent-target token; body=%+v", raw, body)
		}
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
