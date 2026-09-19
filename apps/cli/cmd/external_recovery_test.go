package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func TestExternalRecoveryRefusesMissingStateBeforeOpeningRuntime(t *testing.T) {
	dir := filepath.Join(connectorStateTestDir(t), "absent")
	called := false
	res := runCLI(t, &runOpts{args: []string{"login", "--recovery-token-file", filepath.Join(dir, "token"), "--supervision", "external"}, env: externalLoginEnv(), shareStateDir: dir,
		openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			called = true
			return nil, errors.New("unexpected open")
		}})
	if res.code == 0 || called {
		t.Fatal("recovery opened an absent namespace")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("recovery created namespace")
	}
}

func TestExternalRecoveryRepairsOnlyExactDeviceAuthorizationFailure(t *testing.T) {
	// A real read-only preflight establishes that recovery is never enrollment.
	// The injected runtime isolates network recovery and observes its authority.
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	for _, tc := range []struct {
		name        string
		status      int
		code        string
		recover     bool
		nativeError error
		wantExit    int
	}{
		{"revoked", 401, "api_key_invalid", true, nil, exitcode.Success},
		{"expired or used capability", 401, "api_key_invalid", true, qurl.ErrRecoveryCredentialRejected, exitcode.Auth},
		{"expired episode", 401, "api_key_invalid", true, qurl.ErrCredentialRecoveryExpired, exitcode.Auth},
		{"rejected grant", 401, "api_key_invalid", true, qurl.ErrCredentialRecoveryGrantRejected, exitcode.Unavailable},
		{"forbidden", 403, "api_key_invalid", false, nil, exitcode.Forbidden},
		{"other unauthorized", 401, "token_expired", false, nil, exitcode.Auth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			store, err := connectoragentstate.NewSDKStore(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			state := bootstrapRegisteredState(t)
			sdk, err := store.Handoff()
			if err != nil {
				t.Fatal(err)
			}
			if err := sdk.SaveAgentState(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(t.TempDir(), "recovery-token")
			const secret = "lv_test_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
			if err := os.WriteFile(tokenPath, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, tc.status, tc.code, "Rejected", "device credential rejected")
			})
			repaired := 0
			runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}
			runtime.recoverDeviceAuthorizationFailure = func(ctx context.Context, status int, code string, provider func(context.Context) (string, error)) error {
				repaired++
				if status != 401 || code != "api_key_invalid" {
					t.Fatal("wrong recovery trigger")
				}
				got, err := provider(ctx)
				if err != nil || got != secret {
					t.Fatal("recovery provider lost the explicit capability")
				}
				if tc.nativeError != nil {
					return fmt.Errorf("native recovery: %w", tc.nativeError)
				}
				state.DeviceAPIKey = "lv_live_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"
				state.DeviceAPIKeyID = "key_NewDvK123456"
				return nil
			}
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "login", "--recovery-token-file", tokenPath, "--supervision", "external", "-o", "json"}, env: externalLoginEnv(), shareStateDir: dir,
				openNativeRuntime: func(_ context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
					if cfg.EnrollmentCredentialProvider != nil || cfg.EnrollmentCredential != "" || cfg.RecoveryCredentialProvider == nil {
						t.Fatal("recovery gained enrollment authority or lost resume authority")
					}
					return runtime, nil
				},
			})
			calls := len(srv.Requests())
			if strings.Contains(res.stdout.String()+res.stderr.String(), secret) {
				t.Fatal("recovery disclosed its capability")
			}
			if tc.nativeError != nil {
				if res.code != tc.wantExit || repaired != 1 || calls != 1 {
					t.Fatalf("wrapped recovery exit=%d want=%d attempts=%d calls=%d", res.code, tc.wantExit, repaired, calls)
				}
				return
			}
			if tc.recover {
				if res.code != 0 || repaired != 1 || calls != 2 {
					t.Fatalf("recovery exit=%d attempts=%d calls=%d: %s", res.code, repaired, calls, res.stderr.String())
				}
			} else if res.code == 0 || repaired != 0 || calls != 1 {
				t.Fatal("unrelated failure triggered recovery")
			}
		})
	}
}

func TestExternalRecoveryRejectsMixedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"enrollment", []string{"--enrollment-token-file", "unused"}, externalLoginEnv()},
		{"account", nil, externalLoginEnv("QURL_API_KEY", "unused")},
		{"native supervision", []string{"--supervision", "native"}, externalLoginEnv()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			args := append([]string{"login", "--recovery-token-file", "unused", "--supervision", "external"}, tc.args...)
			res := runCLI(t, &runOpts{args: args, env: tc.env, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				called = true
				return nil, errors.New("unexpected runtime")
			}})
			if res.code == 0 || called {
				t.Fatal("mixed recovery authority was accepted")
			}
		})
	}
}
