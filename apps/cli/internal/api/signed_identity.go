package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/layervai/qurl-go/qurl"
)

// ErrSignedIdentityUnsupported refuses an older service that does not report the setting.
var ErrSignedIdentityUnsupported = &resourceSettingUnconfirmedError{message: "this service does not report signed identity support"}

// SetSignedIdentity changes only the owner's saved identity requirement, without replay.
//
// TODO(upstream-contract): PATCH signed_identity returns 200 with a flat data resource
// row and the confirmed boolean. GET returns it under data.resource. Keep these
// fields, issuer/JWKS profile and README verification limits in sync with qurl-service.
func (c *client) SetSignedIdentity(ctx context.Context, id string, enabled bool) (*ResourceSummary, error) {
	id, path, err := deviceGrantsPath(id)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, struct {
		Enabled bool `json:"signed_identity"`
	}{enabled})
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var env struct {
		Data resourceRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode signed identity change: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingIdentity(id, &sharingRow{CRID: env.Data.CRID, ResourceID: env.Data.ResourceID}); err != nil {
		return nil, err
	}
	if env.Data.SignedIdentity == nil || *env.Data.SignedIdentity != enabled {
		return nil, &resourceSettingUnconfirmedError{message: "the service did not confirm the signed identity setting; run qurl identity status " + id}
	}
	return summarizeResourceRow(&env.Data, "signed identity change")
}
