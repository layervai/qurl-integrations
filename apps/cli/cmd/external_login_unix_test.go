//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/unix"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
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
	// This is a negative-space check, not sealing coverage: the harness injects
	// the environment through opts.lookupEnv, while state.Open and
	// SealedProviderSelected read the process environment so they agree with
	// what qurl-connector will see. The sealed branch is therefore never taken
	// here and the fake runtime writes no state at all. Real sealing is covered
	// by the state package's t.Setenv tests.
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

// TestOneShotEnrollmentTokenReplaysItsFirstOutcome pins that the credential
// provider reads the supervisor's file exactly once. The file is one-shot and
// the supervisor deletes it on every exit path, so a second call must replay
// the cached token rather than re-open a path that is gone and report a
// file-shape error. Letting the platform reject a genuinely spent credential
// keeps a transient enrollment failure retryable.
func TestOneShotEnrollmentTokenReplaysItsFirstOutcome(t *testing.T) {
	path := writeExternalLoginToken(t)
	provider := oneShotEnrollmentToken(path)
	token, err := provider(context.Background(), qurl.AgentEnrollmentCredentialRequest{})
	if err != nil || token != testExternalEnrollmentToken {
		t.Fatalf("first read = %q, %v; want the token", token, err)
	}
	// The supervisor deletes the consumed file, as the enrollment test models.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	again, err := provider(context.Background(), qurl.AgentEnrollmentCredentialRequest{})
	if err != nil || again != testExternalEnrollmentToken {
		t.Fatalf("second read = %q, %v; want the cached token replayed", again, err)
	}
}

// TestOneShotEnrollmentTokenReplaysItsFirstFailure pins the other half: a
// first read that failed is replayed verbatim, so the caller sees why the
// token was rejected rather than a different error on every retry.
func TestOneShotEnrollmentTokenReplaysItsFirstFailure(t *testing.T) {
	provider := oneShotEnrollmentToken(filepath.Join(t.TempDir(), "absent"))
	_, first := provider(context.Background(), qurl.AgentEnrollmentCredentialRequest{})
	if first == nil {
		t.Fatal("reading an absent token file succeeded")
	}
	if _, second := provider(context.Background(), qurl.AgentEnrollmentCredentialRequest{}); second == nil || second.Error() != first.Error() {
		t.Fatalf("second failure = %v, want the first one replayed (%v)", second, first)
	}
}

// Exercise the real Connector runtime and sealed store on the warm path. Cold
// enrollment is a separate network journey; this test seeds a completed device.
func TestExternalLoginWarmRealRuntimeOpensSealedStateWithoutToken(t *testing.T) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(fds[1], bytes.Repeat([]byte{0x7a}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fds[1]); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(fds[0]), unix.F_DUPFD_CLOEXEC, 512)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fds[0]); err != nil {
		t.Fatal(err)
	}
	// Connector reads and closes this inherited descriptor, then caches the key
	// for repeated store opens in this process. Do not close the reused fd here.
	env := externalLoginEnv(connectoragentstate.EnvLocalKeyFD, strconv.Itoa(fd))
	for key, value := range env {
		t.Setenv(key, value)
	}
	srv := apitest.NewServer(t)
	stateDir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := connectorstate.EstablishExternalRuntimeMode(ctx, stateDir); err != nil {
		t.Fatal(err)
	}
	store, err := connectoragentstate.NewSDKStore(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	state := bootstrapRegisteredState(t)
	sdk, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.SaveAgentState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", filepath.Join(stateDir, "absent-token")},
		env:  env, shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return connectorshare.OpenNativeRuntime(ctx, cfg)
		},
	})
	if res.code != 0 {
		t.Fatalf("real warm login exit=%d stderr=%s", res.code, res.stderr.String())
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Authorization") != "Bearer "+state.DeviceAPIKey {
		t.Fatal("warm login did not use the saved device credential")
	}
	if _, err := os.Stat(filepath.Join(stateDir, connectoragentstate.AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext agent state exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, connectoragentstate.SealedAgentStateFile)); err != nil {
		t.Fatal(err)
	}
}

// TestExternalLoginUnusableTokenFileExitsAuth pins the exit code for a token
// file that only fails at read time. The path-shape checks run before the
// namespace exists and are usage errors (exit 2); a wrong mode is raised
// inside the runtime open, after the namespace is labeled, so the remedy is a
// newly minted token rather than a retyped command. Without the token
// reader's own sentinel this lands on the caller's envelope fallback and
// reports exit 3 with an "agent state envelope" prefix, whose documented
// remedy is the environment or another state directory - all wrong here.
//
// The injected runtime calls the credential provider the way the real one
// does, so the assertion covers the whole chain rather than the wrap alone.
func TestExternalLoginUnusableTokenFileExitsAuth(t *testing.T) {
	srv := apitest.NewServer(t)
	path := filepath.Join(t.TempDir(), "enrollment-token")
	// A plain umask slip: readable by the group and the world.
	if err := os.WriteFile(path, []byte(testExternalEnrollmentToken+"\n"), 0o644); err != nil { // #nosec G306 -- the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- keep the unsafe fixture independent of the process umask.
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", path},
		env:           externalLoginEnv(),
		shareStateDir: filepath.Join(t.TempDir(), "state"),
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			_, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{})
			return nil, err
		},
	})
	if res.code != 4 {
		t.Fatalf("unusable token file exit = %d stderr = %q, want 4 (Auth)", res.code, res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), "agent state envelope") {
		t.Fatalf("token-file failure reported as an envelope problem: %q", res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), testExternalEnrollmentToken) {
		t.Fatal("rejected token file echoed the token")
	}
}

// TestExternalLoginAcceptsBothOwnerScopedKinds pins that this gate agrees
// with validateSandboxDeviceIdentity on what "owner-scoped" means. Both
// account and bootstrap are owner-scoped; only the connector-scoped kinds are
// refused by native session operations. Narrowing this to bootstrap alone
// would tell a supervisor to move aside a state directory whose device is
// perfectly usable, which the operator cannot undo.
func TestExternalLoginAcceptsBothOwnerScopedKinds(t *testing.T) {
	for _, kind := range []qurl.RegistrationKeyKind{
		qurl.RegistrationKeyKindAccount,
		qurl.RegistrationKeyKindBootstrap,
	} {
		t.Run(string(kind), func(t *testing.T) {
			state := bootstrapRegisteredState(t)
			state.EnrollmentCredentialKind = string(kind)
			if err := requireExternalOwnerScopedAgentState(context.Background(), &staticAgentStateStore{state: state}); err != nil {
				t.Fatalf("owner-scoped kind %q rejected: %v", kind, err)
			}
		})
	}
	for _, kind := range []qurl.RegistrationKeyKind{
		qurl.RegistrationKeyKindConnectorBootstrap,
		qurl.RegistrationKeyKindAgent,
	} {
		t.Run(string(kind), func(t *testing.T) {
			state := bootstrapRegisteredState(t)
			state.EnrollmentCredentialKind = string(kind)
			err := requireExternalOwnerScopedAgentState(context.Background(), &staticAgentStateStore{state: state})
			if !errors.Is(err, auth.ErrDeviceEnrollmentScope) {
				t.Fatalf("connector-scoped kind %q accepted: %v", kind, err)
			}
		})
	}
}
