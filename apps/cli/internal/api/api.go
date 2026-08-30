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
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

// Client is what commands program against.
type Client interface {
	// MintConnectorEnrollmentToken creates a short-lived, one-shot credential
	// bound to exactly one Connector. The caller supplies the idempotency key
	// so a higher-level enrollment attempt can recover an ambiguous response
	// without minting a second credential.
	MintConnectorEnrollmentToken(ctx context.Context, opts MintConnectorEnrollmentTokenOptions) (*ConnectorEnrollmentToken, error)
	// Publish registers targetURL as a protected resource and returns its
	// identity, CRID included when the service mints one.
	Publish(ctx context.Context, targetURL string, opts PublishOptions) (*Published, error)
	// Resolve mints a temporary access link for the resource identified by
	// id (CRID or public-key resource identifier; the service dual-accepts).
	Resolve(ctx context.Context, id string, opts ResolveOptions) (*Resolved, error)
	// List returns a page of the caller's resources.
	List(ctx context.Context, opts ListOptions) (*ResourcePage, error)
	// Resource returns one owner-visible resource by CRID or public resource ID.
	Resource(ctx context.Context, id string) (*ResourceSummary, error)
	// Sharing returns the durable desired state and current platform-observed
	// connection state of one tunnel resource.
	Sharing(ctx context.Context, id string) (*Sharing, error)
	// SetSharing idempotently changes a tunnel resource's desired state.
	SetSharing(ctx context.Context, id string, desired DesiredState) (*Sharing, error)
	// RestartSharing rotates the serving epoch and leaves the resource desired
	// on, including when it was previously off.
	RestartSharing(ctx context.Context, id string) (*Sharing, error)
	// Delete revokes the resource identified by id. Deletion is idempotent:
	// a resource that is already gone is success, reported via AlreadyGone.
	Delete(ctx context.Context, id string) (*DeleteResult, error)
	// Me returns the identity behind the configured credential. Cheap by
	// platform contract (no repository reads server-side), so login can
	// validate keys with it and whoami can call it freely.
	Me(ctx context.Context) (*Identity, error)
}

// AccountClient adds the one account-authorized bootstrap operation. The CLI
// keeps this capability only while it consumes a user-supplied account key;
// steady-state commands use Client from NewRegistered.
type AccountClient interface {
	Client
	MintAgentEnrollmentToken(ctx context.Context, opts MintAgentEnrollmentTokenOptions) (*AgentEnrollmentToken, error)
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
	// FoundExisting reports whether the service returned an already-published
	// resource instead of minting a new one. Nil means provenance is unknown,
	// as can happen when local enrollment reconciles an uncertain create by
	// reading the resource back.
	FoundExisting *bool
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

// ResourceSummary is one row of a List result. Description and Tags are the
// publish-time metadata (PublishOptions) read back, so a tool built on List
// can recognize a row by the label its publisher gave it.
//
// TODO(upstream-contract): the service redacts description and tags on
// connector-owned rows, so empty here means "not visible on this row", not
// necessarily "never set". Type is not redacted and is always populated —
// legacy rows with no stored type read back as "url".
type ResourceSummary struct {
	CRID         string
	ResourceID   string
	TargetURL    string
	Type         string
	Status       string
	DesiredState DesiredState
	ServingEpoch uint64
	Description  string
	Tags         []string
	CreatedAt    *time.Time
	ExpiresAt    *time.Time
}

// DesiredState is the durable customer intent for a tunnel resource.
type DesiredState string

const (
	// DesiredStateOn requests that the platform admit the tunnel.
	DesiredStateOn DesiredState = "on"
	// DesiredStateOff requests that the platform reject the tunnel.
	DesiredStateOff DesiredState = "off"
)

// ConnectionState is the control plane's observation of the current serving
// epoch. It is never inferred from desired state.
type ConnectionState string

const (
	// ConnectionStopped means the desired state is off.
	ConnectionStopped ConnectionState = "stopped"
	// ConnectionConnecting means the current epoch has not reached serving.
	ConnectionConnecting ConnectionState = "connecting"
	// ConnectionServing means the current epoch is registered and routable.
	ConnectionServing ConnectionState = "serving"
)

// Sharing is the owner-facing lifecycle projection for one tunnel resource.
type Sharing struct {
	ResourceID      string
	CRID            string
	DesiredState    DesiredState
	ServingEpoch    uint64
	ConnectionState ConnectionState
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
	// Sleep overrides the context-aware retry timer. Nil uses that timer; tests
	// inject a recorder when they do not need cancellation behavior.
	Sleep func(time.Duration)
	// NewRequestID mints the X-Request-Id value; nil means a random one.
	NewRequestID func() string
	// HTTPClient is the underlying HTTP client. Nil, or an injected client with
	// Timeout zero, gets a 30-second bound for each HTTP attempt. A nonzero
	// timeout is preserved. A retryable logical request can span multiple
	// attempts and bounded backoffs. Redirects are always refused because
	// credentials must not follow a Location header to another origin.
	HTTPClient *http.Client
}

type client struct {
	sdk            *qurl.Client
	transport      *transport
	registeredDoer qurl.HTTPDoer
	baseURL        string
	authorize      func(context.Context, *http.Request) error
}

// registeredClient exposes exactly Client. The concrete implementation also
// owns the account-only agent-enrollment method for New, but wrapping the
// narrow interface prevents a registered caller from recovering that method
// through a type assertion.
type registeredClient struct{ Client }

// New builds the one Client implementation. The same decorated transport
// serves both the SDK-backed calls and the direct REST calls, so headers,
// retry, and redaction cannot diverge between them.
func New(cfg *Config) (AccountClient, error) {
	if cfg == nil || cfg.BaseURL == "" {
		return nil, fmt.Errorf("%w: base URL must not be empty", qurl.ErrInvalidClientConfig)
	}
	tr := newTransport(cfg)
	provider := qurl.BearerToken(cfg.APIKey)
	// TODO(upstream-contract): the pinned qurl-go WithBaseURL contract rejects
	// cleartext non-loopback API origins before this bearer provider can send a
	// request. TestNewRejectsCleartextNonLoopbackBaseURL pins that boundary here.
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

// NewRegistered builds the steady-state CLI client from the sealed native
// agent state. The account API key is not accepted or retained on this path;
// qurl-go loads the narrow device credential and keeps its exact route and
// origin boundary around direct REST calls.
func NewRegistered(ctx context.Context, cfg *Config, store qurl.AgentStateStore) (Client, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context must not be nil", qurl.ErrInvalidClientConfig)
	}
	if cfg == nil || cfg.BaseURL == "" {
		return nil, fmt.Errorf("%w: base URL must not be empty", qurl.ErrInvalidClientConfig)
	}
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil, fmt.Errorf("%w: account API key is not valid for registered-client open", qurl.ErrInvalidClientConfig)
	}
	tr := newTransport(cfg)
	// TODO(upstream-contract): the pinned qurl-go WithAgentClientBaseURL contract
	// applies the same HTTPS-or-loopback gate to the device credential before it
	// loads state or sends a request. The local API contract test pins this too.
	sdk, err := qurl.OpenRegisteredAgent(ctx, store,
		qurl.WithAgentClientBaseURL(cfg.BaseURL),
		qurl.WithAgentClientHTTPClient(tr),
	)
	if err != nil {
		return nil, err
	}
	doer, err := sdk.RegisteredAgentResourceHTTPDoer()
	if err != nil {
		return nil, err
	}
	core := &client{
		sdk: sdk, transport: tr, registeredDoer: doer,
		baseURL: trimBaseURL(cfg.BaseURL),
	}
	return &registeredClient{Client: core}, nil
}

// Resolve delegates to the SDK's ResolveResource and carries its VerifyCRID
// forward as the result's VerifyKey.
func (c *client) Resolve(ctx context.Context, id string, opts ResolveOptions) (*Resolved, error) {
	var sdkOpts *qurl.ResolveResourceOptions
	if opts.TTLSeconds > 0 {
		sdkOpts = &qurl.ResolveResourceOptions{TTL: time.Duration(opts.TTLSeconds) * time.Second}
	}
	access, err := c.sdk.ResolveResource(ctx, id, sdkOpts)
	if err != nil {
		return nil, mapError(err)
	}
	return &Resolved{
		QURL:             access.Link,
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
