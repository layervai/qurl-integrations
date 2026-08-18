package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// T3 contract tests: the implemented commands against the mock qURL API,
// asserting the wire contract (headers, retry, verification, typed-error UX)
// rather than rendering bytes (goldens own those).

func TestPublishContract(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--quiet", "publish", "https://example.com/data"}})

	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got, want := res.stdout.String(), srv.Key.CRID+"\n"; got != want {
		t.Errorf("quiet stdout = %q, want %q", got, want)
	}

	requests := srv.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	req := requests[0]
	if req.Method != http.MethodPost || req.Path != "/v1/resources" {
		t.Errorf("request = %s %s, want POST /v1/resources", req.Method, req.Path)
	}
	if ua := req.Header.Get("User-Agent"); ua != "qurl-cli/test" {
		t.Errorf("User-Agent = %q, want qurl-cli/test", ua)
	}
	if rid := req.Header.Get("X-Request-Id"); rid != "cli-req-fixed" {
		t.Errorf("X-Request-Id = %q, want the harness-injected id", rid)
	}
	if auth := req.Header.Get("Authorization"); auth != "Bearer "+testAPIKey {
		t.Errorf("Authorization header not set as expected (got %d bytes)", len(auth))
	}
}

func TestListContractHeadersAndPagination(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cursor"); got != "page2" {
			t.Errorf("cursor = %q, want page2", got)
		}
		if got := r.URL.Query().Get("limit"); got != "5" {
			t.Errorf("limit = %q, want 5", got)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{
			"resource_id": srv.Key.ResourceID,
			"crid":        srv.Key.CRID,
			"target_url":  "https://example.com/data",
			"status":      "active",
		}}, map[string]any{"next_cursor": "page3", "has_more": true})
	})

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "list", "--cursor", "page2", "--limit", "5"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "--cursor page3") {
		t.Errorf("expected next-page note on stderr, got %q", res.stderr.String())
	}
	req := srv.Requests()[0]
	if req.Header.Get("User-Agent") != "qurl-cli/test" || req.Header.Get("X-Request-Id") == "" {
		t.Errorf("direct REST path missing CLI headers: %+v", req.Header)
	}
}

func TestListQuietPrintsFullCRIDs(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "list"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got, want := res.stdout.String(), srv.Key.CRID+"\n"; got != want {
		t.Errorf("quiet list stdout = %q, want %q", got, want)
	}
}

func TestResolveByCRIDEchoVerifies(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := res.stdout.String(); got != "https://qurl.link/#qv2.test.link\n" {
		t.Errorf("piped resolve stdout = %q, want the bare link", got)
	}
}

func TestResolveByResourceKeyVerifies(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.ResourceID}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), "https://qurl.link/") {
		t.Errorf("expected link on stdout, got %q", res.stdout.String())
	}
}

func TestResolveWrongKeyCRIDMismatchEmitsNothingExit12(t *testing.T) {
	srv := apitest.NewServer(t)
	other := apitest.GenerateResourceKey(t)
	srv.SetResolveCRID(other.CRID)

	// By CRID: the echo check fails.
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 12 {
		t.Fatalf("exit = %d, want 12; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "nothing was printed") {
		t.Errorf("expected fail-closed message on stderr, got %q", res.stderr.String())
	}

	// By resource key: VerifyKey fails against the delivered CRID.
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.ResourceID}})
	if res.code != 12 {
		t.Fatalf("key-form exit = %d, want 12; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

func TestResolveResponseWithoutCRIDFailsClosed(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"qurl": "https://qurl.link/#qv2.test.link",
			"type": "qv2",
		}, nil)
	})
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 12 {
		t.Fatalf("exit = %d, want 12; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

func TestResolve429RetryAfterHonored(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.Handler429(t, 3))

	var sleeps []time.Duration
	res := runCLI(t, &runOpts{
		args:   []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID},
		sleeps: &sleeps,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d after retry, stderr: %s", res.code, res.stderr.String())
	}
	if len(sleeps) != 1 || sleeps[0] != 3*time.Second {
		t.Errorf("sleeps = %v, want exactly [3s] from Retry-After", sleeps)
	}
	if got := len(srv.Requests()); got != 2 {
		t.Errorf("requests = %d, want 2 (429 then success)", got)
	}
}

func TestRateLimitExhaustionSurfaces429(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodGet, "/v1/resources", 3, apitest.Handler429(t, 1))

	var sleeps []time.Duration
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "list"}, sleeps: &sleeps})
	if res.code != 9 {
		t.Fatalf("exit = %d, want 9; stderr: %s", res.code, res.stderr.String())
	}
	if len(sleeps) != 2 {
		t.Errorf("sleeps = %v, want two waits before giving up", sleeps)
	}
	if got := len(srv.Requests()); got != 3 {
		t.Errorf("requests = %d, want 3 attempts", got)
	}
}

func TestResolveDark503TypedUX(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.HandlerDark503(t))

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 11 {
		t.Fatalf("exit = %d, want 11; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "aren't available from this qURL endpoint") {
		t.Errorf("expected the dark-surface message, got %q", res.stderr.String())
	}
}

func TestDeleteContract(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	req := srv.Requests()[0]
	if req.Method != http.MethodDelete || req.Path != "/v1/resources/"+srv.Key.CRID {
		t.Errorf("request = %s %s, want DELETE of the CRID path", req.Method, req.Path)
	}
}

// TestDeleteIsIdempotentAtCLILevel pins the UX half of idempotent delete: a
// 404 on delete is success (exit 0) with an already-gone note, never an
// error.
func TestDeleteIsIdempotentAtCLILevel(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, apitest.HandlerNotFound404(t, "not_found"))
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "already deleted") {
		t.Errorf("expected the already-gone note, got %q", res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), "Deleted ") {
		t.Errorf("already-gone must not also claim a fresh deletion, got %q", res.stderr.String())
	}
}

// TestDeleteTestCRIDOnProductionRefusedWithoutYes pins the same environment
// guard on the destructive command that resolve carries: a test CRID aimed
// at the production endpoint is refused before any request without --yes.
func TestDeleteTestCRIDOnProductionRefusedWithoutYes(t *testing.T) {
	srv := apitest.NewServer(t) // never contacted
	res := runCLI(t, &runOpts{args: []string{"--endpoint", "https://api.layerv.ai", "delete", srv.Key.CRID}})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "production") {
		t.Errorf("expected the environment-guard message, got %q", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Error("the guard must fire before any request")
	}
}

// TestResolveAfterDeleteIsOwnerTruthful pins the revoked path: resolving a
// deleted resource answers 400 `revoked`, which the CLI maps to exit 5 with
// the owner-truthful message rather than the ambiguous 404 anatomy.
func TestResolveAfterDeleteIsOwnerTruthful(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.HandlerRevoked400(t))
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 5 {
		t.Fatalf("exit = %d, want 5; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "this resource was deleted") {
		t.Errorf("expected the owner-truthful hint, got %q", res.stderr.String())
	}
}

// TestResolveInsufficientScope pins the dedicated-scope failure UX.
func TestResolveInsufficientScope(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.HandlerInsufficientScope403(t))
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 6 {
		t.Fatalf("exit = %d, want 6; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "isn't allowed to request access links") {
		t.Errorf("expected the resolve-access hint, got %q", res.stderr.String())
	}
}

// TestPublishFoundExistingNote pins the already-published UX: success plus a
// stderr note, never an error.
func TestPublishFoundExistingNote(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--quiet", "publish", "https://example.com/data"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "already published") {
		t.Errorf("expected the found-existing note, got %q", res.stderr.String())
	}
	if got, want := res.stdout.String(), srv.Key.CRID+"\n"; got != want {
		t.Errorf("stdout = %q, want the CRID", got)
	}
}

// TestListZeroItemPageWithMoreDoesNotSayNotFound pins the has_more rule:
// empty pages with more behind them must not read as "nothing found".
func TestListZeroItemPageWithMoreDoesNotSayNotFound(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{},
			map[string]any{"has_more": true, "next_cursor": "cur9"})
	})
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "list"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), "No resources found") {
		t.Errorf("empty-but-has-more page must not claim nothing found: %q", res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "--cursor cur9") {
		t.Errorf("expected the continuation note, got %q", res.stderr.String())
	}
}

func TestDeleteNonInteractiveRequiresYes(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID}})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Error("no request may be sent when confirmation is refused")
	}
}

func TestDeleteInteractiveConfirmAndCancel(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "delete", srv.Key.CRID},
		stdin: strings.NewReader("y\n"),
		inTTY: true,
	})
	if res.code != 0 || len(srv.Requests()) != 1 {
		t.Fatalf("confirmed delete: exit=%d requests=%d, stderr: %s", res.code, len(srv.Requests()), res.stderr.String())
	}

	srv2 := apitest.NewServer(t)
	res = runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv2.URL, "delete", srv2.Key.CRID},
		stdin: strings.NewReader("n\n"),
		inTTY: true,
	})
	if res.code != 0 {
		t.Fatalf("canceled delete should exit 0, got %d", res.code)
	}
	if len(srv2.Requests()) != 0 {
		t.Error("canceled delete must not send a request")
	}
	if !strings.Contains(res.stderr.String(), "Canceled") {
		t.Errorf("expected cancel note, got %q", res.stderr.String())
	}
}

func TestTestCRIDOnProductionRefusedWithoutYes(t *testing.T) {
	srv := apitest.NewServer(t) // never contacted
	res := runCLI(t, &runOpts{args: []string{"--endpoint", "https://api.layerv.ai", "resolve", srv.Key.CRID}})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "--yes") {
		t.Errorf("expected the --yes remedy in the message, got %q", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Error("the guard must fire before any request")
	}
}

func TestProductionCRIDOnLocalEndpointWarnsAndProceeds(t *testing.T) {
	srv := apitest.NewServer(t)
	prodCRID := apitest.DeriveCRID(t, srv.Key.DER, apitest.VersionProduction)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", prodCRID}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "non-production endpoint") {
		t.Errorf("expected the environment warning, got %q", res.stderr.String())
	}
	if res.stdout.Len() == 0 {
		t.Error("warn-only mismatch must still emit the link")
	}
}

func TestCRIDTypoWarnsAndForwards(t *testing.T) {
	srv := apitest.NewServer(t)
	// Corrupt the final character to break the CRID's internal check while
	// keeping the alphabet and length valid.
	typo := srv.Key.CRID[:59] + flipCRIDChar(srv.Key.CRID[59])
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", typo}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "appears to contain a typo") {
		t.Errorf("expected the typo warning, got %q", res.stderr.String())
	}
	if len(srv.Requests()) != 1 {
		t.Errorf("typo-warned input must still be forwarded, requests = %d", len(srv.Requests()))
	}
}

func flipCRIDChar(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}
