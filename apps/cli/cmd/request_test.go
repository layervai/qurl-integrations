package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "POST", "/v1/account/link", "--supervision", "external", "-o", "json", "--idempotency-key", "01234567-89ab-cdef-0123-456789abcdef"}, stdin: strings.NewReader(`{"account_token":"private-token"}`), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
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
		{[]string{"GET", "/v1/me", "--idempotency-key", strings.Repeat("a", 32)}, "", "applies only to mutations"},
		{[]string{"POST", "/v1/resources", "--idempotency-key", strings.Repeat("a", 31)}, "", "idempotency key"},
		{[]string{"POST", "/v1/resources", "--idempotency-key", strings.Repeat("a", 257)}, "", "idempotency key"},
		{[]string{"HEAD", "/v1/me"}, "", "request method must be"},
		{[]string{"get", "/v1/me"}, "", "request method must be"},
		{[]string{"GET", "/v1/me?x=1"}, "", "queries are allowed only"},
		{[]string{"POST", "/v1/api-keys"}, "{}", "not a registered-device route"},
	} {
		res := runCLI(t, &runOpts{env: externalLoginEnv(), args: append([]string{"request", "--supervision", "external", "-o", "json"}, tc.args...), stdin: strings.NewReader(tc.body), openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("opened device for invalid input")
			return nil, errors.New("unexpected device open")
		}})
		if res.code == 0 || res.stdout.Len() != 0 || !strings.Contains(res.stderr.String(), tc.want) || strings.Contains(res.stderr.String(), "private-token") {
			t.Fatalf("invalid input exit %d: %s", res.code, res.stderr.String())
		}
	}
}

func TestRequestRequiresJSONOutput(t *testing.T) {
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "GET", "/v1/me"}, stdin: strings.NewReader(""), openAPIClient: func(context.Context) (qurlapi.Client, error) {
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
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "POST", "/v1/resources", "--supervision", "external", "-o", "json"}, stdin: failingReader{}, openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device after stdin failure")
		return nil, errors.New("unexpected device open")
	}})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "could not read request body: stdin is closed") {
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
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "POST", "/v1/resources", "--supervision", "external", "-o", "json"}, stdin: strings.NewReader(body), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
		return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
	}})
	if len(body) != 1<<20 || res.code != 0 || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestRequestRequiresExternalSupervision(t *testing.T) {
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "GET", "/v1/me", "-o", "json"}, stdin: strings.NewReader(""), openAPIClient: func(context.Context) (qurlapi.Client, error) {
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
	res := runCLI(t, &runOpts{env: externalLoginEnv(), args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json", "--quiet"}, stdin: failingReader{}, openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
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
	for _, route := range [][]string{{"POST", "/v1/resources"}, {"PUT", "/v1/resources/r_1/sharing"}, {"PATCH", "/v1/resources/r_1"}} {
		res := runCLI(t, &runOpts{env: externalLoginEnv(), args: append([]string{"request", "--supervision", "external", "-o", "json"}, route...), inTTY: true, openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("opened device with a terminal body")
			return nil, errors.New("unexpected device open")
		}})
		if res.code != 2 || !strings.Contains(res.stderr.String(), "redirect it from the null device") {
			t.Fatalf("%v: exit %d: %s", route, res.code, res.stderr.String())
		}
	}
}

// A namespace whose anonymous enrollment failed keeps its external mark but
// has no device; request must point back at login, not at an account key.
func TestRequestExplainsUnenrolledExternalNamespace(t *testing.T) {
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		_, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent", PublicKeyB64: "key"})
		return nil, err
	}})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "enroll this state directory first") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestRequestRefusesNativeNamespace(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: stateDir, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openNativeRuntime: refuseNativeRuntime(t)})
	if res.code != 3 || !strings.Contains(res.stderr.String(), `runtime supervision is "native", not "external"`) {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	mustNoExternalPolicy(t, stateDir)
}

// A body-less mutation (stdin from the null device) reaches the service with
// no body and no Content-Type, and a DELETE keeps its idempotency key.
func TestRequestBodylessMutations(t *testing.T) {
	srv := apitest.NewServer(t)
	key := strings.Repeat("k", 32)
	srv.Script(http.MethodPost, "/v1/resources/r_1/sharing/restart", func(w http.ResponseWriter, r *http.Request) {
		if got, _ := io.ReadAll(r.Body); len(got) != 0 || r.Header.Get("Content-Type") != "" {
			t.Errorf("body-less POST sent %q with Content-Type %q", got, r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusAccepted)
	})
	srv.Script(http.MethodDelete, "/v1/resources/r_1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != key {
			t.Error("DELETE lost its idempotency key")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	state := bootstrapRegisteredState(t)
	for _, args := range [][]string{
		{"POST", "/v1/resources/r_1/sharing/restart"},
		{"DELETE", "/v1/resources/r_1/sessions", "--idempotency-key", key},
	} {
		res := runCLI(t, &runOpts{env: externalLoginEnv(), args: append([]string{"request", "--supervision", "external", "-o", "json"}, args...), stdin: strings.NewReader(""), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
			return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
		}})
		if res.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, res.code, res.stderr.String())
		}
	}
	if len(srv.Requests()) != 2 {
		t.Fatalf("requests = %d, want 2", len(srv.Requests()))
	}
}

// An absent supervised device fails without starting enrollment or recovery.
func TestRequestMissingStateDoesNotEnrollOrRecover(t *testing.T) {
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Fatal("raw request attempted native runtime enrollment or recovery")
		return nil, errors.New("unexpected runtime open")
	}})
	if res.code != 4 || !strings.Contains(res.stderr.String(), "no enrolled device") || strings.Contains(res.stderr.String(), "qurl account recover") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestRequestRequiresSealedKeyProvider(t *testing.T) {
	res := runCLI(t, &runOpts{env: map[string]string{}, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openAPIClient: func(context.Context) (qurlapi.Client, error) {
		t.Error("opened device without the sealed key provider")
		return nil, errors.New("unexpected device open")
	}})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "request requires LAYERV_KEY_PROVIDER") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

// Connector-scoped devices cannot use the raw owner-scoped resource surface.
func TestRequestRejectsConnectorScopedDevice(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	state := bootstrapRegisteredState(t)
	state.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindConnectorBootstrap)
	saveDeviceState(t, dir, state)
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/resources", "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Fatal("scoped raw request opened native lifecycle")
		return nil, errors.New("unexpected runtime open")
	}})
	if res.code != exitcode.Auth || res.stdout.Len() != 0 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
	for _, req := range srv.Requests() {
		if req.Path == "/v1/resources" {
			t.Fatal("forwarded the request for a connector-scoped device")
		}
	}
}

func TestRequestEnrolledExternalNamespaceReturnsEnvelope(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	saveDeviceState(t, dir, bootstrapRegisteredState(t))
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Fatal("raw request opened native runtime")
		return nil, errors.New("unexpected runtime open")
	}})
	var envelope struct {
		Status int `json:"status"`
	}
	if res.code != 0 || json.Unmarshal(res.stdout.Bytes(), &envelope) != nil || envelope.Status != http.StatusOK {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
	if got := len(srv.Requests()); got != 1 {
		t.Fatalf("supervisor target request count = %d, want 1", got)
	}
}

// TestReadRequestBodyYieldsToCancellation pins that a stalled stdin cannot
// outlive a supervisor's SIGTERM: Main traps the signal into ctx, so the read
// must return when ctx ends.
func TestReadRequestBodyYieldsToCancellation(t *testing.T) {
	stalled, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readRequestBody(ctx, &output.Streams{In: stalled}, http.MethodPost)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdin read ignored cancellation")
	}
}

func TestRequestEnrolledRateLimitPreservesSingleResponse(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodGet, "/v1/me", 3, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limit_exceeded", "Too many requests", "bounded fixture")
	})
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	saveDeviceState(t, dir, bootstrapRegisteredState(t))
	res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Fatal("raw request opened native runtime")
		return nil, errors.New("unexpected runtime open")
	}})
	var envelope struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	}
	if res.code != 0 || json.Unmarshal(res.stdout.Bytes(), &envelope) != nil || envelope.Status != 429 || envelope.Headers["retry-after"] != "1" || len(srv.Requests()) != 1 {
		t.Fatalf("request contract: exit=%d status=%d calls=%d, want exit0 status429 one call", res.code, envelope.Status, len(srv.Requests()))
	}
}

func TestRequestExistingDevicePassesServerAuthorityWithoutLocalWrites(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := apitest.NewServer(t)
			target := "/v1/resources/r_foreign/sharing"
			srv.Script(http.MethodPut, target, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"result":"fixture"}`))
			})
			dir := connectorStateTestDir(t)
			if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			saveDeviceState(t, dir, bootstrapRegisteredState(t))
			registry, err := connectorstate.OpenLocalShareRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.BindOwner(context.Background(), "different-local-share-owner"); err != nil {
				t.Fatal(err)
			}
			paths := []string{connectorstate.AgentStateFile, connectorstate.LocalSharesFile}
			before := make(map[string]string)
			for _, name := range paths {
				b, err := readRequestFixture(t, dir, name)
				if err != nil {
					t.Fatal(err)
				}
				before[name] = string(b)
			}
			res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "PUT", target, "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, stdin: strings.NewReader(`{"desired_state":"off"}`), openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				t.Fatal("request opened native lifecycle")
				return nil, errors.New("unexpected")
			}})
			var envelope struct {
				Status  int               `json:"status"`
				Headers map[string]string `json:"headers"`
			}
			if res.code != 0 || json.Unmarshal(res.stdout.Bytes(), &envelope) != nil || envelope.Status != status || len(srv.Requests()) != 1 || srv.Requests()[0].Path != target {
				t.Fatalf("exit=%d status=%d calls=%d", res.code, envelope.Status, len(srv.Requests()))
			}
			if status == 429 && envelope.Headers["retry-after"] != "7" {
				t.Fatal("lost Retry-After")
			}
			for _, name := range paths {
				after, err := readRequestFixture(t, dir, name)
				if err != nil || string(after) != before[name] {
					t.Fatal("raw request changed local state")
				}
			}
		})
	}
}

func TestRequestRejectsInvalidPersistedDevice(t *testing.T) {
	for _, mode := range []string{"scope", "incomplete", "wrong-agent", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			srv := apitest.NewServer(t)
			dir := connectorStateTestDir(t)
			if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			state := bootstrapRegisteredState(t)
			if mode == "scope" {
				state.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindConnectorBootstrap)
			}
			if mode == "incomplete" {
				state.RegisteredAt = nil
			}
			saveDeviceState(t, dir, state)
			if mode == "wrong-agent" {
				t.Setenv(connectorstate.EnvAgentID, "another-agent")
			}
			if mode == "corrupt" {
				if err := os.WriteFile(filepath.Join(dir, connectorstate.AgentStateFile), []byte("invalid-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/resources", "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				t.Fatal("invalid raw request opened native lifecycle")
				return nil, errors.New("unexpected")
			}})
			if res.code == 0 || len(srv.Requests()) != 0 {
				t.Fatalf("invalid device mode=%s exit=%d calls=%d", mode, res.code, len(srv.Requests()))
			}
		})
	}
}

func TestRequestAbsentEnvelopeDoesNotCreateState(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(strconv.FormatBool(marked), func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "absent-device")
			if marked {
				if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadDir(dir)
			res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				t.Fatal("missing device opened lifecycle")
				return nil, errors.New("unexpected")
			}})
			after, err := os.ReadDir(dir)
			if res.code == 0 || len(after) != len(before) {
				t.Fatal("missing envelope was accepted or created state")
			}
			if !marked && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("request created absent namespace")
			}
		})
	}
}

func TestRequestPersistedRecoveryNeedsExplicitSupervisorAction(t *testing.T) {
	for _, mode := range []string{"missing-credential", "retired-state", "refresh-required"} {
		t.Run(mode, func(t *testing.T) {
			srv := apitest.NewServer(t)
			dir := connectorStateTestDir(t)
			if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			state := bootstrapRegisteredState(t)
			switch mode {
			case "missing-credential":
				state.DeviceAPIKey = ""
			case "retired-state":
				state.Assignment = nil
				state.DeviceAPIKeyID = ""
			case "refresh-required":
				state.SchemaVersion = 8
				state.CredentialRecoveryRefreshRequired = true
			}
			saveDeviceState(t, dir, state)
			before, err := readRequestFixture(t, dir, connectorstate.AgentStateFile)
			if err != nil {
				t.Fatal(err)
			}
			res := runCLI(t, &runOpts{nativeClient: true, env: externalLoginEnv(), shareStateDir: dir, args: []string{"request", "GET", "/v1/me", "--endpoint", srv.URL, "--supervision", "external", "-o", "json"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				t.Fatal("recovery state opened native lifecycle")
				return nil, errors.New("unexpected")
			}})
			expected := exitcode.Auth
			if mode == "refresh-required" {
				expected = exitcode.Unavailable
			}
			if res.code != expected || len(srv.Requests()) != 0 {
				t.Fatalf("mode=%s exit=%d want=%d calls=%d", mode, res.code, expected, len(srv.Requests()))
			}
			if mode != "refresh-required" && !strings.Contains(res.stderr.String(), "keep this state directory intact") {
				t.Fatal("recovery lost supervisor remedy")
			}
			if mode == "refresh-required" && strings.Contains(res.stderr.String(), "enroll a new") {
				t.Fatal("promoted credential incorrectly suggested reenrollment")
			}
			after, err := readRequestFixture(t, dir, connectorstate.AgentStateFile)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("recovery check changed state")
			}
		})
	}
}

func readRequestFixture(t *testing.T, dir, name string) ([]byte, error) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	return root.ReadFile(name)
}
