//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const testExternalEnrollmentToken = "external-enrollment-token-do-not-disclose"

// writeExternalLoginToken writes the supervisor's one-shot token the way a
// supervising app does: one owner-only private file holding the token and a
// trailing newline.
func writeExternalLoginToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(path, []byte(testExternalEnrollmentToken+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

// staticAgentStateStore hands back one exact AgentState, including the
// incomplete shapes the copying bootstrap fixture cannot represent.
type staticAgentStateStore struct{ state *qurl.AgentState }

func (s *staticAgentStateStore) LoadAgentState(context.Context) (*qurl.AgentState, error) {
	return s.state, nil
}

func (*staticAgentStateStore) SaveAgentState(context.Context, *qurl.AgentState) error {
	return errors.New("external login test store unexpectedly saved state")
}

func mustNotLeakEnrollmentToken(t *testing.T, srv *apitest.Server, res *runResult) {
	t.Helper()
	if strings.Contains(res.stdout.String(), testExternalEnrollmentToken) || strings.Contains(res.stderr.String(), testExternalEnrollmentToken) {
		t.Fatal("the enrollment token reached command output")
	}
	for _, request := range srv.Requests() {
		if strings.Contains(request.Header.Get("Authorization"), testExternalEnrollmentToken) {
			t.Fatal("the enrollment token reached the REST surface")
		}
	}
}

// TestExternalLoginEnrollsFreshNamespaceFromTheTokenFile pins the cold path:
// the policy marker is committed before the runtime can write anything, the
// token is read exactly once through the runtime's enrollment provider and is
// not needed afterwards, the owner is bound, and no plaintext envelope is
// written by the CLI.
func TestExternalLoginEnrollsFreshNamespaceFromTheTokenFile(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := filepath.Join(t.TempDir(), "profile", "state")
	tokenPath := writeExternalLoginToken(t)
	state := bootstrapRegisteredState(t)
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}
	providerCalls := 0
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "login", "--enrollment-token-file", tokenPath},
		env:           externalLoginEnv(connectorstate.EnvRuntimeSupervision, string(connectorstate.RuntimeSupervisionExternal)),
		shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if err := connectorstate.RequireRuntimeSupervision(stateDir, connectorstate.RuntimeSupervisionExternal); err != nil {
				t.Fatalf("runtime opened before the external policy was established: %v", err)
			}
			if cfg.StateDir != stateDir || cfg.ClientBaseURL != srv.URL || cfg.AgentID != connectorstate.ConfiguredAgentID() ||
				cfg.Hostname == "" || cfg.Version != "test" || cfg.RefreshMode != connectorRefreshModeAuto ||
				cfg.EnrollmentCredential != "" || cfg.EnrollmentCredentialProvider == nil || cfg.RecoveryCredentialProvider != nil {
				t.Fatalf("external native runtime config = %+v", cfg)
			}
			providerCalls++
			credential, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID})
			if err != nil || credential != testExternalEnrollmentToken {
				t.Fatalf("enrollment provider = (%d bytes, %v)", len(credential), err)
			}
			// The supervisor deletes the one-shot file once it is consumed;
			// nothing after enrollment may need it.
			if err := os.Remove(tokenPath); err != nil {
				t.Fatal(err)
			}
			return runtime, nil
		},
	})
	if res.code != 0 {
		t.Fatalf("external login = exit %d stderr %q", res.code, res.stderr.String())
	}
	if providerCalls != 1 {
		t.Fatalf("enrollment provider calls = %d, want exactly one", providerCalls)
	}
	if mode, err := connectorstate.ReadRuntimeSupervision(stateDir); err != nil || mode != connectorstate.RuntimeSupervisionExternal {
		t.Fatalf("namespace supervision = %q, %v; want external", mode, err)
	}
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if owner, bound, err := registry.OwnerID(context.Background()); err != nil || !bound || owner != apitest.MeOwnerID {
		t.Fatalf("registry owner = (%q, %t, %v), want %q bound", owner, bound, err, apitest.MeOwnerID)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external login wrote a plaintext agent state envelope: %v", err)
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/v1/me" ||
		requests[0].Header.Get("Authorization") != "Bearer "+state.DeviceAPIKey {
		t.Fatalf("external login requests = %+v, want one device-authenticated GET /v1/me", requests)
	}
	mustNotLeakEnrollmentToken(t, srv, res)
	if !strings.Contains(res.stderr.String(), apitest.MeOwnerID) {
		t.Fatalf("login confirmation = %q, want the owner id", res.stderr.String())
	}
	if !runtime.closed {
		t.Fatal("completed external login left the native runtime open")
	}
}

// TestExternalLoginWarmRunNeverReadsTheTokenFile pins the warm path: an
// already enrolled external namespace is opened without the token file
// existing, and neither the policy marker nor the owner binding is rewritten.
func TestExternalLoginWarmRunNeverReadsTheTokenFile(t *testing.T) {
	srv := apitest.NewServer(t)
	ctx := context.Background()
	stateDir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(ctx, stateDir); err != nil {
		t.Fatal(err)
	}
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BindOwner(ctx, apitest.MeOwnerID); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(stateDir, connectorstate.RuntimeModeFile)
	markerBefore, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	sharesPath := filepath.Join(stateDir, connectorstate.LocalSharesFile)
	sharesBefore, err := os.ReadFile(sharesPath) // #nosec G304 -- test-owned state directory.
	if err != nil {
		t.Fatal(err)
	}
	state := bootstrapRegisteredState(t)
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", filepath.Join(t.TempDir(), "absent-token")},
		env:           externalLoginEnv(),
		shareStateDir: stateDir,
		openNativeRuntime: func(_ context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if cfg.EnrollmentCredentialProvider == nil {
				t.Fatal("warm open lost the lazy enrollment provider")
			}
			// A warm runtime has a complete identity and never asks for the
			// credential.
			return runtime, nil
		},
	})
	if res.code != 0 {
		t.Fatalf("warm external login = exit %d stderr %q", res.code, res.stderr.String())
	}
	if markerAfter, err := os.Stat(markerPath); err != nil || !os.SameFile(markerBefore, markerAfter) {
		t.Fatalf("warm login replaced the immutable policy marker: %v", err)
	}
	if sharesAfter, err := os.ReadFile(sharesPath); err != nil || !bytes.Equal(sharesAfter, sharesBefore) { // #nosec G304 -- test-owned state directory.
		t.Fatalf("warm login rewrote the owner binding: %v", err)
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Authorization") != "Bearer "+state.DeviceAPIKey {
		t.Fatalf("warm external login requests = %+v, want one device-authenticated request", requests)
	}
}

// TestExternalLoginRejectsNonOwnerScopedState pins the post-open check: a
// device whose enrollment is not the owner-scoped agent kind, or whose
// registration is incomplete, is an authentication failure that never binds
// an owner, never reaches the service, and leaves the namespace as it is for
// the supervisor to rotate.
func TestExternalLoginRejectsNonOwnerScopedState(t *testing.T) {
	cases := map[string]func(*qurl.AgentState){
		"connector_bootstrap": func(s *qurl.AgentState) {
			s.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindConnectorBootstrap)
		},
		"agent":                func(s *qurl.AgentState) { s.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindAgent) },
		"account":              func(s *qurl.AgentState) { s.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindAccount) },
		"unknown kind":         func(s *qurl.AgentState) { s.EnrollmentCredentialKind = "" },
		"no device credential": func(s *qurl.AgentState) { s.DeviceAPIKey = "" },
		"unregistered":         func(s *qurl.AgentState) { s.RegisteredAt = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			state := bootstrapRegisteredState(t)
			mutate(state)
			runtime := &bootstrapNativeRuntime{store: &staticAgentStateStore{state: state}}
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", writeExternalLoginToken(t)},
				env:           externalLoginEnv(),
				shareStateDir: stateDir,
				openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
					if _, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID}); err != nil {
						t.Fatal(err)
					}
					return runtime, nil
				},
			})
			if res.code != 4 {
				t.Fatalf("exit = %d stderr = %q, want the authentication exit code", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			mustNotLeakEnrollmentToken(t, srv, res)
			if requests := srv.Requests(); len(requests) != 0 {
				t.Fatalf("rejected device state reached the service: %+v", requests)
			}
			if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.LocalSharesFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected device state bound an owner: %v", err)
			}
			if err := connectorstate.RequireRuntimeSupervision(stateDir, connectorstate.RuntimeSupervisionExternal); err != nil {
				t.Fatalf("rejected device state was not left in place for the supervisor: %v", err)
			}
			if !runtime.closed {
				t.Fatal("rejected device state left the native runtime open")
			}
		})
	}
}

func TestExternalLoginEnrollmentFailureLeavesAResumableNamespaceAndRedactsTheToken(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", writeExternalLoginToken(t)},
		env:           externalLoginEnv(),
		shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			credential, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent-durable-01"})
			if err != nil || credential != testExternalEnrollmentToken {
				t.Fatalf("enrollment provider = (%d bytes, %v)", len(credential), err)
			}
			return nil, errors.New("enrollment credential rejected")
		},
	})
	if res.code == 0 {
		t.Fatal("failed enrollment reported success")
	}
	mustEmptyStdout(t, res)
	mustNotLeakEnrollmentToken(t, srv, res)
	if err := connectorstate.RequireRuntimeSupervision(stateDir, connectorstate.RuntimeSupervisionExternal); err != nil {
		t.Fatalf("failed enrollment lost the resumable policy marker: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.LocalSharesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed enrollment bound an owner: %v", err)
	}
}

func TestExternalLoginJSONPrintsTheDeviceIdentityOnly(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)}}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "-o", "json", "login", "--enrollment-token-file", writeExternalLoginToken(t), "--supervision", "external"},
		env:           externalLoginEnv(),
		shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if _, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent-durable-01"}); err != nil {
				t.Fatal(err)
			}
			return runtime, nil
		},
	})
	if res.code != 0 {
		t.Fatalf("external login -o json = exit %d stderr %q", res.code, res.stderr.String())
	}
	want := "{\n  \"owner_id\": \"" + apitest.MeOwnerID + "\",\n  \"auth_type\": \"api_key\",\n  \"device_key_id\": \"" + apitest.MeKeyID + "\",\n  \"device_enrolled\": true\n}\n"
	if got := res.stdout.String(); got != want {
		t.Fatalf("external login JSON = %q, want %q", got, want)
	}
	if res.stderr.Len() != 0 {
		t.Fatalf("external login -o json wrote to stderr: %q", res.stderr.String())
	}
	mustNotLeakEnrollmentToken(t, srv, res)
}
