package state

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

var testRoutingEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func testResourceBinding(t *testing.T, connectorID string) ConnectorResourceBinding {
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
	return ConnectorResourceBinding{
		ConnectorID:        connectorID,
		ResourceID:         base64.RawURLEncoding.EncodeToString(der),
		ConnectorRoutingID: "c-" + testRoutingEncoding.EncodeToString(digest[:]),
		KnockResourceID:    "nhp-target-" + connectorID,
	}
}

func testBindingCRID(t *testing.T, binding *ConnectorResourceBinding, version byte) string {
	t.Helper()
	der, err := base64.RawURLEncoding.DecodeString(binding.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return apitest.DeriveCRID(t, der, version)
}

func TestConnectorResourceTransactionPersistsExactRequestAndWarmContinuity(t *testing.T) {
	store := openTestStore(t)
	tx, err := store.BeginConnectorResource(context.Background(), "billing-api")
	if err != nil {
		t.Fatal(err)
	}
	first := *tx.Request()
	if first.ExpectedResourceID != "" || first.RequestNonce == "" {
		t.Fatalf("fresh request = %+v", first)
	}
	info, err := os.Lstat(filepath.Join(store.Dir(), ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	if (!isWindows(t) && info.Mode().Perm() != connectorResourceFileMode) || !info.Mode().IsRegular() {
		t.Fatalf("state mode = %v, want regular 0600", info.Mode())
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}

	replay, err := store.BeginConnectorResource(context.Background(), "billing-api")
	if err != nil {
		t.Fatal(err)
	}
	if got := *replay.Request(); got != first {
		t.Fatalf("replayed request = %+v, want %+v", got, first)
	}
	binding := testResourceBinding(t, "billing-api")
	if err := replay.Commit(&binding); err != nil {
		t.Fatal(err)
	}
	if err := replay.Close(); err != nil {
		t.Fatal(err)
	}

	warm, err := store.BeginConnectorResource(context.Background(), "billing-api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = warm.Close() }()
	warmRequest := warm.Request()
	if warmRequest.RequestNonce == first.RequestNonce {
		t.Fatal("warm request reused a completed nonce")
	}
	if warmRequest.ExpectedResourceID != binding.ResourceID {
		t.Fatalf("warm expected resource = %q, want %q", warmRequest.ExpectedResourceID, binding.ResourceID)
	}
}

func TestRetireConnectorResourcePreservesIdentityAndBlocksReuse(t *testing.T) {
	store := openTestStore(t)
	binding := testResourceBinding(t, "deleted-api")
	binding.CRID = testBindingCRID(t, &binding, 1)
	commitTestBinding(t, store, &binding)

	retired, err := store.RetireConnectorResource(context.Background(), binding.CRID)
	if err != nil || !retired {
		t.Fatalf("RetireConnectorResource() = %t, %v, want true", retired, err)
	}
	got, gotRetired, found, err := store.ConnectorResourceBinding(context.Background(), binding.ConnectorID)
	if err != nil || !found || !gotRetired || got != binding {
		t.Fatalf("ConnectorResourceBinding() = %+v retired=%t found=%t err=%v", got, gotRetired, found, err)
	}
	if _, err := store.BeginConnectorResource(context.Background(), binding.ConnectorID); !errors.Is(err, ErrConnectorResourceRetired) {
		t.Fatalf("BeginConnectorResource() = %v, want retired error", err)
	}
	retired, err = store.RetireConnectorResource(context.Background(), binding.ResourceID)
	if err != nil || !retired {
		t.Fatalf("idempotent RetireConnectorResource() = %t, %v", retired, err)
	}
	retired, err = store.RetireConnectorResource(context.Background(), "unknown-public-id")
	if err != nil || retired {
		t.Fatalf("unknown RetireConnectorResource() = %t, %v, want false", retired, err)
	}
}

func TestConnectorResourceStateSupportsIndependentConnectorIDs(t *testing.T) {
	store := openTestStore(t)
	first, err := store.BeginConnectorResource(context.Background(), "billing-api")
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "billing-api")
	if err := first.Commit(&binding); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginConnectorResource(context.Background(), "orders-api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	loaded, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Bindings) != 1 || len(loaded.Pending) != 1 || loaded.Pending["orders-api"].ExpectedResourceID != "" {
		t.Fatalf("multi-ID state = %+v", loaded)
	}
}

func TestConnectorResourceTransactionLockIsCrossHandleAndContextBounded(t *testing.T) {
	store := openTestStore(t)
	first, err := store.BeginConnectorResource(context.Background(), "locked-api")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.BeginConnectorResource(ctx, "locked-api")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contending transaction = %v, want deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("contending Connector resource transaction ignored context")
	}
	request := *first.Request()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginConnectorResource(context.Background(), "locked-api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if got := *next.Request(); got != request {
		t.Fatalf("post-lock request = %+v, want exact pending %+v", got, request)
	}
}

func TestConnectorResourceLockRejectsUnsafeEntries(t *testing.T) {
	t.Run("wrong permissions", func(t *testing.T) {
		if isWindows(t) {
			t.Skip("POSIX mode rejection is a Unix contract")
		}
		store := openTestStore(t)
		path := filepath.Join(store.Dir(), connectorResourcesLock)
		if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // intentionally unsafe mode exercises rejection.
			t.Fatal(err)
		}
		if _, err := store.BeginConnectorResource(context.Background(), "locked-api"); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("wrong-mode lock error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		store := openTestStore(t)
		target := filepath.Join(t.TempDir(), "target-lock")
		if err := os.WriteFile(target, nil, connectorResourceFileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(store.Dir(), connectorResourcesLock)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.BeginConnectorResource(context.Background(), "locked-api"); err == nil {
			t.Fatal("symlink lock = nil error")
		}
	})
}

func TestConnectorResourceStateRejectsCorruptionAndUnsafeEntries(t *testing.T) {
	validBinding := testResourceBinding(t, "safe-api")
	validNonce := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `"}}}`
	validBindingState := emptyConnectorResourcesState()
	validBindingState.Bindings[validBinding.ConnectorID] = validBinding
	validBindingJSON, err := encodeConnectorResources(validBindingState)
	if err != nil {
		t.Fatal(err)
	}
	invalidUTF8 := bytes.Replace(validBindingJSON, []byte(validBinding.KnockResourceID), []byte("nhp-target-\xff"), 1)
	loneHighSurrogate := bytes.Replace(validBindingJSON, []byte(validBinding.KnockResourceID), []byte(`nhp-target-\ud800`), 1)
	loneLowSurrogate := bytes.Replace(validBindingJSON, []byte(validBinding.KnockResourceID), []byte(`nhp-target-\udc00`), 1)
	brokenSurrogatePair := bytes.Replace(validBindingJSON, []byte(validBinding.KnockResourceID), []byte(`nhp-target-\ud800\u0041`), 1)
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown top field", data: `{"version":1,"bindings":{},"pending":{},"extra":true}`},
		{name: "noncanonical top field casing", data: `{"Version":1,"bindings":{},"pending":{}}`},
		{name: "unknown nested field", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `","extra":true}}}`},
		{name: "noncanonical nested field casing", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"Connector_ID":"safe-api","request_nonce":"` + validNonce + `"}}}`},
		{name: "duplicate", data: `{"version":1,"version":1,"bindings":{},"pending":{}}`},
		{name: "duplicate nested", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","connector_id":"safe-api","request_nonce":"` + validNonce + `"}}}`},
		{name: "unsupported version", data: `{"version":2,"bindings":{},"pending":{}}`},
		{name: "missing map", data: `{"version":1,"bindings":{}}`},
		{name: "null map", data: `{"version":1,"bindings":null,"pending":{}}`},
		{name: "null optional expected identity", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `","expected_resource_id":null}}}`},
		{name: "empty optional expected identity", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `","expected_resource_id":""}}}`},
		{name: "null optional crid", data: `{"version":1,"bindings":{"safe-api":{"connector_id":"safe-api","resource_id":"` + validBinding.ResourceID + `","connector_routing_id":"` + validBinding.ConnectorRoutingID + `","knock_resource_id":"nhp-target-safe-api","crid":null}},"pending":{}}`},
		{name: "empty optional crid", data: `{"version":1,"bindings":{"safe-api":{"connector_id":"safe-api","resource_id":"` + validBinding.ResourceID + `","connector_routing_id":"` + validBinding.ConnectorRoutingID + `","knock_resource_id":"nhp-target-safe-api","crid":""}},"pending":{}}`},
		{name: "excessive nesting", data: `{"version":1,"bindings":[[[[[[[[[[]]]]]]]]]],"pending":{}}`},
		{name: "invalid raw UTF-8", data: string(invalidUTF8)},
		{name: "lone high surrogate", data: string(loneHighSurrogate)},
		{name: "lone low surrogate", data: string(loneLowSurrogate)},
		{name: "broken surrogate pair", data: string(brokenSurrogatePair)},
		{name: "bad nonce", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"bad"}}}`},
		{name: "map key mismatch", data: `{"version":1,"bindings":{},"pending":{"wrong-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `"}}}`},
		{name: "expected without binding", data: `{"version":1,"bindings":{},"pending":{"safe-api":{"connector_id":"safe-api","request_nonce":"` + validNonce + `","expected_resource_id":"` + validBinding.ResourceID + `"}}}`},
		{name: "trailing value", data: valid + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			path := filepath.Join(store.Dir(), ConnectorResourcesFile)
			if err := os.WriteFile(path, []byte(test.data), connectorResourceFileMode); err != nil {
				t.Fatal(err)
			}
			secureConnectorStateFixtureFile(t, path)
			if _, err := store.BeginConnectorResource(context.Background(), "safe-api"); err == nil || !strings.Contains(err.Error(), "invalid Connector resource state") {
				t.Fatalf("corrupt state error = %v", err)
			}
		})
	}

	t.Run("wrong permissions", func(t *testing.T) {
		if isWindows(t) {
			t.Skip("POSIX mode rejection is a Unix contract")
		}
		store := openTestStore(t)
		path := filepath.Join(store.Dir(), ConnectorResourcesFile)
		if err := os.WriteFile(path, []byte(valid), 0o644); err != nil { //nolint:gosec // intentionally unsafe mode proves the fail-closed read.
			t.Fatal(err)
		}
		if _, err := store.BeginConnectorResource(context.Background(), "safe-api"); err == nil || !strings.Contains(err.Error(), "mode 0644") {
			t.Fatalf("wrong-mode error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		store := openTestStore(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte(valid), connectorResourceFileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(store.Dir(), ConnectorResourcesFile)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.BeginConnectorResource(context.Background(), "safe-api"); err == nil || (!isWindows(t) && !strings.Contains(err.Error(), "non-symlink")) {
			t.Fatalf("symlink error = %v", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		store := openTestStore(t)
		path := filepath.Join(store.Dir(), ConnectorResourcesFile)
		if err := os.WriteFile(path, make([]byte, connectorResourcesMaxBytes+1), connectorResourceFileMode); err != nil {
			t.Fatal(err)
		}
		secureConnectorStateFixtureFile(t, path)
		if _, err := store.BeginConnectorResource(context.Background(), "safe-api"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize error = %v", err)
		}
	})
}

func TestConnectorResourceStateAcceptsPairedJSONSurrogate(t *testing.T) {
	binding := testResourceBinding(t, "unicode-api")
	state := emptyConnectorResourcesState()
	state.Bindings[binding.ConnectorID] = binding
	data, err := encodeConnectorResources(state)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(binding.KnockResourceID), []byte(`nhp-target-\ud83d\ude80`), 1)
	decoded, err := decodeConnectorResources(data)
	if err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
	if got := decoded.Bindings[binding.ConnectorID].KnockResourceID; got != "nhp-target-🚀" {
		t.Fatalf("decoded knock resource ID = %q, want paired Unicode scalar", got)
	}
}

func TestConnectorResourceKnockIDWireBound(t *testing.T) {
	binding := testResourceBinding(t, "bounded-api")
	binding.KnockResourceID = strings.Repeat("k", 64)
	if err := validateBinding(&binding); err != nil {
		t.Fatalf("64-byte knock resource ID = %v, want accepted", err)
	}
	binding.KnockResourceID += "k"
	if err := validateBinding(&binding); err == nil || !strings.Contains(err.Error(), "knock resource ID") {
		t.Fatalf("65-byte knock resource ID error = %v, want canonical rejection", err)
	}

	// The limit is measured in UTF-8 bytes, matching the public LRT contract.
	binding.KnockResourceID = strings.Repeat("é", 32)
	if err := validateBinding(&binding); err != nil {
		t.Fatalf("64-byte UTF-8 knock resource ID = %v, want accepted", err)
	}
	binding.KnockResourceID += "é"
	if err := validateBinding(&binding); err == nil {
		t.Fatal("66-byte UTF-8 knock resource ID = nil error")
	}
}

func TestConnectorResourceCommitCRIDEnrichmentAndOmission(t *testing.T) {
	t.Run("CRID enrichment", func(t *testing.T) {
		store := openTestStore(t)
		binding := testResourceBinding(t, "stable-api")
		commitTestBinding(t, store, &binding)

		binding.CRID = testBindingCRID(t, &binding, apitest.VersionProduction)
		commitTestBinding(t, store, &binding)
		loaded, err := loadConnectorResources(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if got := loaded.Bindings[binding.ConnectorID].CRID; got != binding.CRID {
			t.Fatalf("enriched CRID = %q, want %q", got, binding.CRID)
		}
	})

	t.Run("omitted CRID is preserved", func(t *testing.T) {
		store := openTestStore(t)
		binding := testResourceBinding(t, "stable-api")
		binding.CRID = testBindingCRID(t, &binding, apitest.VersionProduction)
		commitTestBinding(t, store, &binding)

		omitted := binding
		omitted.CRID = ""
		commitTestBinding(t, store, &omitted)
		loaded, err := loadConnectorResources(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if got := loaded.Bindings[binding.ConnectorID].CRID; got != binding.CRID {
			t.Fatalf("CRID after omission = %q, want preserved %q", got, binding.CRID)
		}
	})
}

func TestConnectorResourceCommitContradictionsAreTypedTerminalAcrossRestart(t *testing.T) {
	type preparedCase struct {
		connectorID     string
		response        *ConnectorResourceBinding
		expectedBinding map[string]ConnectorResourceBinding
		expectedID      string
	}
	cases := []struct {
		name    string
		kind    error
		detail  string
		prepare func(*testing.T, *Store) preparedCase
	}{
		{
			name:   "missing binding",
			kind:   ErrConnectorResourceVerification,
			detail: "authenticated response has no binding",
			prepare: func(_ *testing.T, _ *Store) preparedCase {
				return preparedCase{connectorID: "stable-api", expectedBinding: map[string]ConnectorResourceBinding{}}
			},
		},
		{
			name:   "response Connector ID mismatch",
			kind:   ErrConnectorResourceVerification,
			detail: "response Connector ID does not match the durable request",
			prepare: func(t *testing.T, _ *Store) preparedCase {
				wrong := testResourceBinding(t, "other-api")
				return preparedCase{connectorID: "stable-api", response: &wrong, expectedBinding: map[string]ConnectorResourceBinding{}}
			},
		},
		{
			name:   "expected resource mismatch",
			kind:   ErrConnectorResourceVerification,
			detail: "response identity does not match the continuity assertion",
			prepare: func(t *testing.T, store *Store) preparedCase {
				original := testResourceBinding(t, "stable-api")
				commitTestBinding(t, store, &original)
				changed := testResourceBinding(t, "replacement-api")
				changed.ConnectorID = original.ConnectorID
				return preparedCase{
					connectorID: original.ConnectorID, response: &changed,
					expectedBinding: map[string]ConnectorResourceBinding{original.ConnectorID: original},
					expectedID:      original.ResourceID,
				}
			},
		},
		{
			name:   "invalid complete binding",
			kind:   ErrConnectorResourceVerification,
			detail: "response binding is invalid",
			prepare: func(t *testing.T, _ *Store) preparedCase {
				invalid := testResourceBinding(t, "stable-api")
				invalid.KnockResourceID = ""
				return preparedCase{connectorID: invalid.ConnectorID, response: &invalid, expectedBinding: map[string]ConnectorResourceBinding{}}
			},
		},
		{
			name:   "warm routing identity change",
			kind:   ErrConnectorResourceVerification,
			detail: "changed the cached routing or knock binding",
			prepare: func(t *testing.T, store *Store) preparedCase {
				original := testResourceBinding(t, "stable-api")
				commitTestBinding(t, store, &original)
				changed := original
				changed.ConnectorRoutingID = testResourceBinding(t, "other-api").ConnectorRoutingID
				return preparedCase{
					connectorID: original.ConnectorID, response: &changed,
					expectedBinding: map[string]ConnectorResourceBinding{original.ConnectorID: original},
					expectedID:      original.ResourceID,
				}
			},
		},
		{
			name:   "warm knock target change",
			kind:   ErrConnectorResourceVerification,
			detail: "changed the cached routing or knock binding",
			prepare: func(t *testing.T, store *Store) preparedCase {
				original := testResourceBinding(t, "stable-api")
				commitTestBinding(t, store, &original)
				changed := original
				changed.KnockResourceID = "different-nhp-target"
				return preparedCase{
					connectorID: original.ConnectorID, response: &changed,
					expectedBinding: map[string]ConnectorResourceBinding{original.ConnectorID: original},
					expectedID:      original.ResourceID,
				}
			},
		},
		{
			name:   "warm CRID change",
			kind:   ErrConnectorResourceVerification,
			detail: "changed the cached CRID",
			prepare: func(t *testing.T, store *Store) preparedCase {
				original := testResourceBinding(t, "stable-api")
				original.CRID = testBindingCRID(t, &original, apitest.VersionProduction)
				commitTestBinding(t, store, &original)
				changed := original
				changed.CRID = testBindingCRID(t, &changed, apitest.VersionTest)
				return preparedCase{
					connectorID: original.ConnectorID, response: &changed,
					expectedBinding: map[string]ConnectorResourceBinding{original.ConnectorID: original},
					expectedID:      original.ResourceID,
				}
			},
		},
		{
			name:   "cross-Connector resource identity alias",
			kind:   ErrConnectorResourceStateConflict,
			detail: "resource identity is already bound",
			prepare: func(t *testing.T, store *Store) preparedCase {
				first := testResourceBinding(t, "billing-api")
				second := testResourceBinding(t, "orders-api")
				commitTestBinding(t, store, &first)
				second.ResourceID = first.ResourceID
				return preparedCase{
					connectorID: second.ConnectorID, response: &second,
					expectedBinding: map[string]ConnectorResourceBinding{first.ConnectorID: first},
				}
			},
		},
		{
			name:   "cross-Connector routing identity alias",
			kind:   ErrConnectorResourceStateConflict,
			detail: "routing identity is already bound",
			prepare: func(t *testing.T, store *Store) preparedCase {
				first := testResourceBinding(t, "billing-api")
				second := testResourceBinding(t, "orders-api")
				commitTestBinding(t, store, &first)
				second.ConnectorRoutingID = first.ConnectorRoutingID
				return preparedCase{
					connectorID: second.ConnectorID, response: &second,
					expectedBinding: map[string]ConnectorResourceBinding{first.ConnectorID: first},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := secureStateTestDir(t)
			store := openTestStoreAt(t, dir)
			prepared := tc.prepare(t, store)
			tx, err := store.BeginConnectorResource(context.Background(), prepared.connectorID)
			if err != nil {
				t.Fatal(err)
			}
			originalRequest := *tx.Request()
			err = tx.Commit(prepared.response)
			if err == nil || !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("contradictory commit error = %v, want kind %v and detail %q", err, tc.kind, tc.detail)
			}
			var typed *ConnectorResourceCommitError
			if !errors.As(err, &typed) {
				t.Fatalf("contradictory commit error type = %T, want *ConnectorResourceCommitError", err)
			}
			if tx.Request() != nil {
				t.Fatal("terminal contradictory commit left transaction request available")
			}
			if err := tx.Close(); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadConnectorResources(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := loaded.Pending[prepared.connectorID]; ok {
				t.Fatal("terminal contradictory commit retained the exact pending request")
			}
			if len(loaded.Bindings) != len(prepared.expectedBinding) {
				t.Fatalf("binding count after contradictory commit = %d, want %d", len(loaded.Bindings), len(prepared.expectedBinding))
			}
			for connectorID, want := range prepared.expectedBinding {
				if got := loaded.Bindings[connectorID]; got != want {
					t.Fatalf("binding %q after contradictory commit = %+v, want %+v", connectorID, got, want)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			reopened := openTestStoreAt(t, dir)
			defer func() { _ = reopened.Close() }()
			fresh, err := reopened.BeginConnectorResource(context.Background(), prepared.connectorID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fresh.Close() }()
			freshRequest := fresh.Request()
			if freshRequest == nil || freshRequest.RequestNonce == originalRequest.RequestNonce {
				t.Fatalf("request after restart = %+v, want a fresh nonce after terminal contradiction", freshRequest)
			}
			if freshRequest.ExpectedResourceID != prepared.expectedID {
				t.Fatalf("expected resource after restart = %q, want %q", freshRequest.ExpectedResourceID, prepared.expectedID)
			}
		})
	}
}

func TestConnectorResourceCommitContradictionReportsAndRecoversFromDiscardFailure(t *testing.T) {
	if isWindows(t) {
		t.Skip("POSIX read-only mode injection is a Unix contract")
	}
	store := openTestStore(t)
	tx, err := store.BeginConnectorResource(context.Background(), "stable-api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Close() }()
	originalRequest := *tx.Request()
	path := filepath.Join(store.Dir(), ConnectorResourcesFile)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	wrong := testResourceBinding(t, "other-api")
	err = tx.Commit(&wrong)
	if !errors.Is(err, ErrConnectorResourceVerification) || !strings.Contains(err.Error(), "could not discard") {
		t.Fatalf("discard failure error = %v, want typed verification failure with repair guidance", err)
	}
	if tx.Request() == nil {
		t.Fatal("failed durable discard incorrectly finished the transaction")
	}
	if err := os.Chmod(path, connectorResourceFileMode); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Pending["stable-api"]; got.RequestNonce != originalRequest.RequestNonce {
		t.Fatalf("pending request after failed discard = %+v, want original nonce %q", got, originalRequest.RequestNonce)
	}

	err = tx.Commit(&wrong)
	if !errors.Is(err, ErrConnectorResourceVerification) || !strings.Contains(err.Error(), "discarded the contradictory completed request") {
		t.Fatalf("repaired discard error = %v, want typed terminal verification failure", err)
	}
	loaded, err = loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Pending["stable-api"]; ok {
		t.Fatal("repaired discard retained the contradictory completed request")
	}
}

func TestConnectorResourceCommitAllowsSharedKnockTarget(t *testing.T) {
	t.Run("shared knock target", func(t *testing.T) {
		store := openTestStore(t)
		first := testResourceBinding(t, "billing-api")
		second := testResourceBinding(t, "orders-api")
		first.KnockResourceID = "shared-cell-target"
		second.KnockResourceID = first.KnockResourceID
		commitTestBinding(t, store, &first)
		commitTestBinding(t, store, &second)

		loaded, err := loadConnectorResources(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Bindings) != 2 || loaded.Bindings[first.ConnectorID] != first || loaded.Bindings[second.ConnectorID] != second {
			t.Fatalf("shared-target bindings = %+v", loaded.Bindings)
		}
	})
}

func commitTestBinding(t *testing.T, store *Store, binding *ConnectorResourceBinding) {
	t.Helper()
	tx, err := store.BeginConnectorResource(context.Background(), binding.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(binding); err != nil {
		_ = tx.Close()
		t.Fatal(err)
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
}
