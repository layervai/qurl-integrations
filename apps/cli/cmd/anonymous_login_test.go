package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestAnonymousExternalLogin(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	state := bootstrapRegisteredState(t)
	res := runCLI(t, &runOpts{
		args: []string{"login", "--anonymous", "--supervision", "external", "--endpoint", srv.URL, "-o", "json"},
		env:  map[string]string{"LAYERV_KEY_PROVIDER": "local-key", "LAYERV_LOCAL_KEY_FD": "3"}, shareStateDir: dir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if err := connectorstate.RequireRuntimeSupervision(dir, connectorstate.RuntimeSupervisionExternal); err != nil {
				t.Fatal(err)
			}
			request := qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID, PublicKeyB64: state.PublicKeyB64}
			got, err := cfg.EnrollmentCredentialProvider(ctx, request)
			want, _ := qurl.AnonymousEnrollmentCredential(ctx, request)
			if err != nil || got != want {
				t.Fatal("wrong anonymous enrollment credential")
			}
			if _, err := cfg.RecoveryCredentialProvider(ctx); err == nil {
				t.Fatal("anonymous device acquired recovery authority")
			}
			return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
		},
	})
	if res.code != 0 || !strings.Contains(res.stdout.String(), `"device_enrolled": true`) {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
}

func TestAnonymousLoginRejectsUnsafeConfiguration(t *testing.T) {
	for _, extra := range [][]string{{}, {"--supervision", "external", "--enrollment-token-file", "unused"}, {"--supervision", "external"}} {
		res := runCLI(t, &runOpts{args: append([]string{"login", "--anonymous"}, extra...), env: map[string]string{}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			t.Error("opened runtime")
			return nil, errors.New("unexpected")
		}})
		if res.code == 0 {
			t.Fatal("accepted unsafe enrollment")
		}
	}
}

func TestAnonymousLoginRejectsAccountCredentials(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "QURL_API_KEY_FILE"} {
		res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external"}, env: map[string]string{"LAYERV_KEY_PROVIDER": "local-key", "LAYERV_LOCAL_KEY_FD": "3", name: "credential-do-not-read"}, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			t.Error("opened runtime")
			return nil, errors.New("unexpected")
		}})
		if res.code != 2 || strings.Contains(res.stderr.String(), "credential-do-not-read") {
			t.Fatalf("exit %d: %s", res.code, res.stderr.String())
		}
	}
}

func TestAnonymousLoginPreservesFailedExternalNamespace(t *testing.T) {
	dir := connectorStateTestDir(t)
	failed := errors.New("device state unavailable")
	for attempt := 0; attempt < 2; attempt++ {
		res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external"}, env: map[string]string{"LAYERV_KEY_PROVIDER": "local-key", "LAYERV_LOCAL_KEY_FD": "3"}, shareStateDir: dir, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return nil, failed
		}})
		if res.code == 0 || !strings.Contains(res.stderr.String(), failed.Error()) {
			t.Fatalf("lost error: %s", res.stderr.String())
		}
		if err := connectorstate.RequireRuntimeSupervision(dir, connectorstate.RuntimeSupervisionExternal); err != nil {
			t.Fatal(err)
		}
	}
}
