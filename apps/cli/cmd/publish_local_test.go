package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type emptyConnectorEnrollmentClient struct{ qurlapi.Client }

func (emptyConnectorEnrollmentClient) MintConnectorEnrollmentToken(context.Context, qurlapi.MintConnectorEnrollmentTokenOptions) (*qurlapi.ConnectorEnrollmentToken, error) {
	return nil, nil //nolint:nilnil // The test pins the caller's fail-closed guard for an invalid implementation.
}

func TestLocalPublishBindsAuthenticatedOwnerBeforeNativeOpen(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry := &ownerOnlyTestShareRegistry{}
	stop := errors.New("stop after owner binding")
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(_ context.Context, cfg *connectorshare.NativeRuntimeConfig, _ func(string) (string, error)) (*agent.ResolvedResource, error) {
			if cfg.SessionOperations.OwnerID != "" || len(cfg.SessionOptions) != 0 {
				t.Fatalf("discovery-only runtime retained session authority: %#v / %d options", cfg.SessionOperations, len(cfg.SessionOptions))
			}
			return nil, stop
		},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), stop.Error()) {
		t.Fatalf("result = exit %d stderr %q", res.code, res.stderr.String())
	}
	ownerID, present, err := registry.OwnerID(context.Background())
	if err != nil || !present || ownerID != apitest.MeOwnerID {
		t.Fatalf("owner binding = durable %q/%v/%v", ownerID, present, err)
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/v1/me" {
		t.Fatalf("owner bootstrap requests = %#v", requests)
	}
}

func TestLocalEnrollmentIdempotencyIsAttemptScoped(t *testing.T) {
	first := &localEnrollment{}
	key1, err := first.enrollmentAttemptKey("agent-a", "local-a")
	if err != nil {
		t.Fatal(err)
	}
	key1Again, err := first.enrollmentAttemptKey("agent-a", "local-a")
	if err != nil {
		t.Fatal(err)
	}
	second := &localEnrollment{}
	key2, err := second.enrollmentAttemptKey("agent-a", "local-a")
	if err != nil {
		t.Fatal(err)
	}
	if key1 != key1Again || key1 == key2 {
		t.Fatalf("attempt keys first=%q repeated=%q next-process=%q", key1, key1Again, key2)
	}
	if _, err := first.enrollmentAttemptKey("agent-b", "local-a"); err == nil {
		t.Fatal("one enrollment attempt accepted a changed Agent identity")
	}
}

func TestLocalEnrollmentRejectsEmptyMintedCredential(t *testing.T) {
	enrollment := &localEnrollment{
		opts:        &globalOpts{registeredClient: emptyConnectorEnrollmentClient{}},
		target:      &publishTarget{},
		requestedID: "local-a",
	}
	_, err := enrollment.credential(context.Background(), qurl.AgentEnrollmentCredentialRequest{AgentID: "agent-a"})
	if err == nil || !strings.Contains(err.Error(), "empty Connector enrollment credential") {
		t.Fatalf("empty enrollment credential error = %v", err)
	}
}

func TestForegroundPublishKeepsDaemonFailureJoinedWithExpectedCancellation(t *testing.T) {
	daemonFailure := errors.New("daemon shutdown failed")
	got := withoutExpectedDaemonCancellation(errors.Join(context.Canceled, daemonFailure))
	if !errors.Is(got, daemonFailure) || errors.Is(got, context.Canceled) {
		t.Fatalf("filtered daemon error = %v, want only %v", got, daemonFailure)
	}
	if got := withoutExpectedDaemonCancellation(errors.Join(context.Canceled)); got != nil {
		t.Fatalf("filtered expected cancellation = %v, want nil", got)
	}
	wrappedCancellation := fmt.Errorf("stop platform IPC: %w", context.Canceled)
	if got := withoutExpectedDaemonCancellation(wrappedCancellation); got != nil {
		t.Fatalf("filtered wrapped expected cancellation = %v, want nil", got)
	}
	got = withoutExpectedDaemonCancellation(errors.Join(wrappedCancellation, daemonFailure))
	if !errors.Is(got, daemonFailure) || errors.Is(got, context.Canceled) {
		t.Fatalf("filtered wrapped cancellation plus daemon failure = %v, want only %v", got, daemonFailure)
	}
	nestedJoin := fmt.Errorf("share daemon: %w", errors.Join(wrappedCancellation, daemonFailure))
	got = withoutExpectedDaemonCancellation(nestedJoin)
	if !errors.Is(got, daemonFailure) || errors.Is(got, context.Canceled) {
		t.Fatalf("filtered annotated join = %v, want only %v", got, daemonFailure)
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
	stateDir := connectorStateTestDir(t)
	registry := &ownerOnlyTestShareRegistry{ownerID: "own_cli_fixture"}
	var gotRefreshMode string
	var recoveryCredential string
	res := runCLI(t, &runOpts{
		args:            []string{"publish", "http://127.0.0.1:3000"},
		env:             map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry:   registry,
		shareStateDir:   stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(_ context.Context, cfg *connectorshare.NativeRuntimeConfig, _ func(string) (string, error)) (*agent.ResolvedResource, error) {
			gotRefreshMode = cfg.RefreshMode
			if cfg.SessionOperations.OwnerID != "" || len(cfg.SessionOptions) != 0 {
				return nil, fmt.Errorf("discovery-only runtime retained session authority: %#v / %d options", cfg.SessionOperations, len(cfg.SessionOptions))
			}
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

func TestLocalEnrollmentAdvancesDefaultIDOnlyAfterDurableRetirement(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	target, err := classifyPublishTarget("http://127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := generatedLocalConnectorID("agent-one", target.canonicalOrigin)
	if err != nil {
		t.Fatal(err)
	}
	firstServer := apitest.NewServer(t)
	first := localShareFixture(firstServer)
	first.ConnectorID = baseID
	seedLocalConnectorResourceBinding(t, stateDir, &first)
	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := store.RetireConnectorResource(context.Background(), first.ResourceID)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !retired {
		t.Fatalf("retire first binding = %t, %v", retired, err)
	}

	want, err := generatedReplacementLocalConnectorID(baseID, first.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := &localEnrollment{target: target}
	got, err := enrollment.resolveID(context.Background(), stateDir, "agent-one")
	if err != nil || got != want {
		t.Fatalf("resolveID() = %q, %v, want %q", got, err, want)
	}

	explicit := &localEnrollment{target: target, requestedID: baseID}
	if _, err := explicit.resolveID(context.Background(), stateDir, "agent-one"); err == nil || !strings.Contains(err.Error(), "choose a new value with --id") {
		t.Fatalf("retired explicit resolveID() = %v", err)
	}
}
