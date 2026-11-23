package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/layervai/qurl-go/qurl"
)

// CreditContractID names the default contract used when a publisher sets a credit price.
const CreditContractID = "crid-credits"

const consumerContractMode = "contract"

// consumerPolicyWire adds the service's credit amount to the pinned SDK policy.
// TODO(upstream-contract): Remove this extension once qurl-go models policy credits.
type consumerPolicyWire struct {
	qurl.ConsumerAccessPolicy
	Credits int64 `json:"credits,omitempty"`
}

func (w *consumerPolicyWire) policy() *qurl.ConsumerAccessPolicy {
	if w == nil {
		return nil
	}
	return &w.ConsumerAccessPolicy
}

func (w *consumerPolicyWire) price() int64 {
	if w == nil {
		return 0
	}
	return w.Credits
}

// ValidCreditPrice checks the whole-credit range accepted by qurl-service.
// TODO(upstream-contract): Keep this range in sync with ConsumerAccessPolicy.credits.
func ValidCreditPrice(price int64) bool {
	return price >= 1 && price <= 10
}

// SetCreditPrice issues one policy update and checks that the returned resource
// has the requested price. A policy version fences concurrent changes.
func (c *client) SetCreditPrice(ctx context.Context, crid string, credits, version int64) (*ConsumerManagementResult, error) {
	id, path, err := deviceGrantsPath(crid)
	if err != nil {
		return nil, err
	}
	if !ValidCreditPrice(credits) || version < -1 || version >= 1<<53-1 {
		return nil, fmt.Errorf("%w: invalid credit price or policy version", qurl.ErrInvalidResourceRequest)
	}
	current, err := c.Resource(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Private == nil || !*current.Private {
		return nil, fmt.Errorf("%w: credit pricing requires a private resource", qurl.ErrInvalidResourceRequest)
	}
	if current.ConsumerAccess != nil && current.ConsumerAccess.Payment != nil {
		return nil, fmt.Errorf("%w: this CRID has a Stripe payment policy", qurl.ErrInvalidResourceRequest)
	}
	if version == -1 {
		version = current.ConsumerAccessVersion
		if current.CreditPrice == credits && current.ConsumerAccess != nil && current.ConsumerAccess.Version == version {
			return &ConsumerManagementResult{Action: "configure", CRID: current.CRID, Policy: current.ConsumerAccess, CreditPrice: credits}, nil
		}
	}
	policy := consumerPolicyWire{
		ConsumerAccessPolicy: qurl.ConsumerAccessPolicy{Mode: consumerContractMode, Version: version + 1, ContractID: CreditContractID},
		Credits:              credits,
	}
	if current.ConsumerAccess != nil && current.ConsumerAccess.Mode == policy.Mode {
		policy.ContractID = current.ConsumerAccess.ContractID
	}
	return c.updateCreditPrice(ctx, id, path, &policy)
}

func (c *client) updateCreditPrice(ctx context.Context, id, path string, policy *consumerPolicyWire) (*ConsumerManagementResult, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	body := struct {
		Policy          *consumerPolicyWire `json:"consumer_access"`
		ExpectedVersion int64               `json:"expected_consumer_access_version"`
	}{Policy: policy, ExpectedVersion: policy.Version - 1}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, body)
	if err != nil {
		return nil, fmt.Errorf("credit price update at policy version %d: %w", body.ExpectedVersion, err)
	}
	if reply.status != http.StatusOK {
		return nil, fmt.Errorf("credit price update at policy version %d: %w", body.ExpectedVersion, reply.problem())
	}
	var env struct {
		Data resourceRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode credit price update: %w", qurl.ErrInvalidAPIResponse, err)
	}
	row := &env.Data
	if err := validateSharingIdentity(id, &sharingRow{CRID: row.CRID, ResourceID: row.ResourceID}); err != nil {
		return nil, err
	}
	confirmed := row.ConsumerAccess
	if row.Private == nil || !*row.Private || confirmed == nil || confirmed.Credits != policy.Credits || !confirmed.Equal(policy.ConsumerAccessPolicy) {
		return nil, fmt.Errorf("%w: API did not confirm the requested credit price", qurl.ErrInvalidAPIResponse)
	}
	return &ConsumerManagementResult{Action: "configure", CRID: row.CRID, Policy: confirmed.policy(), CreditPrice: confirmed.Credits}, nil
}
