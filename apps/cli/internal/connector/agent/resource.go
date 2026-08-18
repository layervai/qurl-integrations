package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"
)

// EnvKnockResourceID is an advanced custom-deployment override for the
// placement-neutral knock resource returned by the platform. The name is the
// qURL Connector operator contract shared with the standalone Connector.
const EnvKnockResourceID = "LAYERV_KNOCK_RESOURCE_ID"

// ResolveResource resolves slug to its active qURL Connector resource using
// the registered device's resource client: read-by-slug first, ensure on
// not-found, and a read-back reconcile when the ensure outcome is uncertain.
//
// The ensure leg is a mutation whose outcome can be unknown (the request may
// have committed server-side after a transport failure). An authoritative
// platform rejection (a 4xx that is not outcome-ambiguous) is surfaced
// directly; any other ensure failure triggers one read-back by slug so an
// already-committed row is adopted instead of a second mutation being fired.
// When that reconcile still finds nothing, the uncertainty is surfaced to the
// operator rather than retried: a second blind ensure could double-provision.
func ResolveResource(ctx context.Context, client *qurl.Client, slug string) (*qurl.ConnectorResource, error) {
	if client == nil {
		return nil, errors.New("registered device resource client is nil")
	}
	resource, err := client.GetConnectorResourceBySlug(ctx, slug)
	if err == nil {
		return resource, nil
	}
	if errors.Is(err, qurl.ErrAgentStateContinuity) {
		return nil, fmt.Errorf("connector %q: state continuity lost while reading by slug: %w", slug, err)
	}
	if !errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return nil, fmt.Errorf("connector %q: read existing identity by slug before ensure: %w", slug, err)
	}

	result, ensureErr := client.EnsureConnectorResource(ctx, slug)
	if ensureErr == nil {
		if result == nil || result.Resource == nil {
			return nil, fmt.Errorf("connector %q: qURL API returned no resource after ensure", slug)
		}
		return result.Resource, nil
	}
	if errors.Is(ensureErr, qurl.ErrAgentStateContinuity) {
		return nil, fmt.Errorf("connector %q: state continuity lost during ensure: %w", slug, ensureErr)
	}
	if authoritativeEnsureRejection(ensureErr) {
		return nil, fmt.Errorf("connector %q: %w", slug, ensureErr)
	}
	resource, err = client.GetConnectorResourceBySlug(ctx, slug)
	if err == nil {
		return resource, nil
	}
	if errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return nil, fmt.Errorf("connector %q: ensure outcome is uncertain and no active resource is visible by slug; not retrying automatically (a second ensure could double-provision): %w", slug, ensureErr)
	}
	return nil, errors.Join(
		fmt.Errorf("connector %q: ensure outcome is uncertain: %w", slug, ensureErr),
		fmt.Errorf("connector %q: reconcile read after uncertain ensure: %w", slug, err),
	)
}

// authoritativeEnsureRejection reports whether an ensure failure is a
// definitive platform rejection rather than an ambiguous outcome: a 4xx API
// error that is not marked outcome-unknown. Definitive rejections must not
// trigger the read-back adoption path — the platform said no.
func authoritativeEnsureRejection(err error) bool {
	if errors.Is(err, qurl.ErrConnectorResourceOutcomeUnknown) {
		return false
	}
	var apiErr *qurl.APIError
	return errors.As(err, &apiErr) &&
		apiErr.StatusCode >= http.StatusBadRequest &&
		apiErr.StatusCode < http.StatusInternalServerError
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
