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
		env:  externalLoginEnv(), shareStateDir: dir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if err := connectorstate.RequireRuntimeSupervision(dir, connectorstate.RuntimeSupervisionExternal); err != nil {
				t.Fatal(err)
			}
			request := qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID, PublicKeyB64: state.PublicKeyB64}
			got, err := cfg.EnrollmentCredentialProvider(ctx, request)
			want, wantErr := qurl.AnonymousEnrollmentCredential(ctx, request)
			if err != nil || wantErr != nil || want == "" || got != want {
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
	for _, tc := range []struct {
		extra []string
		want  string
	}{
		{nil, "--anonymous requires --supervision external"},
		{[]string{"--supervision", "external", "--enrollment-token-file", "unused"}, "--anonymous cannot be combined with --enrollment-token-file"},
		{[]string{"--supervision", "external"}, "--anonymous requires LAYERV_KEY_PROVIDER"},
	} {
		res := runCLI(t, &runOpts{args: append([]string{"login", "--anonymous"}, tc.extra...), env: map[string]string{}, openNativeRuntime: refuseNativeRuntime(t)})
		if res.code == 0 || !strings.Contains(res.stderr.String(), tc.want) {
			t.Fatalf("%v: exit %d: %s", tc.extra, res.code, res.stderr.String())
		}
	}
}

func TestAnonymousLoginRejectsAccountCredentials(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "QURL_API_KEY_FILE"} {
		res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external"}, env: externalLoginEnv(name, "credential-do-not-read"), openNativeRuntime: refuseNativeRuntime(t)})
		if res.code != 2 || strings.Contains(res.stderr.String(), "credential-do-not-read") {
			t.Fatalf("exit %d: %s", res.code, res.stderr.String())
		}
	}
}

func TestAnonymousLoginPreservesFailedExternalNamespace(t *testing.T) {
	dir := connectorStateTestDir(t)
	failed := errors.New("device state unavailable")
	for attempt := 0; attempt < 2; attempt++ {
		res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external"}, env: externalLoginEnv(), shareStateDir: dir, openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
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

func TestAnonymousLoginInvalidEndpointLeavesNamespaceUnmarked(t *testing.T) {
	dir := connectorStateTestDir(t)
	res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external", "--endpoint", "https://api.example.test/v1?x=1"}, env: externalLoginEnv(), shareStateDir: dir, openNativeRuntime: refuseNativeRuntime(t)})
	if res.code == 0 {
		t.Fatal("accepted an invalid endpoint")
	}
	mustNoExternalPolicy(t, dir)
}

func TestAnonymousLoginNamesItsOwnFlag(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external"}, env: map[string]string{}})
	if !strings.Contains(res.stderr.String(), "--anonymous requires LAYERV_KEY_PROVIDER") {
		t.Fatalf("wrong flag: %s", res.stderr.String())
	}
}

func TestAnonymousLoginWarnsBeforeCleartextEnrollment(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external", "--endpoint", "http://api.example.test"}, env: externalLoginEnv(), openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		return nil, errors.New("stop before network")
	}})
	if strings.Count(res.stderr.String(), "authorization credential would travel unencrypted") != 1 {
		t.Fatalf("missing warning: %s", res.stderr.String())
	}
}

func TestAnonymousLoginRejectsConnectorScopedState(t *testing.T) {
	srv := apitest.NewServer(t)
	state := bootstrapRegisteredState(t)
	state.EnrollmentCredentialKind = string(qurl.RegistrationKeyKindConnectorBootstrap)
	res := runCLI(t, &runOpts{args: []string{"login", "--anonymous", "--supervision", "external", "--endpoint", srv.URL}, env: externalLoginEnv(), openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
	}})
	if res.code == 0 {
		t.Fatal("connector-scoped identity reported successful enrollment")
	}
	// No Me call means no owner binding either: binding needs Me's identity.
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("contacted the server %d times before rejecting the device", n)
	}
}

func TestAnonymousLoginReopensExistingExternalNamespace(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	state := bootstrapRegisteredState(t)
	var outputs []string
	for attempt := 0; attempt < 2; attempt++ {
		enrolled := attempt == 0
		res := runCLI(t, &runOpts{
			args: []string{"login", "--anonymous", "--supervision", "external", "--endpoint", srv.URL, "-o", "json"},
			env:  externalLoginEnv(), shareStateDir: dir,
			openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				// A warm namespace reopens the persisted device; only the first
				// launch spends the anonymous enrollment credential.
				if enrolled {
					if _, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID, PublicKeyB64: state.PublicKeyB64}); err != nil {
						t.Fatal(err)
					}
				}
				return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
			},
		})
		if res.code != 0 || !strings.Contains(res.stdout.String(), `"device_enrolled": true`) {
			t.Fatalf("attempt %d exit %d: %s %s", attempt, res.code, res.stdout.String(), res.stderr.String())
		}
		if err := connectorstate.RequireRuntimeSupervision(dir, connectorstate.RuntimeSupervisionExternal); err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, res.stdout.String())
	}
	if outputs[0] != outputs[1] {
		t.Fatalf("reopened identity changed:\n%s\n%s", outputs[0], outputs[1])
	}
}
