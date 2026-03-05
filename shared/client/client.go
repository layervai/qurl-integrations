// Package client provides a Go client for the QURL API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultTimeout = 30 * time.Second

// Client is a QURL API client.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// New creates a QURL API client.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// QURL represents a QURL resource.
type QURL struct {
	ID          string     `json:"id"`
	ShortCode   string     `json:"short_code"`
	TargetURL   string     `json:"target_url"`
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ClickCount  int        `json:"click_count"`
	LinkURL     string     `json:"link_url"`
}

// CreateInput is the input for creating a QURL.
type CreateInput struct {
	TargetURL   string `json:"target_url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ExpiresIn   string `json:"expires_in,omitempty"`
}

// Create creates a new QURL.
func (c *Client) Create(ctx context.Context, input CreateInput) (*QURL, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal create input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurls", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var qurl QURL
	if err := c.do(req, &qurl); err != nil {
		return nil, err
	}
	return &qurl, nil
}

// Get retrieves a QURL by ID.
func (c *Client) Get(ctx context.Context, id string) (*QURL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/qurls/"+id, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var qurl QURL
	if err := c.do(req, &qurl); err != nil {
		return nil, err
	}
	return &qurl, nil
}

// ListInput is the input for listing QURLs.
type ListInput struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// ListOutput is the output of listing QURLs.
type ListOutput struct {
	QURLs      []QURL `json:"qurls"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// List retrieves a paginated list of QURLs.
func (c *Client) List(ctx context.Context, input ListInput) (*ListOutput, error) {
	url := fmt.Sprintf("%s/v1/qurls?limit=%d", c.baseURL, input.Limit)
	if input.Cursor != "" {
		url += "&cursor=" + input.Cursor
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out ListOutput
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete deletes a QURL by ID.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/qurls/"+id, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.do(req, nil)
}

// APIError represents an error response from the QURL API.
type APIError struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
}

// Error returns the error message.
func (e *APIError) Error() string {
	return fmt.Sprintf("qurl api error (%d): %s", e.StatusCode, e.Message)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		if json.Unmarshal(respBody, apiErr) != nil {
			apiErr.Message = string(respBody)
		}
		return apiErr
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}
