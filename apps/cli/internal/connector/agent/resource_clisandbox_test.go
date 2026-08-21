//go:build clisandbox

package agent

import (
	"context"
	"errors"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestResolveResourceRequestObserverIsDurableAndPreDispatch(t *testing.T) {
	store := openResourceTestStore(t)
	resource := testNativeResource(t, "proof-api")
	dispatched := false
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		dispatched = true
		return &qurl.ConnectorResourceResolution{Resource: resource, FoundExisting: false}, nil
	})

	observed := false
	_, err := ResolveResourceWithRequestObserver(context.Background(), &qurl.AgentRuntimeBinding{}, store, "proof-api", func(request qurl.NativeConnectorResourceRequest) error {
		if dispatched {
			return errors.New("observer ran after dispatch")
		}
		pending := pendingRequestFromDisk(t, store, request.ConnectorID)
		if pending == nil || pending["request_nonce"] != request.RequestNonce {
			return errors.New("observer ran before the exact request was durable")
		}
		observed = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observed || !dispatched {
		t.Fatalf("observer/dispatched = %t/%t, want both true in that order", observed, dispatched)
	}
}

func TestResolveResourceRequestObserverCanStopBeforeDispatch(t *testing.T) {
	store := openResourceTestStore(t)
	dispatched := false
	installResourceResolver(t, func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ *qurl.NativeConnectorResourceRequest, _ ...qurl.AgentRuntimeUDPOption) (*qurl.ConnectorResourceResolution, error) {
		dispatched = true
		return nil, errors.New("unexpected dispatch")
	})
	sentinel := errors.New("observer stopped proof")
	_, err := ResolveResourceWithRequestObserver(context.Background(), &qurl.AgentRuntimeBinding{}, store, "proof-api", func(request qurl.NativeConnectorResourceRequest) error {
		if pendingRequestFromDisk(t, store, request.ConnectorID) == nil {
			t.Fatal("observer ran before pending state was durable")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("observer error = %v, want sentinel", err)
	}
	if dispatched {
		t.Fatal("observer failure still dispatched the request")
	}
	if pendingRequestFromDisk(t, store, "proof-api") == nil {
		t.Fatal("observer failure cleared the replayable pending request")
	}
}
