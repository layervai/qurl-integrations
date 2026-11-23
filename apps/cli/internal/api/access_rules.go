package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

const accessRulesPath = "/v1/me/access-rules"

// AccessRules exposes only the canonical account's versioned preset library.
func (c *client) AccessRules() accessrules.Remote { return accountAccessRules{client: c} }

type accountAccessRules struct{ client *client }

// Load reads the authenticated account library.
func (a accountAccessRules) Load(ctx context.Context) (accessrules.AccountLibrary, error) {
	return a.request(ctx, http.MethodGet, nil)
}

// Replace conditionally writes the complete custom library once.
func (a accountAccessRules) Replace(ctx context.Context, version int64, items []accessrules.Definition) (accessrules.AccountLibrary, error) {
	if version < 0 || version >= accessrules.MaxVersion || accessrules.ValidateItems(items) != nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidResourceRequest
	}
	return a.request(ctx, http.MethodPut, struct {
		Version int64                    `json:"version"`
		Items   []accessrules.Definition `json:"items"`
	}{version, items})
}
func (a accountAccessRules) request(ctx context.Context, method string, body any) (accessrules.AccountLibrary, error) {
	// Version CAS, rather than replay keys, owns recovery from an uncertain PUT.
	reply, err := a.client.doRESTOnce(ctx, method, accessRulesPath, body)
	if err != nil {
		return accessrules.AccountLibrary{}, err
	}
	if reply.status != http.StatusOK {
		fixed := &Error{StatusCode: reply.status, Title: "could not synchronize access rules"}
		if reply.status == http.StatusConflict {
			return accessrules.AccountLibrary{}, errors.Join(accessrules.ErrConflict, fixed)
		}
		if reply.status == http.StatusForbidden {
			// A frozen account keeps its own standing message; any other 403
			// means this identity has no linked account library.
			// TODO(upstream-contract): qurl-service documents only
			// account_frozen and device_not_linked for this route.
			var problem *Error
			if errors.As(reply.problem(), &problem) && strings.EqualFold(problem.Code, "account_frozen") {
				fixed.Code = problem.Code
				return accessrules.AccountLibrary{}, fixed
			}
			return accessrules.AccountLibrary{}, errors.Join(accessrules.ErrUnlinked, fixed)
		}
		return accessrules.AccountLibrary{}, fixed
	}
	// Unknown envelope fields, including the service's built-ins, are ignored
	// so the service can evolve; a rule keeps its unknown fields because the
	// whole list is written back.
	var envelope struct {
		Data struct {
			AccountID string                   `json:"account_id"`
			Version   *int64                   `json:"version"`
			Items     []accessrules.Definition `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal(reply.body, &envelope) != nil || envelope.Data.Version == nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	library := accessrules.AccountLibrary{AccountID: envelope.Data.AccountID, Version: *envelope.Data.Version, Items: envelope.Data.Items}
	if library.Validate() != nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	return library, nil
}
