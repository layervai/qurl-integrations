package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
)

// T3 contract tests for the credential commands: login's validate-then-store
// order, logout's every-backend removal, and whoami's key-resolution
// precedence — asserted against the mock platform and the injected storage
// chain (goldens own the rendered bytes).

// TestLoginValidatesThenStores pins the order: GET /v1/me first, storage
// only after the platform accepted the key.
func TestLoginValidatesThenStores(t *testing.T) {
	srv := apitest.NewServer(t)
	kr := &fakeKeyring{}
	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "login"},
		env:     map[string]string{},
		stdin:   strings.NewReader(testAPIKey + "\n"),
		keyring: kr,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}

	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/v1/me" {
		t.Fatalf("expected exactly one GET /v1/me, got %+v", requests)
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("login must validate the just-typed key, sent %d bytes", len(got))
	}
	if kr.key != testAPIKey {
		t.Errorf("keyring holds %q, want the validated key", kr.key)
	}
	for _, want := range []string{apitest.MeOwnerID, "OS keyring"} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Errorf("login confirmation must mention %q, got %q", want, res.stderr.String())
		}
	}
}

// TestLoginRejectedKeyIsNeverStored is the mock-that-rejects pin: a 401 from
// the platform means exit 4 and nothing in any backend.
func TestLoginRejectedKeyIsNeverStored(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
	kr := &fakeKeyring{}
	dir := t.TempDir()
	res := runCLI(t, &runOpts{
		args:      []string{"--endpoint", srv.URL, "login"},
		env:       map[string]string{},
		stdin:     strings.NewReader(testAPIKey + "\n"),
		keyring:   kr,
		configDir: dir,
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if kr.key != "" {
		t.Errorf("rejected key must not reach the keyring, holds %q", kr.key)
	}
	if _, err := auth.NewFileStore(dir).Load(); err == nil {
		t.Error("rejected key must not reach the file store")
	}
}

// TestLoginFallsBackToFileAndWarns pins the fallback save: with the keyring
// unavailable, the key lands in the 0600 credential file and the warning
// says so.
func TestLoginFallsBackToFileAndWarns(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := t.TempDir()
	res := runCLI(t, &runOpts{
		args:      []string{"--endpoint", srv.URL, "login"},
		env:       map[string]string{},
		stdin:     strings.NewReader(testAPIKey + "\n"),
		keyring:   &fakeKeyring{unavailable: true},
		configDir: dir,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	got, err := auth.NewFileStore(dir).Load()
	if err != nil || got != testAPIKey {
		t.Errorf("file store Load = %q, %v; want the validated key", got, err)
	}
	if !strings.Contains(res.stderr.String(), "mode 0600") {
		t.Errorf("fallback save must warn about the file, got %q", res.stderr.String())
	}
}

// TestLogoutRemovesEveryBackend pins logout: both backends cleared and
// reported, and the repeat run is the idempotent exit-0 note.
func TestLogoutRemovesEveryBackend(t *testing.T) {
	kr := &fakeKeyring{key: testAPIKeyStored}
	dir := t.TempDir()
	if err := auth.NewFileStore(dir).Save(testAPIKeyStored); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, &runOpts{args: []string{"logout"}, env: map[string]string{}, keyring: kr, configDir: dir})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if kr.key != "" {
		t.Error("logout must clear the keyring")
	}
	if _, err := auth.NewFileStore(dir).Load(); err == nil {
		t.Error("logout must clear the file store")
	}
	for _, want := range []string{"OS keyring", "credential file"} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Errorf("logout must report removing from the %s, got %q", want, res.stderr.String())
		}
	}

	res = runCLI(t, &runOpts{args: []string{"logout"}, env: map[string]string{}, keyring: kr, configDir: dir})
	if res.code != 0 {
		t.Fatalf("second logout exit = %d, want the idempotent 0", res.code)
	}
	if !strings.Contains(res.stderr.String(), "nothing to remove") {
		t.Errorf("second logout must say nothing was stored, got %q", res.stderr.String())
	}
}

// TestLogoutSurfacesReachableKeyringDeleteFailure pins the honesty contract:
// when a reachable keyring genuinely fails to delete, logout must exit
// non-zero and must not claim a clean logout — least of all "nothing to
// remove" — because the key may still sit in the keyring.
func TestLogoutSurfacesReachableKeyringDeleteFailure(t *testing.T) {
	kr := &fakeKeyring{key: testAPIKeyStored, deleteErr: errors.New("the collection refused the deletion")}
	res := runCLI(t, &runOpts{args: []string{"logout"}, env: map[string]string{}, keyring: kr})
	if res.code == 0 {
		t.Fatalf("exit = 0; a failed keyring delete must not report a clean logout (stderr: %s)", res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if strings.Contains(res.stderr.String(), "nothing to remove") {
		t.Errorf("must not claim nothing was stored while the key remains, got %q", res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "refused the deletion") {
		t.Errorf("the underlying failure must be visible, got %q", res.stderr.String())
	}
}

// TestWhoamiHermeticEnvNeverTouchesStorage extends the hermetic probe to the
// full command: with QURL_API_KEY set, whoami authenticates with the env key
// even though both backends hold a different one, and no fallback warning
// fires (storage was never read).
func TestWhoamiHermeticEnvNeverTouchesStorage(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := t.TempDir()
	if err := auth.NewFileStore(dir).Save(testAPIKeyStored); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args:      []string{"--endpoint", srv.URL, "whoami"},
		env:       map[string]string{"QURL_API_KEY": testAPIKey},
		keyring:   &fakeKeyring{unavailable: true},
		configDir: dir,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := srv.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("hermetic mode must use the env key, sent %d bytes", len(got))
	}
	if res.stderr.Len() != 0 {
		t.Errorf("hermetic mode must not warn about storage, got %q", res.stderr.String())
	}
}

// TestWhoamiUsesStoredKey covers the storage side of the precedence: no env
// key, the keyring's key authenticates.
func TestWhoamiUsesStoredKey(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "whoami"},
		env:     map[string]string{},
		keyring: &fakeKeyring{key: testAPIKeyStored},
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := srv.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testAPIKeyStored {
		t.Errorf("whoami must use the stored key, sent %d bytes", len(got))
	}
	if res.stderr.Len() != 0 {
		t.Errorf("keyring reads must not warn, got %q", res.stderr.String())
	}
}

// TestWhoamiFileFallbackWarnsOnce covers the last rung: keyring unavailable,
// key read from the credential file, exactly one stderr warning.
func TestWhoamiFileFallbackWarnsOnce(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := t.TempDir()
	if err := auth.NewFileStore(dir).Save(testAPIKeyStored); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args:      []string{"--endpoint", srv.URL, "whoami"},
		env:       map[string]string{},
		keyring:   &fakeKeyring{unavailable: true},
		configDir: dir,
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := srv.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testAPIKeyStored {
		t.Errorf("whoami must use the file-fallback key, sent %d bytes", len(got))
	}
	if got := strings.Count(res.stderr.String(), "mode 0600"); got != 1 {
		t.Errorf("file-fallback read must warn exactly once, warned %d times in %q", got, res.stderr.String())
	}
}

// TestWhoamiQuietPrintsOwnerID pins the --quiet projection: the owner id,
// alone, on stdout.
func TestWhoamiQuietPrintsOwnerID(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "whoami"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got, want := res.stdout.String(), apitest.MeOwnerID+"\n"; got != want {
		t.Errorf("quiet stdout = %q, want %q", got, want)
	}
}

// TestLoginClosedStdinIsUsageErrorNotAHang is the T4 pin: a closed stdin
// (EOF immediately) exits 2 with the needs-input message — it never blocks
// waiting for a key and never sends a request.
func TestLoginClosedStdinIsUsageErrorNotAHang(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "login"},
		env:   map[string]string{},
		stdin: strings.NewReader(""),
	})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("no key means no requests, saw %d", got)
	}
	if !strings.Contains(res.stderr.String(), "no API key provided") {
		t.Errorf("expected the needs-input message, got %q", res.stderr.String())
	}
}
