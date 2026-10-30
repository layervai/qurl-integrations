package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

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
	reply, err := a.client.doRESTOnce(ctx, method, "/v1/me/access-rules", body)
	if err != nil {
		return accessrules.AccountLibrary{}, err
	}
	if reply.status != http.StatusOK {
		fixed := &Error{StatusCode: reply.status, Title: "could not synchronize access rules"}
		if reply.status == http.StatusConflict {
			return accessrules.AccountLibrary{}, errors.Join(accessrules.ErrConflict, fixed)
		}
		if reply.status == http.StatusForbidden {
			return accessrules.AccountLibrary{}, errors.Join(accessrules.ErrUnlinked, fixed)
		}
		return accessrules.AccountLibrary{}, fixed
	}
	var unique map[string]json.RawMessage
	if accessrules.StrictDecode(reply.body, &unique) != nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(reply.body, &envelope) != nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(envelope.Data, &fields) != nil || len(fields) != 4 || bytes.Equal(bytes.TrimSpace(fields["version"]), []byte("null")) {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	var library accessrules.AccountLibrary
	if accessrules.StrictDecode(envelope.Data, &library) != nil || library.Validate() != nil {
		return accessrules.AccountLibrary{}, qurl.ErrInvalidAPIResponse
	}
	return library, nil
}
