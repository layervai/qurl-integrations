package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// TestLoginValidatesAndEnrolls pins the one-time account-key boundary: the
// just-typed key is validated, the machine is enrolled, and the account key
// is not stored by the CLI.
func TestLoginValidatesAndEnrolls(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "login"},
		env:   map[string]string{},
		stdin: strings.NewReader(testAPIKey + "\n"),
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
	for _, want := range []string{apitest.MeOwnerID, "consumed, not stored"} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Errorf("login confirmation must mention %q, got %q", want, res.stderr.String())
		}
	}
}

func TestLoginRejectedKeyIsNotEnrolled(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "login"},
		env:   map[string]string{},
		stdin: strings.NewReader(testAPIKey + "\n"),
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

func TestWhoamiUsesExplicitEnvironmentBootstrap(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "whoami"},
		env:  map[string]string{"QURL_API_KEY": testAPIKey},
	})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if got := srv.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("explicit bootstrap must use the environment key, sent %d bytes", len(got))
	}
	if res.stderr.Len() != 0 {
		t.Errorf("environment bootstrap must not warn about storage, got %q", res.stderr.String())
	}
}

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
