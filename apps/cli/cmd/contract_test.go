package main

import (
	"net/http"
	"os"
	"path/filepath"
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

// TestPublishFoundExistingNote pins the already-published UX under --quiet:
// success plus a stderr note, never an error, and stdout stays CRID-only.
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

// TestPublishFoundExistingTextAnatomy pins the replay document in text mode:
// exit 0, the "Already published" headline, the explanatory note with its
// next step, no raw resource id, and the CRID still last, alone on its line.
func TestPublishFoundExistingTextAnatomy(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/data"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	// The document itself tells the story in text mode; the stderr note
	// belongs to --quiet and JSON only, so any stderr here is double-noting.
	if res.stderr.Len() != 0 {
		t.Errorf("text mode must not also note the replay on stderr, got %q", res.stderr.String())
	}
	stdout := res.stdout.String()
	if !strings.HasPrefix(stdout, "Already published\n") {
		t.Errorf("headline must say the resource already existed, got %q", stdout)
	}
	if !strings.Contains(stdout, "already has an active resource") || !strings.Contains(stdout, "Delete it first") {
		t.Errorf("expected the what-happened note with its next step, got %q", stdout)
	}
	if strings.Contains(stdout, "Resource ID:") {
		t.Errorf("the raw resource id must not render in text mode, got %q", stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if last, want := lines[len(lines)-1], "CRID: "+srv.Key.CRID; last != want {
		t.Errorf("last line = %q, want %q (the CRID must stay last)", last, want)
	}
}

// TestPublishNoCRIDKeepsResourceID pins the fallback the no-CRID warning
// names: when the service mints no CRID, the text document must still carry
// an identifier, so the resource id row comes back for exactly that case.
func TestPublishNoCRIDKeepsResourceID(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishOmitCRID(true)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/data"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	// tabwriter turns the label's tab into padding, so assert the label and
	// the value separately rather than a single tab-joined string that can
	// never match.
	stdout := res.stdout.String()
	if !strings.Contains(stdout, "Resource ID:") || !strings.Contains(stdout, srv.Key.ResourceID) {
		t.Errorf("no-CRID publish must still show the labeled resource id, got %q", stdout)
	}
	if !strings.Contains(res.stderr.String(), "did not return a CRID") {
		t.Errorf("expected the no-CRID warning, got %q", res.stderr.String())
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

// TestVersionAndCompletionSurviveBrokenConfig pins the shell-init contract:
// a malformed or secret-bearing legacy config file must never brick
// `eval "$(qurl completion bash)"` or `qurl version` — neither command
// touches settings, credentials, or the network.
func TestVersionAndCompletionSurviveBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("api_key: lv_live_shouldnotbehere000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"version"}, {"completion", "bash"}} {
		res := runCLI(t, &runOpts{args: args, configDir: dir})
		if res.code != 0 {
			t.Errorf("%v exit = %d with a secret-bearing config; stderr: %s", args, res.code, res.stderr.String())
		}
	}
}

// TestDocsRejectsUnknownMode pins usage discipline on the hidden docs
// command: an invalid mode is a usage error, not a zero-exit help print.
func TestDocsRejectsUnknownMode(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"docs", "bogus"}})
	if res.code != 2 {
		t.Errorf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
}

// TestResolveSubSecondTTLRefused pins clamp-and-report: a requested lifetime
// is never silently dropped, and a sub-second --ttl would truncate to zero
// on the whole-second wire.
func TestResolveSubSecondTTLRefused(t *testing.T) {
	srv := apitest.NewServer(t) // never contacted
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID, "--ttl", "500ms"}})
	if res.code != 2 {
		t.Errorf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Error("sub-second ttl must be refused before any request")
	}
}

// TestListJSONCarriesHasMore pins the scripting contract: has_more — not
// next_cursor presence — is the pagination terminator, so the JSON
// projection must carry it even on a zero-item page.
func TestListJSONCarriesHasMore(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{}, map[string]any{"next_cursor": "page2", "has_more": true})
	})
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "list"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), `"has_more": true`) {
		t.Errorf("json list must carry has_more on an empty-but-more page; got %s", res.stdout.String())
	}
}

// TestPublishJSONCarriesFoundExisting mirrors the text-mode note for
// scripts: the replay document states found_existing: true and still carries
// the existing resource's CRID. The fresh-publish document's explicit
// found_existing: false is pinned by the publish JSON golden.
func TestPublishJSONCarriesFoundExisting(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "publish", "https://example.com/data"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), `"found_existing": true`) {
		t.Errorf("json publish must carry found_existing; got %s", res.stdout.String())
	}
	if !strings.Contains(res.stdout.String(), `"crid": "`+srv.Key.CRID+`"`) {
		t.Errorf("replay json must carry the existing resource's CRID; got %s", res.stdout.String())
	}
}

// TestInsecureEndpointWarning pins the cleartext-credential warning table:
// plain http warns only off-loopback (mocks and harnesses are exempt).
func TestInsecureEndpointWarning(t *testing.T) {
	cases := map[string]bool{
		"http://api.example.com":    true,
		"http://192.0.2.10":         true,
		"https://api.example.com":   false,
		"http://localhost:8080":     false,
		"http://api.localhost:8080": false,
		"http://127.0.0.1:8080":     false,
		"http://[::1]:8080":         false,
	}
	for endpoint, wantWarn := range cases {
		if got := insecureEndpointWarning(endpoint) != ""; got != wantWarn {
			t.Errorf("insecureEndpointWarning(%q) warned=%t, want %t", endpoint, got, wantWarn)
		}
	}
}
