package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ResourceBatchGetMaxIDs bounds one metadata request.
const ResourceBatchGetMaxIDs = 100

// BatchGetResources reads metadata for public-key resource IDs without querying qURL history.
func (c *Client) BatchGetResources(ctx context.Context, ids []string) ([]Resource, error) {
	if len(ids) == 0 || len(ids) > ResourceBatchGetMaxIDs {
		return nil, errors.New("resource metadata lookup requires 1 to 100 IDs")
	}
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			return nil, errors.New("resource metadata lookup has an empty or padded ID")
		}
		if _, duplicate := requested[id]; duplicate {
			return nil, errors.New("resource metadata lookup has a duplicate ID")
		}
		requested[id] = struct{}{}
	}
	body, err := json.Marshal(struct {
		ResourceIDs []string `json:"resource_ids"`
	}{ids})
	if err != nil {
		return nil, fmt.Errorf("marshal resource metadata lookup: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/resources/batch-get", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build resource metadata lookup: %w", err)
	}
	var resources []Resource
	meta, err := c.do(req, &resources, "POST /v1/resources/batch-get")
	if err != nil {
		return nil, err
	}
	if resources == nil || meta == nil || meta.HasMore || meta.NextCursor != "" {
		return nil, errors.New("resource metadata response is missing or incomplete")
	}
	returned := make(map[string]struct{}, len(resources))
	for index := range resources {
		resource := &resources[index]
		if _, ok := requested[resource.ResourceID]; !ok {
			return nil, errors.New("resource metadata response contains an unrequested ID")
		}
		if _, duplicate := returned[resource.ResourceID]; duplicate {
			return nil, errors.New("resource metadata response contains a duplicate ID")
		}
		returned[resource.ResourceID] = struct{}{}
	}
	return resources, nil
}
