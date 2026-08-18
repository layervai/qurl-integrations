package qurlapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func newTestClient(t *testing.T, srv *apitest.Server, sleeps *[]time.Duration) Client {
	t.Helper()
	cfg := Config{
		BaseURL:      srv.URL,
		APIKey:       "lv_test_apitestingvalue123456789",
		Version:      "test",
		NewRequestID: func() string { return "unit-req" },
	}
	if sleeps != nil {
		cfg.Sleep = func(d time.Duration) { *sleeps = append(*sleeps, d) }
	} else {
		cfg.Sleep = func(time.Duration) {}
	}
	client, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestPublishSendsPinnedWireShape(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	// The mock enforces the pinned contract (type=url + target_url required),
	// so a successful publish IS the wire-shape assertion.
	res, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CRID != srv.Key.CRID || res.ResourceID != srv.Key.ResourceID {
		t.Errorf("mapped identity mismatch: %+v", res)
	}
	if res.TargetURL != "https://example.com/data" {
		t.Errorf("target = %q", res.TargetURL)
	}
	if res.FoundExisting {
		t.Error("fresh publish must not report found_existing")
	}

	srv.SetPublishFoundExisting(true)
	res, err = client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.FoundExisting {
		t.Error("meta.found_existing must surface")
	}
}

func TestPublishValidatesTargetLocally(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)
	for name, target := range map[string]string{
		"empty":       "",
		"scheme":      "ftp://example.com",
		"no host":     "https://",
		"credentials": "https://user:pass@example.com/x",
	} {
		if _, err := client.Publish(context.Background(), target, PublishOptions{}); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidResourceRequest", name, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("local validation must not send requests, saw %d", got)
	}
}

func TestResolveVerifyKeyPassesAndFailsClosed(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	res, err := client.Resolve(context.Background(), srv.Key.ResourceID, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.VerifyKey(srv.Key.DER); err != nil {
		t.Errorf("consistent response must verify: %v", err)
	}
	other := apitest.GenerateResourceKey(t)
	if err := res.VerifyKey(other.DER); !errors.Is(err, qurl.ErrCRIDMismatch) {
		t.Errorf("wrong key: err = %v, want ErrCRIDMismatch", err)
	}

	// A Resolved constructed without the SDK wiring fails closed, never open.
	bare := &Resolved{QURL: "https://qurl.link/#x"}
	if err := bare.VerifyKey(srv.Key.DER); !errors.Is(err, qurl.ErrNoCRID) {
		t.Errorf("unwired VerifyKey = %v, want ErrNoCRID", err)
	}
}

func TestResolveDark503PreservesSentinel(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.HandlerDark503(t))
	client := newTestClient(t, srv, nil)

	_, err := client.Resolve(context.Background(), srv.Key.CRID, ResolveOptions{})
	if !errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		t.Fatalf("err = %v, want the dark-surface sentinel preserved through mapping", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("typed Error with 503 must also be reachable, got %v", err)
	}
}

func TestTransportRetries429ThenSucceeds(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", apitest.Handler429(t, 2), apitest.Handler429(t, 1))

	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	page, err := client.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Errorf("items = %d, want the default row", len(page.Items))
	}
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 1*time.Second {
		t.Errorf("sleeps = %v, want [2s 1s] from Retry-After", sleeps)
	}
	if got := len(srv.Requests()); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestTransportRetryAfterFallbackAndCap(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if d := retryDelay(resp, 1); d != 500*time.Millisecond {
		t.Errorf("fallback delay = %v", d)
	}
	resp.Header.Set("Retry-After", "not-a-number")
	if d := retryDelay(resp, 2); d != time.Second {
		t.Errorf("unparseable Retry-After delay = %v", d)
	}
	resp.Header.Set("Retry-After", "3600")
	if d := retryDelay(resp, 1); d != maxRetryAfter {
		t.Errorf("capped delay = %v, want %v", d, maxRetryAfter)
	}
}

// TestDeleteIsIdempotent pins the delete contract: 204 and 404 are both
// success — the second delete of anything reports AlreadyGone, never an
// error — while other failures stay typed.
func TestDeleteIsIdempotent(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	result, err := client.Delete(context.Background(), srv.Key.CRID)
	if err != nil || result.AlreadyGone {
		t.Fatalf("fresh delete: result=%+v err=%v", result, err)
	}

	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, apitest.HandlerNotFound404(t, "not_found"))
	result, err = client.Delete(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatalf("hard-deleted delete must succeed: %v", err)
	}
	if !result.AlreadyGone {
		t.Error("404 delete must report AlreadyGone")
	}

	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusForbidden, "insufficient_scope", "Forbidden", "not yours")
	})
	_, err = client.Delete(context.Background(), srv.Key.CRID)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "insufficient_scope" {
		t.Errorf("typed failure lost: %v", err)
	}
	if apiErr.RequestID != "req_test" {
		t.Errorf("meta.request_id not mapped: %+v", apiErr)
	}
}

// TestDark503IsNeverAutoRetried pins the no-retry rule for the dark surface:
// its Retry-After header reflects deployment state, not transience, so the
// transport must surface it on the first answer.
func TestDark503IsNeverAutoRetried(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", 2, apitest.HandlerDark503(t))

	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	_, err := client.Resolve(context.Background(), srv.Key.CRID, ResolveOptions{})
	if !errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		t.Fatalf("err = %v", err)
	}
	if len(sleeps) != 0 {
		t.Errorf("503 must not be retried; slept %v", sleeps)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestListParsesEnvelopeAndCursor(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "active" {
			t.Errorf("status param = %q", got)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{
			{"resource_id": "r1", "crid": "c1", "target_url": "https://a.example", "status": "active"},
			{"resource_id": "r2", "target_url": "https://b.example", "status": "revoked"},
		}, map[string]any{"next_cursor": "cur2", "has_more": true})
	})
	client := newTestClient(t, srv, nil)

	page, err := client.List(context.Background(), ListOptions{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor != "cur2" || !page.HasMore {
		t.Errorf("page = %+v", page)
	}
	if page.Items[1].CRID != "" || page.Items[1].ResourceID != "r2" {
		t.Errorf("row without crid must survive projection: %+v", page.Items[1])
	}
}

func TestProblemErrorParsesPinnedEnvelope(t *testing.T) {
	var e *Error

	// The pinned envelope: error.{type,title,status,detail,instance,code} +
	// meta.request_id. code is the programmatic field.
	pinned := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"type":"https://api.layerv.ai/errors/revoked","title":"Resource Revoked","status":400,"detail":"prose that may change","instance":"/v1/x","code":"revoked"},"meta":{"request_id":"req_env"}}`)}).problem()
	if !errors.As(pinned, &e) || e.Code != "revoked" || e.Title != "Resource Revoked" || e.RequestID != "req_env" {
		t.Errorf("pinned envelope: %+v", e)
	}

	// The validation variant carries invalid_fields inside error; null must
	// be tolerated.
	validation := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"code":"invalid_request","title":"Bad Request","invalid_fields":{"target_url":"is required"}},"meta":{"request_id":"req_v"}}`)}).problem()
	if !errors.As(validation, &e) || e.InvalidFields["target_url"] != "is required" {
		t.Errorf("validation variant: %+v", e)
	}
	nullFields := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"code":"invalid_request","title":"Bad Request","invalid_fields":null},"meta":{"request_id":"req_n"}}`)}).problem()
	if !errors.As(nullFields, &e) || e.Code != "invalid_request" || len(e.InvalidFields) != 0 {
		t.Errorf("null invalid_fields must be tolerated: %+v", e)
	}

	// Flat fields stay accepted as an intermediary fallback.
	flat := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"code":"bad","title":"Bad","detail":"flat shape","request_id":"req_flat"}`)}).problem()
	if !errors.As(flat, &e) || e.Code != "bad" || e.Detail != "flat shape" || e.RequestID != "req_flat" {
		t.Errorf("flat shape: %+v", flat)
	}

	raw := (&restReply{status: 502, header: http.Header{}, body: []byte("<html>bad gateway</html>")}).problem()
	if !errors.As(raw, &e) || !strings.Contains(e.Detail, "bad gateway") {
		t.Errorf("raw body snippet: %+v", e)
	}

	retryHeader := http.Header{}
	retryHeader.Set("Retry-After", "7")
	limited := (&restReply{status: 429, header: retryHeader, body: []byte(`{}`)}).problem()
	if !errors.As(limited, &e) || e.RetryAfter != 7 {
		t.Errorf("retry-after: %+v", e)
	}
}

func TestRedact(t *testing.T) {
	in := "key lv_live_abc123DEF456ghi789jkl and Bearer lv_test_zzz999yyy888xxx777www here"
	got := Redact(in)
	if strings.Contains(got, "abc123DEF") || strings.Contains(got, "zzz999") {
		t.Fatalf("secrets survived: %q", got)
	}
	if !strings.Contains(got, "lv_***") {
		t.Errorf("marker missing: %q", got)
	}
}

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	_, err := New(&Config{APIKey: "lv_test_apitestingvalue123456789"})
	if !errors.Is(err, qurl.ErrInvalidClientConfig) {
		t.Errorf("err = %v, want ErrInvalidClientConfig", err)
	}
}
