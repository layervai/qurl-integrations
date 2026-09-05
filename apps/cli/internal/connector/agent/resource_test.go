package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

var routingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func testNativeResource(t *testing.T, connectorID string) *qurl.ConnectorResource {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return &qurl.ConnectorResource{
		ResourceID:         base64.RawURLEncoding.EncodeToString(der),
		CRID:               apitest.DeriveCRID(t, der, apitest.VersionProduction),
		ConnectorRoutingID: "c-" + routingIDEncoding.EncodeToString(digest[:]),
		KnockResourceID:    "nhp-resource-a",
		Slug:               connectorID,
	}
}

func TestResolveResourceRejectsMissingCRID(t *testing.T) {
	store := openResourceTestStore(t)
	resource := testNativeResource(t, "missing-crid")
	resource.CRID = ""
	installResourceResolver(t, func(context.Context, *qurl.AgentRuntimeBinding,
		*qurl.NativeConnectorResourceRequest, ...qurl.AgentRuntimeUDPOption,
	) (*qurl.ConnectorResourceResolution, error) {
		return &qurl.ConnectorResourceResolution{Resource: resource}, nil
	})
	_, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, resource.Slug)
	if !errors.Is(err, state.ErrConnectorResourceVerification) || !strings.Contains(err.Error(), "crid is required") {
		t.Fatalf("missing native CRID = %v, want terminal verification error", err)
	}
	if pending := pendingRequestFromDisk(t, store, resource.Slug); pending != nil {
		t.Fatalf("missing native CRID retained request: %+v", pending)
	}
}

func installResourceResolver(t *testing.T, resolver func(context.Context, *qurl.AgentRuntimeBinding, *qurl.NativeConnectorResourceRequest, ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error)) {
	t.Helper()
	original := resolveRegisteredAgentConnectorResource
	resolveRegisteredAgentConnectorResource = resolver
	t.Cleanup(func() { resolveRegisteredAgentConnectorResource = original })
}

func openResourceTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(resourceTestStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func resourceTestStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := state.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pendingRequestFromDisk(t *testing.T, store *state.Store, connectorID string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Dir(), state.ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Pending map[string]map[string]any `json:"pending"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Pending[connectorID]
}

func TestResolveResourcePersistsBeforeDispatchAndCommitsCompleteBinding(t *testing.T) {
	store := openResourceTestStore(t)
	resource := testNativeResource(t, "billing-api")
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, request *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		pending := pendingRequestFromDisk(t, store, request.ConnectorID)
		if got := pending["request_nonce"]; got != request.RequestNonce {
			t.Fatalf("durable nonce before dispatch = %v, want %q", got, request.RequestNonce)
		}
		if _, exists := pending["expected_resource_id"]; exists {
			t.Fatal("fresh request persisted an expected_resource_id")
		}
		return &qurl.ConnectorResourceResolution{Resource: resource, FoundExisting: false}, nil
	})

	result, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "billing-api")
	if err != nil {
		t.Fatal(err)
	}
	if result.Resource != resource || result.FoundExisting == nil || *result.FoundExisting {
		t.Fatalf("result = %+v, want created authenticated resource", result)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir(), state.ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Bindings map[string]state.ConnectorResourceBinding        `json:"bindings"`
		Pending  map[string]state.PendingConnectorResourceRequest `json:"pending"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Pending) != 0 {
		t.Fatalf("pending after commit = %+v, want empty", envelope.Pending)
	}
	if got := envelope.Bindings["billing-api"]; got.ResourceID != resource.ResourceID || got.ConnectorRoutingID != resource.ConnectorRoutingID || got.KnockResourceID != resource.KnockResourceID {
		t.Fatalf("binding = %+v, want complete authenticated resource", got)
	}
}

func TestResolveConfiguredResourceReauthorizesExactBinding(t *testing.T) {
	store := openResourceTestStore(t)
	resource := testNativeResource(t, "headless-api")
	t.Setenv(EnvKnockResourceID, "headless-knock-override")
	configured := &state.ConnectorResourceBinding{
		ConnectorID: resource.Slug, ResourceID: resource.ResourceID, CRID: resource.CRID,
		ConnectorRoutingID: resource.ConnectorRoutingID, KnockResourceID: "headless-knock-override",
	}
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, request *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		if request.ConnectorID != configured.ConnectorID || request.ExpectedResourceID != configured.ResourceID || request.RequestNonce == "" {
			t.Fatalf("configured native request = %+v", request)
		}
		return &qurl.ConnectorResourceResolution{Resource: resource, FoundExisting: true}, nil
	})

	result, err := ResolveConfiguredResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, configured)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Resource != resource || result.FoundExisting == nil || !*result.FoundExisting {
		t.Fatalf("configured result = %+v", result)
	}
	durable, retired, found, err := store.ConnectorResourceBinding(context.Background(), configured.ConnectorID)
	if err != nil || !found || retired || durable.KnockResourceID != resource.KnockResourceID {
		t.Fatalf("durable platform binding = %+v retired=%t found=%t err=%v", durable, retired, found, err)
	}
}

func TestResolveConfiguredResourceRejectsEffectiveKnockMismatch(t *testing.T) {
	store := openResourceTestStore(t)
	resource := testNativeResource(t, "headless-api")
	configured := &state.ConnectorResourceBinding{
		ConnectorID: resource.Slug, ResourceID: resource.ResourceID, CRID: resource.CRID,
		ConnectorRoutingID: resource.ConnectorRoutingID, KnockResourceID: "configured-knock-override",
	}
	t.Setenv(EnvKnockResourceID, "different-knock-override")
	installResourceResolver(t, func(context.Context, *qurl.AgentRuntimeBinding, *qurl.NativeConnectorResourceRequest, ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		t.Fatal("conflicting local knock override reached the assigned cell")
		return nil, errors.New("unexpected resolver call")
	})

	_, err := ResolveConfiguredResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, configured)
	if !errors.Is(err, state.ErrConnectorResourceVerification) || !strings.Contains(err.Error(), EnvKnockResourceID) {
		t.Fatalf("effective knock mismatch = %v, want actionable verification error", err)
	}
	if _, statErr := os.Lstat(filepath.Join(store.Dir(), state.ConnectorResourcesFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("effective knock mismatch changed resource state: %v", statErr)
	}
}

func TestResolveResourceLostResponseReplaysExactNonceThenWarmStartPinsIdentity(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "fresh name"
		if reuse {
			name = "reused name after delete"
		}
		t.Run(name, func(t *testing.T) { testResolveResourceLostResponse(t, reuse) })
	}
}

func testResolveResourceLostResponse(t *testing.T, reuse bool) {
	t.Helper()
	dir := resourceTestStateDir(t)
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	resource := testNativeResource(t, "orders-api")
	if reuse {
		old := testNativeResource(t, resource.Slug)
		tx, err := store.BeginConnectorResource(context.Background(), old.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(&state.ConnectorResourceBinding{
			ConnectorID: old.Slug, ResourceID: old.ResourceID, CRID: old.CRID,
			ConnectorRoutingID: old.ConnectorRoutingID, KnockResourceID: old.KnockResourceID,
		}); err != nil {
			_ = tx.Close()
			t.Fatal(err)
		}
		if err := tx.Close(); err != nil {
			t.Fatal(err)
		}
		if retired, err := store.RetireConnectorResource(context.Background(), old.CRID); err != nil || !retired {
			t.Fatalf("retire original resource = %t, %v", retired, err)
		}
		if err := store.PrepareConnectorResourceReuse(context.Background(), old.Slug); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var requests []qurl.NativeConnectorResourceRequest
	lost := true
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, request *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		mu.Lock()
		requests = append(requests, *request)
		first := lost
		lost = false
		mu.Unlock()
		if first {
			return nil, errors.New("UDP response lost after dispatch")
		}
		return &qurl.ConnectorResourceResolution{Resource: resource, FoundExisting: true}, nil
	})

	if _, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "orders-api"); err == nil {
		t.Fatal("lost response = nil error")
	}
	if pendingRequestFromDisk(t, store, "orders-api") == nil {
		t.Fatal("lost response cleared the durable request")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(dir)
	if err != nil {
		t.Fatalf("reopen state after simulated process restart: %v", err)
	}
	if _, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "orders-api"); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "orders-api"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want lost + exact replay + warm continuity", len(requests))
	}
	if requests[0].ExpectedResourceID != "" {
		t.Fatal("new resource request retained the deleted resource identity")
	}
	if requests[0] != requests[1] {
		t.Fatalf("lost-response replay changed request:\nfirst %+v\nretry %+v", requests[0], requests[1])
	}
	if requests[2].RequestNonce == requests[1].RequestNonce {
		t.Fatal("completed warm start reused the spent nonce")
	}
	if requests[2].ExpectedResourceID != resource.ResourceID {
		t.Fatalf("warm expected_resource_id = %q, want %q", requests[2].ExpectedResourceID, resource.ResourceID)
	}
}

// A failed service-side slug release can leave a deleted resource discoverable.
// Explicit name reuse must preserve that native conflict and retry only through
// the normal resource exchange after the service has released the name.
func TestResolveResourceReusePreservesNativeConflict(t *testing.T) {
	store := openResourceTestStore(t)
	old := testNativeResource(t, "reusable-api")
	replacement := testNativeResource(t, old.Slug)
	var requests []qurl.NativeConnectorResourceRequest
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, request *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		requests = append(requests, *request)
		switch len(requests) {
		case 1:
			return &qurl.ConnectorResourceResolution{Resource: old}, nil
		case 2:
			return nil, qurl.ErrConnectorResourceIdentityConflict
		default:
			return &qurl.ConnectorResourceResolution{Resource: replacement}, nil
		}
	})
	ctx := context.Background()
	if _, err := ResolveResourceWithResult(ctx, &qurl.AgentRuntimeBinding{}, store, old.Slug); err != nil {
		t.Fatal(err)
	}
	if retired, err := store.RetireConnectorResource(ctx, old.CRID); err != nil || !retired {
		t.Fatalf("retire old resource = %t, %v", retired, err)
	}
	if err := store.PrepareConnectorResourceReuse(ctx, old.Slug); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveResourceWithResult(ctx, &qurl.AgentRuntimeBinding{}, store, old.Slug); !errors.Is(err, qurl.ErrConnectorResourceIdentityConflict) {
		t.Fatalf("reuse with retained revoked slug = %v, want native identity conflict", err)
	}
	if len(requests) != 2 || pendingRequestFromDisk(t, store, old.Slug) != nil {
		t.Fatal("terminal conflict retried or retained the rejected request")
	}
	if _, _, found, err := store.ConnectorResourceBinding(ctx, old.Slug); err != nil || found {
		t.Fatalf("native conflict committed a replacement binding: found=%t err=%v", found, err)
	}
	if err := store.PrepareConnectorResourceReuse(ctx, old.Slug); err != nil {
		t.Fatal(err)
	}
	result, err := ResolveResourceWithResult(ctx, &qurl.AgentRuntimeBinding{}, store, old.Slug)
	if err != nil || result.Resource.ResourceID != replacement.ResourceID {
		t.Fatalf("retry after service slug release = %+v, %v", result, err)
	}
	if requests[1].ExpectedResourceID != "" || requests[2].ExpectedResourceID != "" ||
		requests[1].RequestNonce == requests[0].RequestNonce || requests[2].RequestNonce == requests[1].RequestNonce {
		t.Fatal("reuse retained a deleted identity or replayed a terminal request")
	}
}

func TestResolveResourcePendingPolicy(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantPending bool
	}{
		{name: "transport", err: errors.New("UDP timeout"), wantPending: true},
		{name: "unavailable", err: qurl.ErrConnectorResourceUnavailable, wantPending: true},
		{name: "rate limited", err: qurl.ErrConnectorResourceRateLimited, wantPending: true},
		{name: "invalid response", err: qurl.ErrInvalidNativeConnectorResourceResponse, wantPending: true},
		{name: "invalid local request", err: qurl.ErrInvalidNativeConnectorResourceRequest, wantPending: true},
		{name: "identity rejected", err: qurl.ErrConnectorResourceIdentityRejected},
		{name: "entitlement", err: qurl.ErrConnectorResourceEntitlementDenied},
		{name: "identity conflict", err: qurl.ErrConnectorResourceIdentityConflict},
		{name: "quota", err: qurl.ErrConnectorResourceQuotaExceeded},
		{name: "invalid request", err: qurl.ErrConnectorResourceRequestRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openResourceTestStore(t)
			installResourceResolver(t, func(context.Context, *qurl.AgentRuntimeBinding, *qurl.NativeConnectorResourceRequest, ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
				return nil, test.err
			})
			_, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "policy-api")
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			gotPending := pendingRequestFromDisk(t, store, "policy-api") != nil
			if gotPending != test.wantPending {
				t.Fatalf("pending = %v, want %v", gotPending, test.wantPending)
			}
		})
	}
}

func TestResolveResourceLocalBindingContradictionsAreTerminal(t *testing.T) {
	tests := []struct {
		name       string
		resolution func(*testing.T) *qurl.ConnectorResourceResolution
	}{
		{
			name: "missing resolution",
			resolution: func(*testing.T) *qurl.ConnectorResourceResolution {
				return nil
			},
		},
		{
			name: "missing resource",
			resolution: func(*testing.T) *qurl.ConnectorResourceResolution {
				return &qurl.ConnectorResourceResolution{}
			},
		},
		{
			name: "response Connector ID mismatch",
			resolution: func(t *testing.T) *qurl.ConnectorResourceResolution {
				return &qurl.ConnectorResourceResolution{Resource: testNativeResource(t, "other-api")}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openResourceTestStore(t)
			var nonces []string
			installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, request *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
				nonces = append(nonces, request.RequestNonce)
				return test.resolution(t), nil
			})

			for attempt := 0; attempt < 2; attempt++ {
				_, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, store, "stable-api")
				if !errors.Is(err, state.ErrConnectorResourceVerification) {
					t.Fatalf("attempt %d error = %v, want local verification failure", attempt+1, err)
				}
				if pendingRequestFromDisk(t, store, "stable-api") != nil {
					t.Fatalf("attempt %d retained a locally rejected completed request", attempt+1)
				}
			}
			if len(nonces) != 2 || nonces[0] == nonces[1] {
				t.Fatalf("request nonces = %v, want a fresh request after the terminal local rejection", nonces)
			}
		})
	}
}

func TestResolveResourceRejectsNilRuntimeInputsBeforeStateMutation(t *testing.T) {
	store := openResourceTestStore(t)
	if _, err := ResolveResourceWithResult(context.Background(), nil, store, "nil-api"); err == nil {
		t.Fatal("nil binding = nil error")
	}
	if _, err := os.Lstat(filepath.Join(store.Dir(), state.ConnectorResourcesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nil binding state file error = %v, want not exist", err)
	}
	if _, err := ResolveResourceWithResult(context.Background(), &qurl.AgentRuntimeBinding{}, nil, "nil-api"); err == nil {
		t.Fatal("nil store = nil error")
	}
}

func TestKnockResourceID(t *testing.T) {
	t.Setenv(EnvKnockResourceID, "")
	if got, err := KnockResourceID(&qurl.ConnectorResource{KnockResourceID: "assigned-target"}); err != nil || got != "assigned-target" {
		t.Fatalf("assigned KnockResourceID = (%q, %v)", got, err)
	}
	t.Setenv(EnvKnockResourceID, " custom-target ")
	if got, err := KnockResourceID(nil); err != nil || got != "custom-target" {
		t.Fatalf("override KnockResourceID = (%q, %v)", got, err)
	}
	t.Setenv(EnvKnockResourceID, "")
	if _, err := KnockResourceID(nil); err == nil {
		t.Fatal("missing KnockResourceID = nil error")
	}
}
