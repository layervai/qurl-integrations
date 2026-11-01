package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/resourceidentity"
)

// AccessCode contains public metadata; the one-time code is deliberately separate.
type AccessCode struct {
	ID         string     `json:"access_code_id"`
	ResourceID string     `json:"resource_id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	MaxUses    int        `json:"max_uses"`
	UseCount   int        `json:"use_count"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// CreatedAccessCode is emitted only on the explicit create command's stdout.
type CreatedAccessCode struct {
	AccessCode
	Code string `json:"code"`
}

// AccessCodeInput binds a code to one owned resource.
type AccessCodeInput struct {
	ResourceID string     `json:"resource_id"`
	Name       string     `json:"name,omitempty"`
	MaxUses    int        `json:"max_uses"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

var accessCodeIDPattern = regexp.MustCompile(`^acd_[a-z0-9_-]{11}$`)
var accessCodePattern = regexp.MustCompile(`^ac_[a-z0-9_-]{22}$`)

// ValidateAccessCodeID accepts the revocation ID, never the secret code.
func ValidateAccessCodeID(id string) error {
	if !accessCodeIDPattern.MatchString(id) {
		return fmt.Errorf("%w: access code ID must be acd_ followed by 11 lowercase URL-safe characters", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateAccessCode checks secret shape without including its value in errors.
func ValidateAccessCode(code string) error {
	if !accessCodePattern.MatchString(code) {
		return fmt.Errorf("%w: invalid access code format", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateAccessCodeInput runs before sign-in and again immediately before creation.
func ValidateAccessCodeInput(input AccessCodeInput, now time.Time) error {
	_, keyErr := resourceidentity.ValidateResourceID(input.ResourceID)
	if keyErr != nil && crid.Validate(input.ResourceID) != nil {
		return fmt.Errorf("%w: supply a public resource key or valid CRID", qurl.ErrInvalidResourceRequest)
	}
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) > 100 || input.MaxUses < 0 {
		return fmt.Errorf("%w: name must contain at most 100 characters and max uses must be nonnegative", qurl.ErrInvalidResourceRequest)
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expiry must be in the future", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

func validAccessCode(item *AccessCode) bool {
	_, err := resourceidentity.ValidateResourceID(item.ResourceID)
	return err == nil && ValidateAccessCodeID(item.ID) == nil && (item.Status == accountActiveStatus || item.Status == "revoked") && item.MaxUses >= 0 && item.UseCount >= 0 && !item.CreatedAt.IsZero() && utf8.ValidString(item.Name) && utf8.RuneCountInString(item.Name) <= 100
}

// ListAccessCodes reads one bounded system-account page, excluding plaintext codes.
func (c *client) ListAccessCodes(ctx context.Context, limit int, cursor string) (*AccountPage[AccessCode], error) {
	if err := ValidateAccountPage(limit); err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, accountListPath("/v1/access-codes", limit, cursor), nil)
	if err != nil {
		return nil, err
	}
	page, err := decodeAccountPage[AccessCode](reply, limit)
	if err != nil {
		return nil, err
	}
	for i := range page.Data {
		if !validAccessCode(&page.Data[i]) {
			return nil, fmt.Errorf("%w: incomplete access code metadata", qurl.ErrInvalidAPIResponse)
		}
	}
	return page, nil
}

// CreateAccessCode makes one attempt because plaintext cannot be recovered safely.
func (c *client) CreateAccessCode(ctx context.Context, input AccessCodeInput) (*CreatedAccessCode, error) {
	if err := ValidateAccessCodeInput(input, c.now()); err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/access-codes", input)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		return nil, reply.problem()
	}
	var wire struct {
		Data CreatedAccessCode `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || !validAccessCode(&wire.Data.AccessCode) || ValidateAccessCode(wire.Data.Code) != nil || wire.Data.Status != accountActiveStatus || wire.Data.UseCount != 0 || wire.Data.Name != input.Name || wire.Data.MaxUses != input.MaxUses || !sameAccessCodeExpiry(wire.Data.ExpiresAt, input.ExpiresAt) {
		return nil, fmt.Errorf("%w: incomplete or mismatched created access code", qurl.ErrInvalidAPIResponse)
	}
	if crid.Validate(input.ResourceID) == nil {
		if resourceidentity.ValidatePair(input.ResourceID, wire.Data.ResourceID) != nil {
			return nil, fmt.Errorf("%w: access code resource does not match CRID", qurl.ErrInvalidAPIResponse)
		}
	} else if wire.Data.ResourceID != input.ResourceID {
		return nil, fmt.Errorf("%w: access code resource does not match", qurl.ErrInvalidAPIResponse)
	}
	return &wire.Data, nil
}

func sameAccessCodeExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// DeleteAccessCode revokes one nonsecret ID without retry.
func (c *client) DeleteAccessCode(ctx context.Context, id string) error {
	if err := ValidateAccessCodeID(id); err != nil {
		return err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, "/v1/access-codes/"+id, nil)
	if err != nil {
		return err
	}
	if reply.status != http.StatusNoContent {
		return reply.problem()
	}
	return nil
}

// RedeemAccessCode sends no account authority and never retries a consumed use.
// elapsed is actual time since secret input began, not a fabricated bot-check value.
func RedeemAccessCode(ctx context.Context, cfg *Config, code string, elapsed time.Duration) (string, error) {
	if cfg == nil || cfg.APIKey != "" || cfg.OwnerID != "" {
		return "", fmt.Errorf("%w: redemption requires an anonymous configuration", qurl.ErrInvalidClientConfig)
	}
	if err := ValidateAccessCode(code); err != nil {
		return "", err
	}
	if elapsed < 3*time.Second {
		return "", fmt.Errorf("%w: wait at least three seconds after starting code input", qurl.ErrInvalidResourceRequest)
	}
	tr := newTransport(cfg)
	provider := qurl.CredentialProviderFunc(func(context.Context, *http.Request) error { return nil })
	sdk, err := qurl.NewClient(provider, qurl.WithBaseURL(cfg.BaseURL), qurl.WithHTTPClient(tr))
	if err != nil {
		return "", err
	}
	c := &client{sdk: sdk, transport: tr, baseURL: trimBaseURL(cfg.BaseURL), authorize: provider.Authorize, now: clock(cfg)}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/access-codes/redeem", struct {
		Code     string `json:"code"`
		Honeypot string `json:"honeypot"`
		Elapsed  int64  `json:"elapsed_ms"`
	}{Code: code, Elapsed: elapsed.Milliseconds()})
	if err != nil {
		return "", err
	}
	if reply.status != http.StatusOK {
		return "", reply.problem()
	}
	var wire struct {
		Data struct {
			RedirectURL string `json:"redirect_url"`
		} `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil {
		return "", fmt.Errorf("%w: invalid redemption result", qurl.ErrInvalidAPIResponse)
	}
	u, err := url.Parse(wire.Data.RedirectURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || ((u.Path == "" || u.Path == "/") && u.Fragment == "") {
		return "", fmt.Errorf("%w: missing redemption link", qurl.ErrInvalidAPIResponse)
	}
	return wire.Data.RedirectURL, nil
}
