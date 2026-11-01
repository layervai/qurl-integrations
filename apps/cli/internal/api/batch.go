package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/resourceidentity"
)

// BatchCreateItem mirrors the URL-only public CreateQurlRequest, preserving omissions.
type BatchCreateItem struct {
	Type            string          `json:"type,omitempty"`
	Private         *bool           `json:"private,omitempty"`
	TargetURL       string          `json:"target_url"`
	ExpiresIn       string          `json:"expires_in,omitempty"`
	OneTimeUse      *bool           `json:"one_time_use,omitempty"`
	MaxSessions     *int            `json:"max_sessions,omitempty"`
	SessionDuration string          `json:"session_duration,omitempty"`
	AccessPolicy    json.RawMessage `json:"access_policy,omitempty"`
	Label           string          `json:"label,omitempty"`
	CustomDomain    string          `json:"custom_domain,omitempty"`
}

// BatchCreateInput contains one bounded request; the CLI never splits or retries it.
type BatchCreateInput struct {
	Items []BatchCreateItem `json:"items"`
}

// ParseBatchCreateInput rejects unknown outer/item fields and multiple documents.
// Nested access-policy validation stays with the service's authoritative schema.
func ParseBatchCreateInput(raw []byte) (BatchCreateInput, error) {
	var input BatchCreateInput
	if len(raw) > 1<<20 || !validBatchInputObject(raw) {
		return input, fmt.Errorf("%w: batch input must be one object of at most 1 MiB without null or repeated fields", qurl.ErrInvalidResourceRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		return input, fmt.Errorf("%w: expected a JSON object with items using the public URL create fields", qurl.ErrInvalidResourceRequest)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return input, fmt.Errorf("%w: supply exactly one JSON document", qurl.ErrInvalidResourceRequest)
	}
	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(raw, &envelope)
	for _, item := range envelope.Items {
		if !validBatchInputObject(item) {
			return input, fmt.Errorf("%w: batch items cannot contain null or repeated fields", qurl.ErrInvalidResourceRequest)
		}
	}
	return input, ValidateBatchCreateInput(input)
}

func validBatchInputObject(raw []byte) bool {
	seen := map[string]bool{}
	valid := true
	object := objectMembers(raw, func(name string, value json.RawMessage) {
		if seen[name] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			valid = false
		}
		seen[name] = true
	})
	return object && valid
}

// ValidateBatchCreateInput checks every target before browser sign-in or a write.
func ValidateBatchCreateInput(input BatchCreateInput) error {
	if len(input.Items) < 1 || len(input.Items) > 100 {
		return fmt.Errorf("%w: batch must contain 1–100 items", qurl.ErrInvalidResourceRequest)
	}
	for i := range input.Items {
		item := &input.Items[i]
		if item.Type != "" && item.Type != "url" {
			return fmt.Errorf("%w: item %d must use URL type", qurl.ErrInvalidResourceRequest, i)
		}
		if err := validateTargetURL(item.TargetURL); err != nil {
			return fmt.Errorf("%w: item %d needs an HTTP or HTTPS target without credentials", qurl.ErrInvalidResourceRequest, i)
		}
		if item.MaxSessions != nil && (*item.MaxSessions < 0 || *item.MaxSessions > 1000) {
			return fmt.Errorf("%w: item %d max_sessions must be 0–1000", qurl.ErrInvalidResourceRequest, i)
		}
	}
	return nil
}

// BatchItemError is a service result for one failed item, never an automatic retry instruction.
type BatchItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BatchItemResult keeps the original zero-based index even if the service orders results differently.
type BatchItemResult struct {
	Index         int             `json:"index"`
	Success       bool            `json:"success"`
	ResourceID    string          `json:"resource_id,omitempty"`
	CRID          string          `json:"crid,omitempty"`
	QURLLink      string          `json:"qurl_link,omitempty"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	BrandedDomain string          `json:"branded_domain,omitempty"`
	QURLSite      string          `json:"qurl_site,omitempty"`
	Error         *BatchItemError `json:"error,omitempty"`
}

// BatchCreateResult retains every result, including mixed and all-failed responses.
type BatchCreateResult struct {
	Succeeded int               `json:"succeeded"`
	Failed    int               `json:"failed"`
	Results   []BatchItemResult `json:"results"`
}

// BatchCreate sends exactly one request without generic response-cache replay.
func (c *client) BatchCreate(ctx context.Context, input BatchCreateInput) (*BatchCreateResult, error) {
	if err := ValidateBatchCreateInput(input); err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/qurls/batch", input)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated && reply.status != http.StatusMultiStatus && reply.status != http.StatusBadRequest {
		return nil, reply.problem()
	}
	var wire struct {
		Data *struct {
			Succeeded *int              `json:"succeeded"`
			Failed    *int              `json:"failed"`
			Results   []json.RawMessage `json:"results"`
		} `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || wire.Data == nil {
		if reply.status == http.StatusBadRequest {
			return nil, reply.problem()
		}
		return nil, fmt.Errorf("%w: missing batch results", qurl.ErrInvalidAPIResponse)
	}
	data := wire.Data
	if data.Succeeded == nil || data.Failed == nil || len(data.Results) != len(input.Items) {
		return nil, fmt.Errorf("%w: incomplete batch counts", qurl.ErrInvalidAPIResponse)
	}
	result := &BatchCreateResult{Succeeded: *data.Succeeded, Failed: *data.Failed, Results: make([]BatchItemResult, len(input.Items))}
	seen := make([]bool, len(input.Items))
	succeeded := 0
	for _, raw := range data.Results {
		item, err := decodeBatchItem(raw, seen)
		if err != nil {
			return nil, err
		}
		seen[item.Index] = true
		if item.Success {
			succeeded++
		}
		result.Results[item.Index] = *item
	}
	expectedStatus := http.StatusMultiStatus
	if succeeded == len(input.Items) {
		expectedStatus = http.StatusCreated
	} else if succeeded == 0 {
		expectedStatus = http.StatusBadRequest
	}
	if succeeded != result.Succeeded || len(input.Items)-succeeded != result.Failed || reply.status != expectedStatus {
		return nil, fmt.Errorf("%w: batch counts or status disagree with results", qurl.ErrInvalidAPIResponse)
	}
	return result, nil
}

func decodeBatchItem(raw json.RawMessage, seen []bool) (*BatchItemResult, error) {
	var presence struct {
		Index   *int  `json:"index"`
		Success *bool `json:"success"`
	}
	var item BatchItemResult
	if json.Unmarshal(raw, &presence) != nil || presence.Index == nil || presence.Success == nil || *presence.Index < 0 || *presence.Index >= len(seen) || seen[*presence.Index] || json.Unmarshal(raw, &item) != nil {
		return nil, fmt.Errorf("%w: duplicate, missing or invalid batch index", qurl.ErrInvalidAPIResponse)
	}
	if err := validateBatchItemResult(&item); err != nil {
		return nil, err
	}
	return &item, nil
}

func validateBatchItemResult(item *BatchItemResult) error {
	if !item.Success {
		if item.Error == nil || item.Error.Code == "" || item.Error.Message == "" || item.QURLLink != "" {
			return fmt.Errorf("%w: incomplete failed batch item", qurl.ErrInvalidAPIResponse)
		}
		return nil
	}
	u, err := url.Parse(item.QURLLink)
	if err != nil || u.Scheme != schemeHTTPS || u.Hostname() == "" || u.User != nil || ((u.Path == "" || u.Path == "/") && u.Fragment == "") || item.Error != nil || item.ExpiresAt == nil || item.ExpiresAt.IsZero() || resourceidentity.ValidatePair(item.CRID, item.ResourceID) != nil {
		return fmt.Errorf("%w: incomplete or mismatched successful batch item", qurl.ErrInvalidAPIResponse)
	}
	return nil
}
