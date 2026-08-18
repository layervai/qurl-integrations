package apitest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// The platform's pinned error envelope:
//
//	{"error": {type, title, status, detail, instance, code}, "meta": {request_id}}
//
// error.code is the only programmatic contract; type is mechanically derived
// from code and detail is prose. The validation variant adds invalid_fields
// (possibly null) inside error.

// WriteEnvelope writes the platform's success envelope: {"data": ..., "meta":
// {...}}. meta always carries a request_id; pass extra meta fields (like
// next_cursor or has_more) through meta.
func WriteEnvelope(t *testing.T, w http.ResponseWriter, status int, data any, meta map[string]any) {
	t.Helper()
	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["request_id"]; !ok {
		meta["request_id"] = "req_test"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": meta}); err != nil {
		t.Errorf("encode envelope: %v", err)
	}
}

// WriteProblem writes the pinned error envelope for one problem code.
func WriteProblem(t *testing.T, w http.ResponseWriter, status int, code, title, detail string) {
	t.Helper()
	WriteProblemExtra(t, w, status, code, title, detail, nil, nil)
}

// WriteProblemExtra writes the pinned error envelope with optional
// invalid_fields (the validation variant; an explicit nil is serialized
// when includeNullFields carries the key) and extra meta entries.
func WriteProblemExtra(t *testing.T, w http.ResponseWriter, status int, code, title, detail string, invalidFields map[string]string, extraMeta map[string]any) {
	t.Helper()
	errObj := map[string]any{
		// type is mechanically derived from code server-side; consumers must
		// match on code only.
		"type":      "https://api.layerv.ai/errors/" + code,
		"title":     title,
		fieldStatus: status,
		"detail":    detail,
		"instance":  "/test/instance",
		"code":      code,
	}
	if invalidFields != nil {
		errObj["invalid_fields"] = invalidFields
	}
	meta := map[string]any{"request_id": "req_test"}
	for k, v := range extraMeta {
		meta[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"error": errObj, "meta": meta}); err != nil {
		t.Errorf("encode problem: %v", err)
	}
}

// Handler429 returns a handler answering 429 with the given Retry-After
// seconds, for scripting rate-limit sequences.
func Handler429(t *testing.T, retryAfterSeconds int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", "slow down")
	}
}

// HandlerDark503 answers the dark-surface 503 the production environment
// serves before temporary access links are promoted: code
// service_unavailable with Retry-After: 60. Clients must NOT auto-retry it —
// it reflects deployment state, not transience — which the transport's
// 429-only retry policy honors by construction.
func HandlerDark503(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable",
			"temporary access links are not enabled in this environment")
	}
}

// HandlerRevoked400 answers a resolve of a revoked (API-deleted) resource:
// the owner-truthful 400 `revoked`, deliberately distinct from the ambiguous
// 404 anti-oracle.
func HandlerRevoked400(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusBadRequest, "revoked", "Resource Revoked",
			"this resource was revoked and no longer resolves")
	}
}

// HandlerTombstoned410 answers a resolve of a tombstoned resource: 410
// `resource_tombstoned` plus meta.tombstone.
func HandlerTombstoned410(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblemExtra(t, w, http.StatusGone, "resource_tombstoned", "Gone",
			"this resource lifecycle is closed", nil,
			map[string]any{"tombstone": map[string]any{"closed_at": fixtureCreatedAt}})
	}
}

// HandlerNotFound404 answers with one of the platform's two not-found code
// spellings: resolve emits resource_not_found, every other resource route
// emits not_found.
func HandlerNotFound404(t *testing.T, code string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusNotFound, code, "Not Found",
			"the requested resource does not exist")
	}
}

// HandlerInsufficientScope403 answers a resolve with a key that lacks the
// dedicated resolve scope.
func HandlerInsufficientScope403(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusForbidden, "insufficient_scope", "Forbidden",
			"this key does not carry the scope this operation requires")
	}
}

// HandlerAPIKeyInvalid401 answers as the platform does for a key it does not
// recognize (revoked or never minted): 401 `api_key_invalid`.
func HandlerAPIKeyInvalid401(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusUnauthorized, "api_key_invalid", "Unauthorized",
			"the provided API key is not valid")
	}
}

// HandlerAPIKeyExpired401 answers as the platform does for a key past its
// expiry: 401 `api_key_expired`.
func HandlerAPIKeyExpired401(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusUnauthorized, "api_key_expired", "Unauthorized",
			"the provided API key has expired")
	}
}

// HandlerAccountFrozen403 answers as the platform does when the account
// behind an otherwise-valid key is frozen: 403 `account_frozen`.
func HandlerAccountFrozen403(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(t, w, http.StatusForbidden, "account_frozen", "Forbidden",
			"this account is frozen")
	}
}
