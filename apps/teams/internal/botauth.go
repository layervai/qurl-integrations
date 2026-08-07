package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const (
	botConnectorMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	botAuthHTTPTimeout      = 10 * time.Second
	botJWKSRefreshInterval  = 15 * time.Minute
)

// TokenValidator validates the incoming Teams bearer token for a request.
type TokenValidator interface {
	Validate(ctx context.Context, bearerToken, serviceURL string) error
}

// IncomingTokenValidator validates Bot Framework bearer tokens for Teams requests.
type IncomingTokenValidator struct {
	AppID       string
	MetadataURL string
	HTTPClient  *http.Client

	mu       sync.Mutex
	issuer   string
	jwksURL  string
	cache    *jwk.Cache
	cacheCtx context.Context
}

type botOpenIDMetadata struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// NewIncomingTokenValidator builds the default Teams incoming token validator.
func NewIncomingTokenValidator(ctx context.Context, appID string) *IncomingTokenValidator {
	if ctx == nil {
		ctx = context.Background()
	}
	return &IncomingTokenValidator{
		AppID:       strings.TrimSpace(appID),
		MetadataURL: botConnectorMetadataURL,
		HTTPClient:  &http.Client{Timeout: botAuthHTTPTimeout},
		cacheCtx:    ctx,
	}
}

// Validate checks the bearer token against Bot Framework metadata and JWKS.
func (v *IncomingTokenValidator) Validate(ctx context.Context, bearerToken, serviceURL string) error {
	if strings.TrimSpace(v.AppID) == "" {
		return errors.New("incoming token validator app id is required")
	}
	bearerToken = strings.TrimSpace(bearerToken)
	if bearerToken == "" {
		return errors.New("missing bearer token")
	}
	set, issuer, err := v.keySet(ctx)
	if err != nil {
		return err
	}
	tok, err := jwt.Parse([]byte(bearerToken),
		jwt.WithKeySet(set,
			jws.WithRequireKid(true),
			jws.WithInferAlgorithmFromKey(true)),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(v.AppID),
		jwt.WithValidate(true),
	)
	if err != nil {
		return fmt.Errorf("verify bot connector token: %w", err)
	}
	if claim, ok := tok.Get("serviceurl"); ok {
		if actual, ok := claim.(string); ok && actual != "" && !sameServiceURL(actual, serviceURL) {
			return fmt.Errorf("serviceUrl claim mismatch: token=%q request=%q", actual, serviceURL)
		}
	}
	return nil
}

func (v *IncomingTokenValidator) keySet(ctx context.Context) (jwk.Set, string, error) {
	v.mu.Lock()
	if v.cache != nil && v.jwksURL != "" && v.issuer != "" {
		cache := v.cache
		jwksURL := v.jwksURL
		issuer := v.issuer
		v.mu.Unlock()
		set, err := cache.Get(ctx, jwksURL)
		if err != nil {
			return nil, "", fmt.Errorf("get bot connector jwks: %w", err)
		}
		return set, issuer, nil
	}
	v.mu.Unlock()

	meta, err := v.fetchMetadata(ctx)
	if err != nil {
		return nil, "", err
	}
	cache := jwk.NewCache(v.cacheCtx)
	if err := cache.Register(meta.JWKSURI, jwk.WithMinRefreshInterval(botJWKSRefreshInterval)); err != nil {
		return nil, "", fmt.Errorf("register bot connector jwks: %w", err)
	}
	if _, err := cache.Refresh(ctx, meta.JWKSURI); err != nil {
		return nil, "", fmt.Errorf("refresh bot connector jwks: %w", err)
	}

	v.mu.Lock()
	if v.cache == nil {
		v.cache = cache
		v.jwksURL = meta.JWKSURI
		v.issuer = meta.Issuer
	}
	cache = v.cache
	jwksURL := v.jwksURL
	issuer := v.issuer
	v.mu.Unlock()

	set, err := cache.Get(ctx, jwksURL)
	if err != nil {
		return nil, "", fmt.Errorf("get bot connector jwks: %w", err)
	}
	return set, issuer, nil
}

func (v *IncomingTokenValidator) fetchMetadata(ctx context.Context) (*botOpenIDMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.MetadataURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build bot connector metadata request: %w", err)
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch bot connector metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bot connector metadata returned %d", resp.StatusCode)
	}
	var meta botOpenIDMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode bot connector metadata: %w", err)
	}
	if meta.Issuer == "" || meta.JWKSURI == "" {
		return nil, errors.New("bot connector metadata missing issuer or jwks_uri")
	}
	return &meta, nil
}

func (v *IncomingTokenValidator) httpClient() *http.Client {
	if v.HTTPClient != nil {
		return v.HTTPClient
	}
	return &http.Client{Timeout: botAuthHTTPTimeout}
}

func sameServiceURL(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	ub, err := url.Parse(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}
