package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestLocalPublishBindsAuthenticatedOwnerBeforeNativeOpen(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after owner binding")
	var authority connectorshare.NativeSessionOperationAuthority
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(_ context.Context, cfg *connectorshare.NativeRuntimeConfig, _ func(string) (string, error)) (*agent.ResolvedResource, error) {
			authority = cfg.SessionOperations
			return nil, stop
		},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), stop.Error()) {
		t.Fatalf("result = exit %d stderr %q", res.code, res.stderr.String())
	}
	ownerID, present, err := registry.OwnerID(context.Background())
	if err != nil || !present || ownerID != apitest.MeOwnerID || authority.OwnerID != apitest.MeOwnerID {
		t.Fatalf("owner binding = durable %q/%v/%v authority %#v", ownerID, present, err, authority)
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/v1/me" {
		t.Fatalf("owner bootstrap requests = %#v", requests)
	}
}

func TestLocalPublishRejectsUnsupportedInputsBeforeStateOrNetwork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "description", args: []string{"publish", "http://127.0.0.1:3000", "--description", "local"}, want: "--description is not supported"},
		{name: "empty description still explicit", args: []string{"publish", "http://127.0.0.1:3000", "--description="}, want: "--description is not supported"},
		{name: "tag", args: []string{"publish", "http://localhost:3000", "--tag", "dev"}, want: "--tag is not supported"},
		{name: "alias", args: []string{"publish", "http://[::1]:3000", "--alias", "dev"}, want: "--alias is not supported"},
		{name: "https", args: []string{"publish", "https://localhost:3000"}, want: "cleartext http"},
		{name: "path", args: []string{"publish", "http://127.0.0.1:3000/api"}, want: "without a path"},
		{name: "remote id", args: []string{"publish", "https://example.com", "--id", "local-test"}, want: "--id applies only"},
		{name: "remote foreground", args: []string{"publish", "https://example.com", "--foreground"}, want: "--foreground applies only"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			srv := apitest.NewServer(t)
			opens := 0
			args := append([]string{"--endpoint", srv.URL}, test.args...)
			res := runCLI(t, &runOpts{
				args: args,
				env:  map[string]string{},
				localResource: func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
					opens++
					return nil, errors.New("unexpected Connector open")
				},
			})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), test.want) {
				t.Errorf("stderr missing %q:\n%s", test.want, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			if opens != 0 {
				t.Errorf("invalid input opened Connector state %d times", opens)
			}
			if got := len(srv.Requests()); got != 0 {
				t.Errorf("invalid input sent %d API requests", got)
			}
		})
	}
}

func TestLocalPublishAlwaysUsesAutomaticAssignmentRecovery(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var gotRefreshMode string
	var recoveryCredential string
	var sessionOperations connectorshare.NativeSessionOperationAuthority
	res := runCLI(t, &runOpts{
		args:            []string{"publish", "http://127.0.0.1:3000"},
		env:             map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry:   registry,
		shareStateDir:   stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(_ context.Context, cfg *connectorshare.NativeRuntimeConfig, _ func(string) (string, error)) (*agent.ResolvedResource, error) {
			gotRefreshMode = cfg.RefreshMode
			sessionOperations = cfg.SessionOperations
			if cfg.RecoveryCredentialProvider == nil {
				return nil, errors.New("missing Connector credential-recovery provider")
			}
			var err error
			recoveryCredential, err = cfg.RecoveryCredentialProvider(context.Background())
			if err != nil {
				return nil, err
			}
			return nil, errors.New("stop after inspecting Connector configuration")
		},
	})
	if res.code != 1 || !strings.Contains(res.stderr.String(), "stop after inspecting") {
		t.Fatalf("result = exit %d stderr %q", res.code, res.stderr.String())
	}
	if gotRefreshMode != "auto" {
		t.Fatalf("Connector refresh mode = %q, want auto", gotRefreshMode)
	}
	if recoveryCredential != testAPIKey {
		t.Fatalf("Connector recovery credential did not resolve the exact signed-in account authority")
	}
	if sessionOperations.OwnerID != "own_cli_fixture" {
		t.Fatalf("Connector session operation authority = %#v", sessionOperations)
	}
	mustEmptyStdout(t, res)
}

func TestLocalPublishRejectsInvalidExplicitConnectorIDBeforeOpen(t *testing.T) {
	t.Parallel()
	opens := 0
	res := runCLI(t, &runOpts{
		args: []string{"publish", "http://127.0.0.1:3000", "--id", "NOT_VALID"},
		env:  map[string]string{},
		preflightTarget: func(context.Context, string, int) error {
			opens++
			return errors.New("unexpected target preflight")
		},
	})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "invalid Connector ID") {
		t.Fatalf("result = exit %d stderr %q", res.code, res.stderr.String())
	}
	if opens != 0 {
		t.Fatalf("invalid ID opened Connector state %d times", opens)
	}
}
