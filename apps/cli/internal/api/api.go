// Package qurlapi is the CLI's single seam to the qURL platform.
//
// Commands never construct HTTP requests, set headers, or interpret status
// codes: they call this package's Client interface and get repo-owned result
// structs and errors back. Everything transport-shaped lives here in one
// place — the User-Agent and X-Request-Id headers, bounded 429 retry that
// honors Retry-After, redaction of credentials from every diagnostic line,
// and the mapping of wire failures onto typed errors.
//
// Resolve delegates to qurl-go's ResolveResource (whose VerifyCRID carries
// the client half of the CRID trust story). Publish, list, and delete are
// direct calls against the /v1/resources REST surface through the same
// transport and error mapping: v0.5.3's ProtectURL does not send the
// platform's required `type: url` discriminator, and its list/delete
// variants validate connector-only identifier forms that reject CRIDs.
package qurlapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

// Client is what commands program against.
type Client interface {
	// Publish registers targetURL as a protected resource and returns its
	// identity, CRID included when the service mints one.
	Publish(ctx context.Context, targetURL string, opts PublishOptions) (*Published, error)
	// Resolve mints a temporary access link for the resource identified by
	// id (CRID or public-key resource identifier; the service dual-accepts).
	Resolve(ctx context.Context, id string, opts ResolveOptions) (*Resolved, error)
	// List returns a page of the caller's resources.
	List(ctx context.Context, opts ListOptions) (*ResourcePage, error)
	// Delete revokes the resource identified by id. Deletion is idempotent:
	// a resource that is already gone is success, reported via AlreadyGone.
	Delete(ctx context.Context, id string) (*DeleteResult, error)
	// Me returns the identity behind the configured credential. Cheap by
	// platform contract (no repository reads server-side), so login can
	// validate keys with it and whoami can call it freely.
	Me(ctx context.Context) (*Identity, error)
}

// PublishOptions carries the optional publish metadata.
type PublishOptions struct {
	Description string
	Tags        []string
	Alias       string
}

// ResolveOptions carries the optional resolve parameters.
type ResolveOptions struct {
	// TTLSeconds asks for the minted link's lifetime; 0 leaves the server
	// default in effect. The server may clamp the value it grants.
	TTLSeconds int
}

// ListOptions carries the optional list parameters; zero values are omitted
// from the request so server defaults apply.
type ListOptions struct {
	Limit  int
	Cursor string
	Status string
	Type   string
}

// Published is the repo-owned result of Publish.
type Published struct {
	CRID       string
	ResourceID string
	TargetURL  string
	Status     string
	CreatedAt  *time.Time
	ExpiresAt  *time.Time
	// FoundExisting reports that the service returned an already-published
	// resource instead of minting a new one.
	FoundExisting bool
}

// DeleteResult reports a completed (idempotent) delete.
type DeleteResult struct {
	// AlreadyGone reports that the resource was gone before the request.
	AlreadyGone bool
}

// Resolved is the repo-owned result of Resolve.
type Resolved struct {
	QURL             string
	CRID             string
	Type             string
	ExpiresAt        time.Time
	ExpiresInSeconds int
	SingleUse        bool

	verifyKey func(derSPKI []byte) error
}

// VerifyKey ties this response to a resource public key the caller already
// holds (DER SubjectPublicKeyInfo bytes, exactly as delivered). nil means the
// response's CRID commits to that key. Any non-nil error is fail-closed: the
// SDK's qurl.ErrNoCRID when the response carried no CRID, qurl.ErrCRIDMismatch
// when the key does not derive it, or the crid package sentinels when the
// held value fails the local gate.
func (r *Resolved) VerifyKey(derSPKI []byte) error {
	if r.verifyKey == nil {
		return fmt.Errorf("%w: response cannot be verified", qurl.ErrNoCRID)
	}
	return r.verifyKey(derSPKI)
}

// ResourcePage is one page of List results. HasMore — not NextCursor — is
// the continuation signal: the platform legitimately returns short or even
// empty pages with HasMore true after post-filtering.
type ResourcePage struct {
	Items      []ResourceSummary
	NextCursor string
	HasMore    bool
}

// ResourceSummary is one row of a List result.
type ResourceSummary struct {
	CRID       string
	ResourceID string
	TargetURL  string
	Status     string
	CreatedAt  *time.Time
	ExpiresAt  *time.Time
}

// Config configures New. Zero hooks get production defaults.
type Config struct {
	// BaseURL is the qURL API origin (no trailing slash required).
	BaseURL string
	// APIKey is the bearer credential. Never logged; see Redact.
	APIKey string
	// Version is the CLI version, used in the User-Agent header.
	Version string
	// Verbose, when non-nil, receives one already-redacted diagnostic line
	// per transport event.
	Verbose func(format string, args ...any)
	// Sleep is used between 429 retries; nil means time.Sleep. Tests inject
	// a recorder.
	Sleep func(time.Duration)
	// NewRequestID mints the X-Request-Id value; nil means a random one.
	NewRequestID func() string
	// HTTPClient is the underlying HTTP client; nil means a client with a
	// 30-second timeout that refuses redirects (credentials must not follow
	// a Location header to another origin).
	HTTPClient *http.Client
}

type client struct {
	sdk       *qurl.Client
	transport *transport
	baseURL   string
	authorize func(context.Context, *http.Request) error
}

// New builds the one Client implementation. The same decorated transport
// serves both the SDK-backed calls and the direct REST calls, so headers,
// retry, and redaction cannot diverge between them.
func New(cfg *Config) (Client, error) {
	if cfg == nil || cfg.BaseURL == "" {
		return nil, fmt.Errorf("%w: base URL must not be empty", qurl.ErrInvalidClientConfig)
	}
	tr := newTransport(cfg)
	provider := qurl.BearerToken(cfg.APIKey)
	sdk, err := qurl.NewClient(provider,
		qurl.WithBaseURL(cfg.BaseURL),
		qurl.WithHTTPClient(tr),
	)
	if err != nil {
		return nil, err
	}
	return &client{
		sdk:       sdk,
		transport: tr,
		baseURL:   trimBaseURL(cfg.BaseURL),
		authorize: provider.Authorize,
	}, nil
}

// Resolve delegates to the SDK's ResolveResource and carries its VerifyCRID
// forward as the result's VerifyKey.
func (c *client) Resolve(ctx context.Context, id string, opts ResolveOptions) (*Resolved, error) {
	var sdkOpts *qurl.ResolveResourceOptions
	if opts.TTLSeconds > 0 {
		sdkOpts = &qurl.ResolveResourceOptions{TTLSeconds: opts.TTLSeconds}
	}
	access, err := c.sdk.ResolveResource(ctx, id, sdkOpts)
	if err != nil {
		return nil, mapError(err)
	}
	return &Resolved{
		QURL:             access.QURL,
		CRID:             access.CRID,
		Type:             access.Type,
		ExpiresAt:        access.ExpiresAt,
		ExpiresInSeconds: access.ExpiresInSeconds,
		SingleUse:        access.SingleUse,
		verifyKey:        access.VerifyCRID,
	}, nil
}

// mapError converts a wire failure into this package's typed *Error while
// keeping every sentinel in the original chain reachable through Unwrap —
// errors.Is against qurl.ErrTemporaryAccessLinksDisabled (and friends) keeps
// working on the mapped error.
//
// Upstream SDK asks (qurl-go v0.5.3's APIError carries only
// StatusCode/Code/Type/Title/Detail — qurl/client.go:1345): RequestID, so
// SDK-path failures can print a request id like direct-path ones do; and
// RetryAfter, so SDK-path 429s that outlive the transport's bounded retry can
// render the server's requested wait. Populate both here the release they
// appear.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *qurl.APIError
	if errors.As(err, &apiErr) {
		return &Error{
			StatusCode: apiErr.StatusCode,
			Code:       apiErr.Code,
			Title:      apiErr.Title,
			Detail:     apiErr.Detail,
			err:        err,
		}
	}
	return err
}
