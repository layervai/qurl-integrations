package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// EnvKnockResourceID is an advanced custom-deployment override for the
// placement-neutral knock resource returned by the platform. The name is the
// qURL Connector operator contract shared with the standalone Connector.
const EnvKnockResourceID = "LAYERV_KNOCK_RESOURCE_ID"

// ResolvedResource carries the selected Connector resource plus the creation
// provenance authenticated by the assigned cell.
type ResolvedResource struct {
	Resource      *qurl.ConnectorResource
	FoundExisting *bool
}

// Test-only seam for qurl-go's direct assigned-cell LST. Production keeps this
// bound to the SDK and has no HTTP, Hub, relay, or cross-cell fallback.
var resolveRegisteredAgentConnectorResource = qurl.ResolveRegisteredAgentConnectorResource

// ResolveResourceWithResult resolves or creates one Connector resource over
// native NHP using the registered runtime's assigned cell. It persists the
// exact nonce and continuity assertion before dispatch, replays them after an
// uncertain outcome, and atomically commits the complete authenticated binding
// on success.
func ResolveResourceWithResult(
	ctx context.Context,
	binding *qurl.AgentRuntimeBinding,
	store *state.Store,
	connectorID string,
	udpOpts ...qurl.AgentRuntimeUDPOption,
) (resolved *ResolvedResource, retErr error) {
	return resolveResourceWithRequestObserver(ctx, binding, store, connectorID, nil, udpOpts...)
}

type connectorResourceRequestObserver func(qurl.NativeConnectorResourceRequest) error

func resolveResourceWithRequestObserver(
	ctx context.Context,
	binding *qurl.AgentRuntimeBinding,
	store *state.Store,
	connectorID string,
	observer connectorResourceRequestObserver,
	udpOpts ...qurl.AgentRuntimeUDPOption,
) (resolved *ResolvedResource, retErr error) {
	if binding == nil {
		return nil, errors.New("registered device runtime binding is nil")
	}
	if store == nil {
		return nil, errors.New("registered device state store is nil")
	}
	tx, err := store.BeginConnectorResource(ctx, connectorID)
	if err != nil {
		return nil, fmt.Errorf("connector %q: prepare native resource request: %w", connectorID, err)
	}
	defer func() {
		if closeErr := tx.Close(); closeErr != nil {
			// Do not report a usable result when transaction ownership could not
			// be released cleanly. Commit already made a successful binding
			// durable, so the next invocation warm-continues from that identity.
			resolved = nil
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	request := tx.Request()
	if observer != nil {
		if err := observer(*request); err != nil {
			return nil, fmt.Errorf("connector %q: observe durable native resource request before dispatch: %w", connectorID, err)
		}
	}
	resolution, err := resolveRegisteredAgentConnectorResource(ctx, binding, request, udpOpts...)
	if err != nil {
		if terminalConnectorResourceDenial(err) {
			if clearErr := tx.ClearPending(); clearErr != nil {
				return nil, errors.Join(
					fmt.Errorf("connector %q: assigned cell rejected resource request: %w", connectorID, err),
					clearErr,
				)
			}
		}
		return nil, fmt.Errorf("connector %q: resolve resource through assigned NHP cell: %w", connectorID, err)
	}
	if resolution == nil || resolution.Resource == nil {
		// A successful SDK return is an authenticated completed response. Route
		// an absent binding through Commit's terminal local-verification policy
		// so this exact nonce cannot become an unrecoverable replay loop. This is
		// deliberately VerificationFailed rather than the retryable SDK invalid-
		// response taxonomy: success means the SDK completed authentication, and
		// Commit atomically discards the completed request while accepting no
		// identity.
		return nil, fmt.Errorf("connector %q: verify authenticated resource binding: %w", connectorID, tx.Commit(nil))
	}
	resource := resolution.Resource
	if err := tx.Commit(&state.ConnectorResourceBinding{
		ConnectorID:        resource.Slug,
		ResourceID:         resource.ResourceID,
		CRID:               resource.CRID,
		ConnectorRoutingID: resource.ConnectorRoutingID,
		KnockResourceID:    resource.KnockResourceID,
	}); err != nil {
		return nil, fmt.Errorf("connector %q: verify and persist authenticated resource binding: %w", connectorID, err)
	}
	found := resolution.FoundExisting
	return &ResolvedResource{Resource: resource, FoundExisting: &found}, nil
}

// terminalConnectorResourceDenial is the closed set of authenticated outcomes
// that prove no mutation occurred. Transport failures, cancellations,
// unavailable/rate-limited results, and malformed authenticated replies keep
// the pending request for exact replay. qurl-go guarantees an invalid native
// request is rejected before DNS, socket creation, or packet construction; it
// also keeps pending so repairing the local context or runtime binding retries
// the exact logical request instead of silently replacing its nonce.
func terminalConnectorResourceDenial(err error) bool {
	return errors.Is(err, qurl.ErrConnectorResourceIdentityRejected) ||
		errors.Is(err, qurl.ErrConnectorResourceEntitlementDenied) ||
		errors.Is(err, qurl.ErrConnectorResourceIdentityConflict) ||
		errors.Is(err, qurl.ErrConnectorResourceQuotaExceeded) ||
		errors.Is(err, qurl.ErrConnectorResourceRequestRejected)
}

// KnockResourceID resolves the NHP knock operand for a resolved resource: the
// LAYERV_KNOCK_RESOURCE_ID override when set (trimmed), else the
// platform-assigned knock resource. An empty result is a configuration error
// the caller must fail closed on.
func KnockResourceID(resource *qurl.ConnectorResource) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv(EnvKnockResourceID)); explicit != "" {
		return explicit, nil
	}
	if resource == nil || strings.TrimSpace(resource.KnockResourceID) == "" {
		return "", errors.New("no NHP knock resource: the platform did not assign one and " + EnvKnockResourceID + " is not set")
	}
	return resource.KnockResourceID, nil
}
