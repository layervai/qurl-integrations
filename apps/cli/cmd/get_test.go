package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// T3/T4 tests for `qurl get`: the real command tree against the mock qURL
// API and its link-host route, plus the harness contracts (no hangs, no
// browser off a terminal, binary-clean stdout).

// resolveRoute is the mock's resolve path for the fixture CRID.
func resolveRoute(srv *apitest.Server) string {
	return "/v1/resources/" + srv.Key.CRID + "/resolve"
}

// downloadServer returns a mock whose resolve answers point at its own
// link-host route, so downloads stay in-process.
func downloadServer(t *testing.T) *apitest.Server {
	t.Helper()
	srv := apitest.NewServer(t)
	srv.SetResolveQURL(srv.URL + apitest.DownloadPath)
	return srv
}

func handlerGone(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }

func TestGetDownloadEndToEnd(t *testing.T) {
	srv := downloadServer(t)
	dest := filepath.Join(t.TempDir(), "out.bin")
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", dest},
		browser: browser,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := readTestFile(t, dest); string(got) != apitest.DefaultDownloadPayload {
		t.Errorf("downloaded file = %q, want the mock payload", got)
	}
	mustNotExistCmd(t, dest+".part")
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "Saved to "+dest) {
		t.Errorf("expected the saved-to confirmation, got %q", res.stderr.String())
	}
	if len(browser.opened) != 0 {
		t.Errorf("download mode launched a browser: %q", browser.opened)
	}

	requests := srv.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want resolve then download", len(requests))
	}
	if requests[0].Method != http.MethodPost || requests[0].Path != resolveRoute(srv) {
		t.Errorf("first request = %s %s, want the resolve", requests[0].Method, requests[0].Path)
	}
	if requests[1].Method != http.MethodGet || requests[1].Path != apitest.DownloadPath {
		t.Errorf("second request = %s %s, want the download GET", requests[1].Method, requests[1].Path)
	}
	// The API credential authenticates the resolve and must never follow
	// the minted link to the download host.
	if auth := requests[0].Header.Get("Authorization"); auth == "" {
		t.Error("resolve request lost its Authorization header")
	}
	if auth := requests[1].Header.Get("Authorization"); auth != "" {
		t.Errorf("download request carried Authorization (%d bytes); the key must never reach the link host", len(auth))
	}
}

// TestGetExpiryRetrySequence pins the T3 retry contract end to end: first
// GET 410 → re-resolve → second GET succeeds.
func TestGetExpiryRetrySequence(t *testing.T) {
	srv := downloadServer(t)
	srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
	dest := filepath.Join(t.TempDir(), "out.bin")
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", dest},
		browser: browser,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := readTestFile(t, dest); string(got) != apitest.DefaultDownloadPayload {
		t.Errorf("downloaded file = %q, want the mock payload", got)
	}

	requests := srv.Requests()
	sequence := make([]string, 0, len(requests))
	for _, req := range requests {
		sequence = append(sequence, req.Method+" "+req.Path)
	}
	want := []string{
		http.MethodPost + " " + resolveRoute(srv),
		http.MethodGet + " " + apitest.DownloadPath,
		http.MethodPost + " " + resolveRoute(srv),
		http.MethodGet + " " + apitest.DownloadPath,
	}
	if strings.Join(sequence, "\n") != strings.Join(want, "\n") {
		t.Errorf("request sequence:\n%s\nwant:\n%s", strings.Join(sequence, "\n"), strings.Join(want, "\n"))
	}
	if len(browser.opened) != 0 {
		t.Errorf("expiry retry launched a browser: %q", browser.opened)
	}
}

// TestGetRetryReverifiesFreshAnswer pins that the mid-download re-resolve
// goes through the same fail-closed verification: a substituted answer on
// the retry aborts with exit 12 and no file.
func TestGetRetryReverifiesFreshAnswer(t *testing.T) {
	srv := downloadServer(t)
	otherCRID := apitest.DeriveCRID(t, []byte("a-different-resource-key"), apitest.VersionTest)
	mintAnswer := func(crid string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"qurl":               srv.URL + apitest.DownloadPath,
				"crid":               crid,
				"type":               "qv2",
				"expires_at":         "2026-03-01T00:05:00Z",
				"expires_in_seconds": 300,
				"single_use":         true,
			}, nil)
		}
	}
	srv.Script(http.MethodPost, resolveRoute(srv), mintAnswer(srv.Key.CRID), mintAnswer(otherCRID))
	srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
	dest := filepath.Join(t.TempDir(), "out.bin")

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", dest}})
	if res.code != 12 {
		t.Fatalf("exit = %d, want 12; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	mustNotExistCmd(t, dest)
	mustNotExistCmd(t, dest+".part")

	// The tainted answer must not be fetched: one download GET only.
	downloads := 0
	for _, req := range srv.Requests() {
		if req.Path == apitest.DownloadPath {
			downloads++
		}
	}
	if downloads != 1 {
		t.Errorf("download GETs = %d, want 1 (nothing fetched after failed verification)", downloads)
	}
}

func TestGetBrowserOpensVerifiedLinkOnTTY(t *testing.T) {
	srv := apitest.NewServer(t)
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID},
		tty:     true,
		browser: browser,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	wantLink := "https://qurl.link/#qv2.test.link"
	if len(browser.opened) != 1 || browser.opened[0] != wantLink {
		t.Fatalf("browser opened %q, want exactly [%s]", browser.opened, wantLink)
	}
	if !strings.Contains(res.stdout.String(), wantLink) {
		t.Errorf("stdout %q should carry the link", res.stdout.String())
	}
	if !strings.Contains(res.stderr.String(), "Opening it in your browser") {
		t.Errorf("expected the launch note on stderr, got %q", res.stderr.String())
	}
}

func TestGetBrowserFailureStillLeavesTheLink(t *testing.T) {
	srv := apitest.NewServer(t)
	browser := &fakeBrowser{err: errors.New("no display")}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID},
		tty:     true,
		browser: browser,
	})
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), "https://qurl.link/") {
		t.Errorf("the link must be printed before the launch attempt; stdout = %q", res.stdout.String())
	}
	if !strings.Contains(res.stderr.String(), "couldn't open your browser") {
		t.Errorf("expected the launcher failure message, got %q", res.stderr.String())
	}
}

// TestGetVerifyMismatchNeverActs pins fail-closed ordering: a mismatched
// resolve answer means no browser, no bytes, exit 12.
func TestGetVerifyMismatchNeverActs(t *testing.T) {
	srv := apitest.NewServer(t)
	other := apitest.GenerateResourceKey(t)
	srv.SetResolveCRID(other.CRID)
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID},
		tty:     true,
		browser: browser,
	})
	if res.code != 12 {
		t.Fatalf("exit = %d, want 12; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if len(browser.opened) != 0 {
		t.Errorf("verification failure still launched a browser: %q", browser.opened)
	}
}

// TestGetPipedWithoutFileRefusedBeforeNetwork pins §16.2: the refusal is
// local — no request, no browser, exit 2, remedy on stderr.
func TestGetPipedWithoutFileRefusedBeforeNetwork(t *testing.T) {
	srv := apitest.NewServer(t)
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID},
		browser: browser,
	})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	for _, remedy := range []string{"--file", "qurl resolve"} {
		if !strings.Contains(res.stderr.String(), remedy) {
			t.Errorf("refusal must point at %s, got %q", remedy, res.stderr.String())
		}
	}
	if len(srv.Requests()) != 0 {
		t.Errorf("refusal must precede any request, saw %d", len(srv.Requests()))
	}
	if len(browser.opened) != 0 {
		t.Errorf("piped get launched a browser: %q", browser.opened)
	}
}

// TestGetExistingFileRefusedBeforeNetwork pins the overwrite refusal to
// exit 7 (Conflict) and to firing before any credential or network use.
func TestGetExistingFileRefusedBeforeNetwork(t *testing.T) {
	srv := apitest.NewServer(t)
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", dest}})
	if res.code != 7 {
		t.Fatalf("exit = %d, want 7; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "--force") {
		t.Errorf("expected the --force remedy, got %q", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Errorf("overwrite refusal must precede any request, saw %d", len(srv.Requests()))
	}
	if got := readTestFile(t, dest); string(got) != "precious" {
		t.Errorf("existing file was touched: %q", got)
	}
}

func TestGetForceReplacesExistingFile(t *testing.T) {
	srv := downloadServer(t)
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dest, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", dest, "--force"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := readTestFile(t, dest); string(got) != apitest.DefaultDownloadPayload {
		t.Errorf("destination = %q, want the fresh payload", got)
	}
}

func TestGetQuietPrintsDestinationPath(t *testing.T) {
	srv := downloadServer(t)
	dest := filepath.Join(t.TempDir(), "out.bin")

	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "get", srv.Key.CRID, "--file", dest}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got, want := res.stdout.String(), dest+"\n"; got != want {
		t.Errorf("quiet stdout = %q, want %q", got, want)
	}
}

// TestGetUsageRefusals pins the flag-level guards: an explicitly empty
// --file and the raw-bytes/JSON combination are usage errors before any
// network traffic.
func TestGetUsageRefusals(t *testing.T) {
	cases := map[string][]string{
		"empty file value": {"get", "%CRID%", "--file", ""},
		"dash with json":   {"-o", "json", "get", "%CRID%", "--file", "-"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			resolved := make([]string, len(args))
			for i, a := range args {
				resolved[i] = strings.ReplaceAll(a, "%CRID%", srv.Key.CRID)
			}
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, resolved...)})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			if len(srv.Requests()) != 0 {
				t.Errorf("usage refusal must precede any request, saw %d", len(srv.Requests()))
			}
		})
	}
}

// TestGetTestCRIDOnProductionRefusedWithoutYes pins the same environment
// guard resolve and delete carry, on the same exit code, before any
// request — and that no browser starts either.
func TestGetTestCRIDOnProductionRefusedWithoutYes(t *testing.T) {
	srv := apitest.NewServer(t) // never contacted
	browser := &fakeBrowser{}
	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", "https://api.layerv.ai", "get", srv.Key.CRID},
		tty:     true,
		browser: browser,
	})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "--yes") {
		t.Errorf("expected the --yes remedy, got %q", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Error("the guard must fire before any request")
	}
	if len(browser.opened) != 0 {
		t.Errorf("the guard must fire before any browser launch: %q", browser.opened)
	}
}

// TestGetFileDashBinaryCleanStdout is the T4 harness contract: with a
// closed (empty) stdin and piped stdout, `get --file -` terminates, writes
// the payload bytes verbatim — NULs, CRs, escapes and all — and decorates
// nothing.
func TestGetFileDashBinaryCleanStdout(t *testing.T) {
	payload := []byte("bin\x00ary\r\n\x1b[31mpay\xff\xfeload")
	srv := downloadServer(t)
	srv.SetDownloadPayload(payload)
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "-"},
		browser: browser,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if !bytes.Equal(res.stdout.Bytes(), payload) {
		t.Errorf("stdout = %q, want the exact payload bytes", res.stdout.Bytes())
	}
	if res.stderr.Len() != 0 {
		t.Errorf("--file - must not decorate any stream, stderr = %q", res.stderr.String())
	}
	if len(browser.opened) != 0 {
		t.Errorf("--file - launched a browser: %q", browser.opened)
	}
}

// TestGetFileDashOnTerminalIsAllowed pins that an explicit --file - wins
// even on a TTY: the user asked for bytes, not a browser.
func TestGetFileDashOnTerminalIsAllowed(t *testing.T) {
	srv := downloadServer(t)
	browser := &fakeBrowser{}

	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "-"},
		tty:     true,
		browser: browser,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := res.stdout.String(); got != apitest.DefaultDownloadPayload {
		t.Errorf("stdout = %q, want the payload", got)
	}
	if len(browser.opened) != 0 {
		t.Errorf("--file - on a TTY launched a browser: %q", browser.opened)
	}
}

// readTestFile reads a file this test created under t.TempDir.
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- path is a t.TempDir destination the test itself chose.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func mustNotExistCmd(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (stat err %v), want absent", path, err)
	}
}
