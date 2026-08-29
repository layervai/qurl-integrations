package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

type bootstrapAgentStateStore struct{ state *qurl.AgentState }

func (s *bootstrapAgentStateStore) LoadAgentState(context.Context) (*qurl.AgentState, error) {
	stateCopy := *s.state
	registeredAt := *s.state.RegisteredAt
	stateCopy.RegisteredAt = &registeredAt
	assignment := *s.state.Assignment
	stateCopy.Assignment = &assignment
	return &stateCopy, nil
}

func (*bootstrapAgentStateStore) SaveAgentState(context.Context, *qurl.AgentState) error {
	return errors.New("registered bootstrap client unexpectedly saved state")
}

type bootstrapNativeRuntime struct {
	store    qurl.AgentStateStore
	closed   bool
	closeErr error
}

// ownerOnlyTestShareRegistry keeps the registered-device ownership seam
// hermetic and independent of filesystem state on every platform.
type ownerOnlyTestShareRegistry struct {
	ownerID          string
	bindRaceWinnerID string
	bindCalls        int
}

func (r *ownerOnlyTestShareRegistry) BindOwner(_ context.Context, ownerID string) error {
	r.bindCalls++
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return errors.New("test owner ID is empty")
	}
	if r.bindRaceWinnerID != "" {
		r.ownerID = r.bindRaceWinnerID
		return connectorstate.ErrLocalShareOwnerConflict
	}
	if r.ownerID != "" && r.ownerID != ownerID {
		return errors.New("test owner ID changed")
	}
	r.ownerID = ownerID
	return nil
}

func (r *ownerOnlyTestShareRegistry) OwnerID(context.Context) (ownerID string, present bool, err error) {
	return r.ownerID, r.ownerID != "", nil
}

func (*ownerOnlyTestShareRegistry) Get(context.Context, string) (*connectorstate.LocalShare, error) {
	return nil, errors.New("unexpected test registry Get")
}

func (*ownerOnlyTestShareRegistry) Put(context.Context, *connectorstate.LocalShare) error {
	return errors.New("unexpected test registry Put")
}

func (*ownerOnlyTestShareRegistry) SetDesired(context.Context, string, string, uint64) (*connectorstate.LocalShare, error) {
	return nil, errors.New("unexpected test registry SetDesired")
}

func (*ownerOnlyTestShareRegistry) Delete(context.Context, string) error {
	return errors.New("unexpected test registry Delete")
}

func (r *bootstrapNativeRuntime) Handoff() (qurl.AgentStateStore, error) { return r.store, nil }
func (r *bootstrapNativeRuntime) Close() error {
	r.closed = true
	return r.closeErr
}

func bootstrapRegisteredState(t *testing.T) *qurl.AgentState {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x37}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(time.Second)
	return &qurl.AgentState{
		AgentID: "agent-durable-01", PrivateKeyB64: base64.StdEncoding.EncodeToString(private.Bytes()),
		PublicKeyB64: base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()), SchemaVersion: 7,
		RegisteredAt: &now, DeviceAPIKey: "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		DeviceAPIKeyID: "key_AbCdEf123456", EnrollmentCredentialKind: "bootstrap",
		Assignment: &qurl.AgentAssignment{CellID: "cell-01", AssignmentGeneration: 7, EndpointRevision: 1,
			LeaseExpiresAt: now.Add(time.Hour), Endpoint: qurl.NHPUDPEndpoint{Host: "cell0.nhp.layerv.ai", Port: 443,
				ServerPublicKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32))}},
	}
}

func bootstrapGlobalOpts(t *testing.T, endpoint string, runtime *bootstrapNativeRuntime) *globalOpts {
	t.Helper()
	stateDir := connectorStateTestDir(t)
	registry := &ownerOnlyTestShareRegistry{}
	return &globalOpts{
		resolvedEndpoint: endpoint,
		version:          "bootstrap-test",
		lookupEnv: func(string) (string, bool) {
			t.Fatal("warm registered-device open read an account credential")
			return "", false
		},
		resolveShareStateDir: func(string) (string, error) { return stateDir, nil },
		resolveHubBootstrap:  func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil },
		openShareRegistry: func(string) (localShareRegistry, error) {
			return registry, nil
		},
		openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return runtime, nil
		},
	}
}

func TestOpenNativeRegisteredClient_OneTimeAccountEnrollment(t *testing.T) {
	srv := apitest.NewServer(t)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	var idempotencyKeys []string
	srv.ScriptRepeat(http.MethodPost, "/v1/api-keys", 2, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("enrollment authorization = %q", got)
		}
		idempotencyKeys = append(idempotencyKeys, r.Header.Get("Idempotency-Key"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if got := body["target"]; got != "agent" {
			t.Errorf("target = %v, want agent", got)
		}
		if _, present := body["claims"]; present {
			t.Error("unbound device enrollment must omit claims")
		}
		if _, present := body["scopes"]; present {
			t.Error("device enrollment must not request caller-selected scopes")
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
			"api_key": "one-shot-agent-enrollment", "key_id": "key_enrollment01",
			"kind": "enrollment_token", "target": "agent", "claims": []any{},
			"status": "active", "expires_at": expires,
		}, nil)
	})

	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)}}
	opts := bootstrapGlobalOpts(t, srv.URL, runtime)
	account, err := opts.apiClient(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	accountIdentity, err := account.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opts.openNativeRuntime = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		if cfg.ClientBaseURL != srv.URL || cfg.AgentID != connectorstate.ConfiguredAgentID() {
			t.Errorf("native config base/agent = %q/%q", cfg.ClientBaseURL, cfg.AgentID)
		}
		for range 2 {
			credential, providerErr := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent-durable-01"})
			if providerErr != nil || credential != "one-shot-agent-enrollment" {
				t.Fatalf("enrollment provider = %q, %v", credential, providerErr)
			}
		}
		return runtime, nil
	}

	client, deviceIdentity, err := opts.openNativeRegisteredClient(context.Background(), account, testAPIKey, accountIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || deviceIdentity.OwnerID != accountIdentity.OwnerID {
		t.Fatalf("registered identity = %#v", deviceIdentity)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] != idempotencyKeys[1] {
		t.Fatalf("idempotency keys = %q, want one attempt-scoped value reused twice", idempotencyKeys)
	}
	decodedIdempotency, err := hex.DecodeString(idempotencyKeys[0])
	if err != nil || len(decodedIdempotency) != 32 {
		t.Fatalf("idempotency key = %q, want 32 random bytes in hex: %v", idempotencyKeys[0], err)
	}
	for _, request := range srv.Requests() {
		if strings.Contains(request.Header.Get("Authorization"), "one-shot-agent-enrollment") {
			t.Error("one-shot enrollment credential reached the REST surface")
		}
	}
	if err := opts.closeAPIClient(); err != nil {
		t.Fatal(err)
	}
	if !runtime.closed {
		t.Error("native runtime was not closed")
	}
}

func TestOpenNativeRegisteredClient_WarmOpenDoesNotReadAccountKey(t *testing.T) {
	srv := apitest.NewServer(t)
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)}}
	opts := bootstrapGlobalOpts(t, srv.URL, runtime)
	client, identity, err := opts.openNativeRegisteredClient(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opts.closeAPIClient() }()
	if client == nil || identity == nil || identity.OwnerID != apitest.MeOwnerID {
		t.Fatalf("warm registered identity = %#v", identity)
	}
	if len(srv.Requests()) != 1 || srv.Requests()[0].Header.Get("Authorization") != "Bearer "+bootstrapRegisteredState(t).DeviceAPIKey {
		t.Fatalf("warm open requests = %+v", srv.Requests())
	}
}

func TestOpenNativeRegisteredClient_AccountSwitchHasSafeRecovery(t *testing.T) {
	srv := apitest.NewServer(t)
	state := bootstrapRegisteredState(t)
	runtime := &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}
	opts := bootstrapGlobalOpts(t, srv.URL, runtime)
	requested := &qurlapi.Identity{OwnerID: "owner-other-account"}

	_, _, err := opts.openNativeRegisteredClient(context.Background(), nil, "", requested)
	var conflict *deviceAccountConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, auth.ErrDeviceAccountConflict) || errors.Is(err, auth.ErrCredentialConflict) {
		t.Fatalf("account-switch error = %v, want typed credential conflict", err)
	}
	for _, want := range []string{
		apitest.MeKeyID,
		apitest.MeOwnerID,
		requested.OwnerID,
		"qURL dashboard",
		"move or remove the complete state directory",
		"do not edit individual state files",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("account-switch error = %q, want %q", err, want)
		}
	}
	if !runtime.closed {
		t.Fatal("account-switch rejection did not close the native runtime")
	}
}

func TestBindRegisteredDeviceOwnerReportsConcurrentRaceWinner(t *testing.T) {
	registry := &ownerOnlyTestShareRegistry{bindRaceWinnerID: "owner-race-winner"}
	err := bindRegisteredDeviceOwner(
		context.Background(), registry, "/state/connector-v2", "key-device", "owner-requested",
	)
	var conflict *deviceAccountConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, auth.ErrDeviceAccountConflict) || errors.Is(err, auth.ErrCredentialConflict) {
		t.Fatalf("concurrent owner error = %v, want typed credential conflict", err)
	}
	for _, want := range []string{"owner-race-winner", "owner-requested", "key-device", "/state/connector-v2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("concurrent owner error = %q, want %q", err, want)
		}
	}
}

func TestBindRegisteredDeviceOwnerDoesNotRewriteExistingBinding(t *testing.T) {
	registry := &ownerOnlyTestShareRegistry{ownerID: "owner-existing"}
	if err := bindRegisteredDeviceOwner(
		context.Background(), registry, "/state/connector-v2", "key-device", "owner-existing",
	); err != nil {
		t.Fatal(err)
	}
	if registry.bindCalls != 0 {
		t.Fatalf("warm owner binding called BindOwner %d time(s), want zero", registry.bindCalls)
	}
}

func TestRegisteredAccountBootstrap_LegacyCleanupFailureWarnsAfterEnrollment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deleteErr := errors.New("locked compatibility keyring")
	keyring := &fakeKeyring{key: testAPIKeyStored, deleteErr: deleteErr}
	opts := &globalOpts{
		streams:        &output.Streams{Out: &stdout, Err: &stderr},
		resolvedFormat: output.FormatText,
		now:            time.Now,
		configDir:      t.TempDir(),
		newCredentialStore: func(dir string, onFileRead func()) *auth.Chain {
			return auth.NewChain(keyring, auth.NewFileStore(dir), onFileRead)
		},
	}
	bootstrap := &registeredAccountBootstrap{opts: opts, lazy: true, used: true}
	bootstrap.retireLegacyKey()
	if !strings.Contains(stderr.String(), "machine enrollment succeeded") ||
		!strings.Contains(stderr.String(), "legacy stored account key") ||
		!strings.Contains(stderr.String(), deleteErr.Error()) {
		t.Fatalf("cleanup warning = %q", stderr.String())
	}
	if keyring.key != testAPIKeyStored {
		t.Fatal("failed compatibility-key cleanup claimed or performed removal")
	}
}

func TestLocalPublishReusesRegisteredIdentityWithoutSecondMeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "identity-cache-test"})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := &qurlapi.Identity{OwnerID: apitest.MeOwnerID}
	opts := &globalOpts{openRegisteredClient: func(context.Context, qurlapi.AccountClient, string, *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error) {
		return client, wantIdentity, nil
	}}
	registry := &ownerOnlyTestShareRegistry{}
	ownerID, gotClient, err := localPublishOwner(context.Background(), opts, registry, "/state/connector-v2")
	if err != nil {
		t.Fatal(err)
	}
	if ownerID != wantIdentity.OwnerID || gotClient != client || registry.ownerID != wantIdentity.OwnerID {
		t.Fatalf("local publish owner/client/registry = %q/%T/%q", ownerID, gotClient, registry.ownerID)
	}
	if registry.bindCalls != 1 {
		t.Fatalf("local publish bound owner %d time(s), want one", registry.bindCalls)
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("local publish repeated /v1/me after registered open: %+v", requests)
	}
}

func TestLocalPublishOwnerRaceReturnsSafeRecovery(t *testing.T) {
	srv := apitest.NewServer(t)
	client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "owner-race-test"})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := &qurlapi.Identity{OwnerID: "owner-requested", Key: &qurlapi.KeyIdentity{KeyID: "key-device"}}
	opts := &globalOpts{openRegisteredClient: func(context.Context, qurlapi.AccountClient, string, *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error) {
		return client, wantIdentity, nil
	}}
	registry := &ownerOnlyTestShareRegistry{bindRaceWinnerID: "owner-race-winner"}
	_, _, err = localPublishOwner(context.Background(), opts, registry, "/state/connector-v2")
	var conflict *deviceAccountConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, auth.ErrDeviceAccountConflict) || errors.Is(err, auth.ErrCredentialConflict) {
		t.Fatalf("local-publish owner race error = %v, want typed credential conflict", err)
	}
	for _, want := range []string{"owner-race-winner", "owner-requested", "key-device", "/state/connector-v2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("local-publish owner race error = %q, want %q", err, want)
		}
	}
}

func TestWhoamiReusesRegisteredIdentityWithoutSecondMeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "whoami-cache-test"})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := &qurlapi.Identity{OwnerID: apitest.MeOwnerID}
	var stdout, stderr bytes.Buffer
	root, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
		g.openAPIClient = nil
		g.openRegisteredClient = func(context.Context, qurlapi.AccountClient, string, *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error) {
			return client, wantIdentity, nil
		}
	})
	root.SetArgs([]string{"whoami"})
	if code := run(context.Background(), root, opts); code != 0 {
		t.Fatalf("whoami exit = %d, stderr = %q", code, stderr.String())
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("whoami repeated /v1/me after registered open: %+v", requests)
	}
	if !strings.Contains(stdout.String(), apitest.MeOwnerID) {
		t.Fatalf("whoami stdout = %q, want owner %q", stdout.String(), apitest.MeOwnerID)
	}
}

func TestRunKeepsCompletedCommandSuccessfulWhenNativeRuntimeCloseFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	streams := &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}
	root, opts := newRoot("close-test", streams)
	runtime := &bootstrapNativeRuntime{closeErr: errors.New("close failure")}
	opts.nativeRuntime = runtime
	root.SetArgs([]string{"help"})
	if code := run(context.Background(), root, opts); code != 0 {
		t.Fatalf("completed help exit = %d, stderr = %q", code, stderr.String())
	}
	if !runtime.closed || !strings.Contains(stderr.String(), "local native-state cleanup reported a problem") {
		t.Fatalf("runtime closed=%t stderr=%q", runtime.closed, stderr.String())
	}
}
