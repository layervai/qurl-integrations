//go:build clisandbox

package agent

import (
	"context"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// ResolveResourceWithRequestObserver is the clisandbox observation seam for
// observing the exact request after the state store durably persists it and
// immediately before qurl-go dispatches it. Production builds do not expose
// this function; ordinary callers use ResolveResourceWithResult.
func ResolveResourceWithRequestObserver(
	ctx context.Context,
	binding *qurl.AgentRuntimeBinding,
	store *state.Store,
	connectorID string,
	observer func(qurl.NativeConnectorResourceRequest) error,
	udpOpts ...qurl.AgentRuntimeUDPOption,
) (*ResolvedResource, error) {
	return resolveResourceWithRequestObserver(ctx, binding, store, connectorID, observer, udpOpts...)
}
