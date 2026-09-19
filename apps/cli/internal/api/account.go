package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"

	"net/http"

	"github.com/layervai/qurl-go/qurl"
)

// LinkAccount attaches the current device namespace to a verified account.
// TODO(upstream-contract): qurl-service owns the account routes, response shapes,
// X-QURL-Owner membership rules, and 64 linked-device limit.
// The access token stays in memory and is never stored in the device state.
func (c *client) LinkAccount(ctx context.Context, accountToken string) error {
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/account/link", map[string]string{"account_token": accountToken})
	if err != nil {
		return err
	}
	if reply.status != http.StatusOK {
		return reply.problem()
	}
	var result struct {
		OwnerID   string `json:"owner_id"`
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(reply.body, &result) != nil || result.OwnerID == "" || result.AccountID == "" {
		return fmt.Errorf(msgAccountLinkInvalid, qurl.ErrInvalidAPIResponse)
	}
	return nil
}

// AccountOwners discovers recoverable namespaces using the browser account.
func AccountOwners(ctx context.Context, cfg *Config) ([]string, error) {
	account, err := New(cfg)
	if err != nil {
		return nil, err
	}
	c := account.(*client)
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/account/owners", nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var result struct {
		Owners []string `json:"owners"`
	}
	if json.Unmarshal(reply.body, &result) != nil || len(result.Owners) == 0 || len(result.Owners) > 65 {
		return nil, fmt.Errorf(msgAccountOwnersInvalid, qurl.ErrInvalidAPIResponse)
	}
	return result.Owners, nil
}
