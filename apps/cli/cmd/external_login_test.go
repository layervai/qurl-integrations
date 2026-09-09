package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// externalLoginEnv is the supervisor's process contract for an external
// enrollment: a local-key sealed namespace and no account credential. extra
// holds key/value pairs layered on top of it.
func externalLoginEnv(extra ...string) map[string]string {
	env := map[string]string{
		connectoragentstate.EnvKeyProvider: connectoragentstate.KeyProviderLocalKey,
		connectoragentstate.EnvLocalKeyFD:  "3",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	return env
}

// refuseNativeRuntime fails the test if a rejected external login reaches the
// native runtime at all.
func refuseNativeRuntime(t *testing.T) func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
	t.Helper()
	return func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Fatal("rejected external login opened the native runtime")
		return nil, errors.New("unreachable native runtime")
	}
}

func mustNoExternalPolicy(t *testing.T, stateDir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.RuntimeModeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected external login established the policy marker: %v", err)
	}
}

// TestExternalLoginRequiresExternalSupervision pins the command surface: the
// token-file form is only meaningful under external supervision, and a
// mismatch is a usage error that reads neither stdin nor the namespace.
func TestExternalLoginRequiresExternalSupervision(t *testing.T) {
	srv := apitest.NewServer(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	cases := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "default native", args: []string{"login", "--enrollment-token-file", tokenPath}, env: externalLoginEnv()},
		{name: "explicit native flag", args: []string{"--supervision", "native", "login", "--enrollment-token-file", tokenPath}, env: externalLoginEnv()},
		{name: "native environment", args: []string{"login", "--enrollment-token-file", tokenPath}, env: externalLoginEnv(connectorstate.EnvRuntimeSupervision, "native")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			res := runCLI(t, &runOpts{
				args:              append([]string{"--endpoint", srv.URL}, tc.args...),
				env:               tc.env,
				stdin:             strings.NewReader(testAPIKey + "\n"),
				shareStateDir:     stateDir,
				openNativeRuntime: refuseNativeRuntime(t),
			})
			if res.code != 2 || !strings.Contains(res.stderr.String(), "--supervision external") {
				t.Fatalf("exit = %d stderr = %q, want usage error naming --supervision external", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			mustNoExternalPolicy(t, stateDir)
		})
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("rejected external logins fell through to account login: %+v", requests)
	}
}

func TestExternalLoginRefusesAccountKeyAuthority(t *testing.T) {
	srv := apitest.NewServer(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	for name, env := range map[string]map[string]string{
		"inline key": externalLoginEnv(auth.EnvAPIKey, testAPIKey),
		"key file":   externalLoginEnv(auth.EnvAPIKeyFile, filepath.Join(t.TempDir(), "api-key")),
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			res := runCLI(t, &runOpts{
				args:              []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
				env:               env,
				shareStateDir:     stateDir,
				openNativeRuntime: refuseNativeRuntime(t),
			})
			if res.code != 2 || !strings.Contains(res.stderr.String(), auth.EnvAPIKey) {
				t.Fatalf("exit = %d stderr = %q, want usage error naming the account-key environment", res.code, res.stderr.String())
			}
			if strings.Contains(res.stderr.String(), testAPIKey) {
				t.Fatal("rejected external login echoed the account key")
			}
			mustEmptyStdout(t, res)
			mustNoExternalPolicy(t, stateDir)
		})
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("rejected external logins reached the service: %+v", requests)
	}
}

func TestExternalLoginRequiresTheLocalKeyProviderContract(t *testing.T) {
	const malformedDescriptor = "descriptor-value-must-not-be-echoed"
	srv := apitest.NewServer(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	cases := map[string]map[string]string{
		"no provider":          {connectoragentstate.EnvLocalKeyFD: "3"},
		"file provider":        {connectoragentstate.EnvKeyProvider: connectoragentstate.KeyProviderFile, connectoragentstate.EnvLocalKeyFD: "3"},
		"no descriptor":        {connectoragentstate.EnvKeyProvider: connectoragentstate.KeyProviderLocalKey},
		"empty descriptor":     externalLoginEnv(connectoragentstate.EnvLocalKeyFD, ""),
		"reserved descriptor":  externalLoginEnv(connectoragentstate.EnvLocalKeyFD, "2"),
		"malformed descriptor": externalLoginEnv(connectoragentstate.EnvLocalKeyFD, malformedDescriptor),
		"padded descriptor":    externalLoginEnv(connectoragentstate.EnvLocalKeyFD, " 3"),
		"signed descriptor":    externalLoginEnv(connectoragentstate.EnvLocalKeyFD, "+3"),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			res := runCLI(t, &runOpts{
				args:              []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
				env:               env,
				shareStateDir:     stateDir,
				openNativeRuntime: refuseNativeRuntime(t),
			})
			stderr := res.stderr.String()
			if res.code != 2 || (!strings.Contains(stderr, connectoragentstate.EnvKeyProvider) && !strings.Contains(stderr, connectoragentstate.EnvLocalKeyFD)) {
				t.Fatalf("exit = %d stderr = %q, want usage error naming the local-key environment", res.code, stderr)
			}
			if strings.Contains(stderr, malformedDescriptor) {
				t.Fatalf("rejected external login echoed the descriptor value: %q", stderr)
			}
			mustEmptyStdout(t, res)
			mustNoExternalPolicy(t, stateDir)
		})
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("rejected external logins reached the service: %+v", requests)
	}
}

// TestExternalLoginRejectsUnusableTokenPaths pins that an explicitly empty or
// non-absolute token path is a usage error and never falls through to the
// account-key login, even with a valid key waiting on stdin.
func TestExternalLoginRejectsUnusableTokenPaths(t *testing.T) {
	srv := apitest.NewServer(t)
	valid := filepath.Join(t.TempDir(), "enrollment-token")
	sep := string(filepath.Separator)
	cases := map[string]string{
		"explicit empty": "",
		"relative":       "enrollment-token",
		"unclean":        filepath.Dir(valid) + sep + "child" + sep + ".." + sep + "enrollment-token",
		"padded":         " " + valid,
	}
	for name, tokenPath := range cases {
		t.Run(name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			res := runCLI(t, &runOpts{
				args:              []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
				env:               externalLoginEnv(),
				stdin:             strings.NewReader(testAPIKey + "\n"),
				shareStateDir:     stateDir,
				openNativeRuntime: refuseNativeRuntime(t),
			})
			if res.code != 2 || !strings.Contains(res.stderr.String(), "absolute") {
				t.Fatalf("exit = %d stderr = %q, want usage error about the token path", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			mustNoExternalPolicy(t, stateDir)
		})
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("unusable token paths fell through to account login: %+v", requests)
	}
}

func TestExternalLoginPreflightsTheEndpointBeforeTouchingTheNamespace(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	res := runCLI(t, &runOpts{
		args:              []string{"--endpoint", "not-an-absolute-endpoint", "--supervision", "external", "login", "--enrollment-token-file", filepath.Join(t.TempDir(), "enrollment-token")},
		env:               externalLoginEnv(),
		shareStateDir:     stateDir,
		openNativeRuntime: refuseNativeRuntime(t),
	})
	if res.code == 0 {
		t.Fatalf("invalid endpoint was accepted: stdout=%q stderr=%q", res.stdout.String(), res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint preflight failure created the namespace: %v", err)
	}
}

func TestExternalLoginRefusesAnotherAccountsNamespaceWithoutOverwrite(t *testing.T) {
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
	if err := registry.BindOwner(ctx, "owner-other"); err != nil {
		t.Fatal(err)
	}
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)}}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", filepath.Join(t.TempDir(), "absent-token")},
		env:           externalLoginEnv(),
		shareStateDir: stateDir,
		openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return runtime, nil
		},
	})
	if res.code != 7 || !strings.Contains(res.stderr.String(), "owner-other") {
		t.Fatalf("wrong-owner external login = exit %d stderr %q, want the account conflict", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if owner, _, err := registry.OwnerID(ctx); err != nil || owner != "owner-other" {
		t.Fatalf("wrong-owner registry was rewritten: owner=%q err=%v", owner, err)
	}
	if !runtime.closed {
		t.Fatal("refused external login left the native runtime open")
	}
}

func TestPlainLoginDoesNotEstablishExternalPolicy(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "login"},
		env:           map[string]string{},
		stdin:         strings.NewReader(testAPIKey + "\n"),
		shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("plain login = exit %d stderr %q", res.code, res.stderr.String())
	}
	mustNoExternalPolicy(t, stateDir)
}
