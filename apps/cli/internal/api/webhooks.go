package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"
)

// WebhookManagement uses the signed-in account; linked device owners are unsupported.
type WebhookManagement interface {
	Me(context.Context) (*Identity, error)
	ListWebhooks(context.Context, int, string) (*WebhookPage[Webhook], error)
	GetWebhook(context.Context, string) (*Webhook, error)
	CreateWebhook(context.Context, WebhookInput) (*WebhookSecret, error)
	UpdateWebhook(context.Context, string, WebhookInput) (*Webhook, error)
	DeleteWebhook(context.Context, string) error
	RotateWebhookSecret(context.Context, string) (*WebhookSecret, error)
	WebhookEvents(context.Context) ([]WebhookEvent, error)
	WebhookDeliveries(context.Context, string, int, string) (*WebhookPage[WebhookDelivery], error)
}

// NewWebhookManagement shares the CLI transport without device-owner selection.
func NewWebhookManagement(cfg *Config) (WebhookManagement, error) {
	if cfg == nil || cfg.OwnerID != "" {
		return nil, fmt.Errorf("%w: webhooks require the browser account owner", qurl.ErrInvalidResourceRequest)
	}
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return c.(*client), nil
}

// Webhook excludes signing secrets from ordinary reads.
// TODO(upstream-contract): qurl-service api/openapi.yaml owns webhook shapes and retry rules.
type Webhook struct {
	ID                  string   `json:"webhook_id"`
	OwnerID             string   `json:"owner_id"`
	URL                 string   `json:"url"`
	Events              []string `json:"events"`
	Status              string   `json:"status"`
	Description         string   `json:"description,omitempty"`
	CreatedAt           string   `json:"created_at,omitempty"`
	UpdatedAt           string   `json:"updated_at,omitempty"`
	FailureCount        int      `json:"failure_count"`
	LastDeliverySuccess bool     `json:"last_delivery_success"`
	LastDeliveryTime    int64    `json:"last_delivery_time,omitempty"`
}

// WebhookSecret is returned only by explicit creation and rotation.
type WebhookSecret struct {
	Webhook
	Secret string `json:"secret"`
}

// WebhookInput preserves an explicit empty description on update.
type WebhookInput struct {
	URL         *string   `json:"url,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Description *string   `json:"description,omitempty"`
	Status      *string   `json:"status,omitempty"`
}

// WebhookPage contains one bounded page without automatic cursor traversal.
type WebhookPage[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// WebhookEvent is a catalog entry, not a delivery guarantee.
type WebhookEvent struct {
	Type        string `json:"type"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// WebhookDelivery omits endpoint response bodies and errors, which can contain secrets.
type WebhookDelivery struct {
	ID           string `json:"delivery_id"`
	WebhookID    string `json:"webhook_id"`
	EventType    string `json:"event_type"`
	Status       string `json:"status"`
	ResponseCode int    `json:"response_code"`
	DurationMS   int    `json:"duration_ms"`
	RetryCount   int    `json:"retry_count"`
	CreatedAt    string `json:"created_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

const webhookActiveStatus = "active"

var webhookIDPattern = regexp.MustCompile(`^wh_[A-Za-z0-9_-]{16}$`)
var webhookSecretPattern = regexp.MustCompile(`^whsec_[A-Za-z0-9_-]{43}$`)
var webhookDeliveryIDPattern = regexp.MustCompile(`^wd_[A-Za-z0-9_-]{16}$`)
var webhookEventNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$`)

// ValidateWebhookID rejects path syntax and secrets before browser sign-in.
func ValidateWebhookID(id string) error {
	if !webhookIDPattern.MatchString(id) {
		return fmt.Errorf("%w: webhook ID must be wh_ followed by 16 URL-safe characters", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateWebhookPage checks the server page bounds before sign-in.
func ValidateWebhookPage(limit int) error {
	if limit < 1 || limit > 100 {
		return fmt.Errorf("%w: limit must be 1–100", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateWebhookInput checks fields without changing endpoint URLs or descriptions.
func ValidateWebhookInput(input WebhookInput, create bool) error {
	if create && (input.URL == nil || input.Events == nil || input.Status != nil) {
		return fmt.Errorf("%w: create requires URL and events; status is update-only", qurl.ErrInvalidResourceRequest)
	}
	if !create && input.URL == nil && input.Events == nil && input.Description == nil && input.Status == nil {
		return fmt.Errorf("%w: supply at least one webhook field", qurl.ErrInvalidResourceRequest)
	}
	if input.URL != nil {
		if err := validateWebhookEndpoint(*input.URL); err != nil {
			return err
		}
	}
	if input.Description != nil && (!utf8.ValidString(*input.Description) || utf8.RuneCountInString(*input.Description) > 500) {
		return fmt.Errorf("%w: description must be valid text within 500 characters", qurl.ErrInvalidResourceRequest)
	}
	if input.Status != nil && *input.Status != webhookActiveStatus && *input.Status != "disabled" {
		return fmt.Errorf("%w: status must be active or disabled", qurl.ErrInvalidResourceRequest)
	}
	if input.Events != nil {
		return validateWebhookEvents(*input.Events)
	}
	return nil
}

func validateWebhookEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || !utf8.ValidString(raw) || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%w: webhook URL must be HTTPS without credentials or a fragment, up to 2048 bytes", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

func validateWebhookEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("%w: supply at least one event", qurl.ErrInvalidResourceRequest)
	}
	seen := map[string]bool{}
	for _, event := range events {
		switch event {
		case "qurl.created", "qurl.expired", "qurl.revoked", "qurl.updated", "resource.closed", "qurl.accessed", "consumer.subscribed", "qurl.access_denied", "qurl.token_exhausted", "quota.warning", "quota.exceeded", "token.minted", "token.expired", "domain.verified", "domain.failed", "domain.deleted":
		default:
			return fmt.Errorf("%w: unsupported webhook event; use webhooks events", qurl.ErrInvalidResourceRequest)
		}
		if seen[event] {
			return fmt.Errorf("%w: duplicate webhook event", qurl.ErrInvalidResourceRequest)
		}
		seen[event] = true
	}
	return nil
}

func webhookListPath(path string, limit int, cursor string) string {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	return path + "?" + query.Encode()
}

func decodeWebhookPage[T any](reply *restReply, limit int) (*WebhookPage[T], error) {
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var wire struct {
		Data []T `json:"data"`
		Meta *struct {
			HasMore    *bool  `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		} `json:"meta"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || wire.Data == nil || len(wire.Data) > limit || wire.Meta == nil || wire.Meta.HasMore == nil || (*wire.Meta.HasMore && wire.Meta.NextCursor == "") {
		return nil, fmt.Errorf("%w: incomplete webhook page", qurl.ErrInvalidAPIResponse)
	}
	return &WebhookPage[T]{Data: wire.Data, HasMore: *wire.Meta.HasMore, NextCursor: wire.Meta.NextCursor}, nil
}

// ListWebhooks reads metadata only, even when a server adds a secret field.
func (c *client) ListWebhooks(ctx context.Context, limit int, cursor string) (*WebhookPage[Webhook], error) {
	if err := ValidateWebhookPage(limit); err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, webhookListPath("/v1/webhooks", limit, cursor), nil)
	if err != nil {
		return nil, err
	}
	page, err := decodeWebhookPage[Webhook](reply, limit)
	if err != nil {
		return nil, err
	}
	for i := range page.Data {
		if !validWebhook(&page.Data[i]) {
			return nil, fmt.Errorf("%w: invalid webhook metadata", qurl.ErrInvalidAPIResponse)
		}
	}
	return page, nil
}

func validWebhook(item *Webhook) bool {
	return webhookIDPattern.MatchString(item.ID) && item.OwnerID != "" && item.URL != "" && (item.Status == webhookActiveStatus || item.Status == "disabled")
}

func decodeWebhook(reply *restReply, status int, id string) (*Webhook, error) {
	if reply.status != status {
		return nil, reply.problem()
	}
	var wire struct {
		Data Webhook `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || !validWebhook(&wire.Data) || (id != "" && wire.Data.ID != id) {
		return nil, fmt.Errorf("%w: invalid webhook metadata", qurl.ErrInvalidAPIResponse)
	}
	return &wire.Data, nil
}

// GetWebhook fetches one metadata record without its signing secret.
func (c *client) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	if err := ValidateWebhookID(id); err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/webhooks/"+id, nil)
	if err != nil {
		return nil, err
	}
	return decodeWebhook(reply, http.StatusOK, id)
}

// CreateWebhook makes one HTTP attempt; there is no idempotent replay guarantee.
func (c *client) CreateWebhook(ctx context.Context, input WebhookInput) (*WebhookSecret, error) {
	if err := ValidateWebhookInput(input, true); err != nil {
		return nil, err
	}
	item, err := c.writeWebhookSecret(ctx, "/v1/webhooks", "", input, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	if item.URL != *input.URL || !sameWebhookEvents(item.Events, *input.Events) || item.Status != webhookActiveStatus || (input.Description != nil && item.Description != *input.Description) {
		return nil, fmt.Errorf("%w: created webhook differs from request", qurl.ErrInvalidAPIResponse)
	}
	return item, nil
}

func sameWebhookEvents(left, right []string) bool {
	a, b := slices.Clone(left), slices.Clone(right)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// UpdateWebhook makes one attempt and checks the returned fields against the request.
func (c *client) UpdateWebhook(ctx context.Context, id string, input WebhookInput) (*Webhook, error) {
	if err := ValidateWebhookID(id); err != nil {
		return nil, err
	}
	if err := ValidateWebhookInput(input, false); err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, "/v1/webhooks/"+id, input)
	if err != nil {
		return nil, err
	}
	item, err := decodeWebhook(reply, http.StatusOK, id)
	if err != nil {
		return nil, err
	}
	if (input.URL != nil && item.URL != *input.URL) || (input.Events != nil && !sameWebhookEvents(item.Events, *input.Events)) || (input.Description != nil && item.Description != *input.Description) || (input.Status != nil && item.Status != *input.Status) {
		return nil, fmt.Errorf("%w: updated webhook differs from request", qurl.ErrInvalidAPIResponse)
	}
	return item, nil
}

// DeleteWebhook reports success only for a confirmed removal.
func (c *client) DeleteWebhook(ctx context.Context, id string) error {
	if err := ValidateWebhookID(id); err != nil {
		return err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, "/v1/webhooks/"+id, nil)
	if err != nil {
		return err
	}
	if reply.status != http.StatusNoContent {
		return reply.problem()
	}
	return nil
}

// RotateWebhookSecret replaces the old secret without an automatic retry.
func (c *client) RotateWebhookSecret(ctx context.Context, id string) (*WebhookSecret, error) {
	if err := ValidateWebhookID(id); err != nil {
		return nil, err
	}
	return c.writeWebhookSecret(ctx, "/v1/webhooks/"+id+"/secret", id, nil, http.StatusOK)
}

func (c *client) writeWebhookSecret(ctx context.Context, path, id string, body any, status int) (*WebhookSecret, error) {
	reply, err := c.doRESTOnce(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if reply.status != status {
		return nil, reply.problem()
	}
	var wire struct {
		Data WebhookSecret `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || !validWebhook(&wire.Data.Webhook) || (id != "" && wire.Data.ID != id) || !webhookSecretPattern.MatchString(wire.Data.Secret) {
		return nil, fmt.Errorf("%w: incomplete webhook secret result", qurl.ErrInvalidAPIResponse)
	}
	return &wire.Data, nil
}

// WebhookEvents reads the current catalog without claiming every event is delivered.
func (c *client) WebhookEvents(ctx context.Context) ([]WebhookEvent, error) {
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/webhooks/events", nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var wire struct {
		Data []WebhookEvent `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || wire.Data == nil {
		return nil, fmt.Errorf("%w: incomplete webhook event catalog", qurl.ErrInvalidAPIResponse)
	}
	for _, item := range wire.Data {
		if !webhookEventNamePattern.MatchString(item.Type) || item.Category == "" {
			return nil, fmt.Errorf("%w: invalid webhook event catalog", qurl.ErrInvalidAPIResponse)
		}
	}
	return wire.Data, nil
}

// WebhookDeliveries reads bounded metadata without endpoint response text.
func (c *client) WebhookDeliveries(ctx context.Context, id string, limit int, cursor string) (*WebhookPage[WebhookDelivery], error) {
	if err := ValidateWebhookID(id); err != nil {
		return nil, err
	}
	if err := ValidateWebhookPage(limit); err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, webhookListPath("/v1/webhooks/"+id+"/deliveries", limit, cursor), nil)
	if err != nil {
		return nil, err
	}
	page, err := decodeWebhookPage[WebhookDelivery](reply, limit)
	if err != nil {
		return nil, err
	}
	for _, item := range page.Data {
		if !webhookDeliveryIDPattern.MatchString(item.ID) || item.WebhookID != id || item.Status == "" {
			return nil, fmt.Errorf("%w: invalid webhook delivery metadata", qurl.ErrInvalidAPIResponse)
		}
	}
	return page, nil
}
