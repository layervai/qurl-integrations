package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRequestCommand(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/account/link", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["account_token"] != "private-token" {
			t.Error("stdin body lost")
		}
		if r.Header.Get("Idempotency-Key") != "01234567-89ab-cdef-0123-456789abcdef" {
			t.Error("idempotency key lost")
		}
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
	})
	state := bootstrapRegisteredState(t)
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "POST", "/v1/account/link", "--supervision", "external", "-o", "json", "--idempotency-key", "01234567-89ab-cdef-0123-456789abcdef"}, stdin: strings.NewReader(`{"account_token":"private-token"}`), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
		return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
	}})
	var envelope struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(res.stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if res.code != 0 || envelope.Status != 429 || envelope.Headers["retry-after"] != "3" || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
	for _, secret := range []string{state.DeviceAPIKey, "private-token"} {
		if strings.Contains(res.stdout.String()+res.stderr.String(), secret) {
			t.Fatal("credential leaked")
		}
	}
}

func TestRequestRejectsInputBeforeOpeningDevice(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		body, want string
	}{
		{[]string{"GET", "https://evil.test/v1/me"}, "", "canonical /v1/ path"},
		{[]string{"POST", "/v1/account/link"}, `{"account_token":"private-token"} garbage`, "request body must be JSON"},
		{[]string{"POST", "/v1/account/link"}, "{}" + strings.Repeat(" ", qurlapi.MaxRequestBody-1), "exceeds 1 MiB"},
		{[]string{"GET", "/v1/me", "--idempotency-key", strings.Repeat("a", 32) + "\r\nheader"}, "", "idempotency key"},
		{[]string{"POST", "/v1/resources", "--idempotency-key", strings.Repeat("a", 31)}, "", "idempotency key"},
		{[]string{"POST", "/v1/resources", "--idempotency-key", strings.Repeat("a", 257)}, "", "idempotency key"},
		{[]string{"HEAD", "/v1/me"}, "", "request method must be"},
		{[]string{"get", "/v1/me"}, "", "request method must be"},
		{[]string{"GET", "/v1/me?x=1"}, "", "queries are allowed only"},
	} {
		res := runCLI(t, &runOpts{env: map[string]string{}, args: append([]string{"request", "--supervision", "external", "-o", "json"}, tc.args...), stdin: strings.NewReader(tc.body), openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("opened device for invalid input")
			return nil, errors.New("unexpected device open")
		}})
		if res.code == 0 || res.stdout.Len() != 0 || !strings.Contains(res.stderr.String(), tc.want) || strings.Contains(res.stderr.String(), "private-token") {
			t.Fatalf("invalid input exit %d: %s", res.code, res.stderr.String())
		}
	}
}

func TestRequestRequiresJSONOutput(t *testing.T) {
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "GET", "/v1/me"}, stdin: strings.NewReader(""), openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device without JSON output")
		return nil, errors.New("unexpected device open")
	}})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "request requires --output json") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stdin is closed") }

func TestRequestReportsStdinReadError(t *testing.T) {
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "POST", "/v1/resources", "--supervision", "external", "-o", "json"}, stdin: failingReader{}, openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device after stdin failure")
		return nil, errors.New("unexpected device open")
	}})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "could not read request body: stdin is closed") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestRequestAcceptsOneMiBBody(t *testing.T) {
	srv := apitest.NewServer(t)
	body := `{"x":"` + strings.Repeat("a", (1<<20)-8) + `"}`
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if got, _ := io.ReadAll(r.Body); string(got) != body {
			t.Errorf("request body arrived with %d bytes, want %d", len(got), len(body))
		}
		w.WriteHeader(http.StatusCreated)
	})
	state := bootstrapRegisteredState(t)
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "POST", "/v1/resources", "--supervision", "external", "-o", "json"}, stdin: strings.NewReader(body), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
		return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
	}})
	if len(body) != 1<<20 || res.code != 0 || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestRequestAcceptsBoundaryIdempotencyKeys(t *testing.T) {
	for _, key := range []string{strings.Repeat("a", 32), strings.Repeat("a", 256)} {
		if err := qurlapi.ValidateRequestIdempotencyKey(key); err != nil {
			t.Fatalf("rejected %d-byte key: %v", len(key), err)
		}
	}
}

func TestRequestRequiresExternalSupervision(t *testing.T) {
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "GET", "/v1/me", "-o", "json"}, stdin: strings.NewReader(""), openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device in a native namespace")
		return nil, errors.New("unexpected device open")
	}})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "request requires --supervision external") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

// A GET never reads stdin, so an inherited pipe that never reaches EOF (here
// one that fails if read) cannot stall it.
func TestRequestGETReturnsEnvelopeWithoutReadingStdin(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner_id":"owner-a&b"}`))
	})
	state := bootstrapRegisteredState(t)
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, stdin: failingReader{}, openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
		return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
	}})
	var envelope struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    struct {
			OwnerID string `json:"owner_id"`
		} `json:"body"`
	}
	if res.code != 0 || json.Unmarshal(res.stdout.Bytes(), &envelope) != nil || envelope.Status != http.StatusOK ||
		envelope.Headers["content-type"] != "application/json" || envelope.Body.OwnerID != "owner-a&b" || strings.Contains(res.stdout.String(), `\u0026`) {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
}

func TestRequestRefusesAccountKeyConfiguration(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "QURL_API_KEY_FILE"} {
		res := runCLI(t, &runOpts{args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, env: map[string]string{name: "credential-do-not-read"}, openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("opened device with account authority configured")
			return nil, errors.New("unexpected device open")
		}})
		if res.code != 2 || !strings.Contains(res.stderr.String(), "request cannot be combined with") || strings.Contains(res.stderr.String(), "credential-do-not-read") {
			t.Fatalf("%s: exit %d: %s", name, res.code, res.stderr.String())
		}
	}
}

func TestRequestRefusesTerminalBodyInput(t *testing.T) {
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "POST", "/v1/resources", "--supervision", "external", "-o", "json"}, inTTY: true, openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device with a terminal body")
		return nil, errors.New("unexpected device open")
	}})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "redirect it from the null device") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

// A namespace whose anonymous enrollment failed keeps its external mark but
// has no device; request must point back at login, not at an account key.
func TestRequestExplainsUnenrolledExternalNamespace(t *testing.T) {
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		_, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent", PublicKeyB64: "key"})
		return nil, err
	}})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "run `qurl login --anonymous --supervision external`") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}
