package internal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	oauthRebindTestTeamID  = "TISSUE526"
	oauthRebindOwnerUserID = "UOWNER526"
	oauthRebindOtherUserID = "UOTHER526"
	oauthRebindAdminEmail  = "admin@example.com"
	oauthRebindCookiePath  = "/oauth/qurl"
)

func TestOAuthCallbackRefusesNonOwnerRebindWithRealWorkspaceStore(t *testing.T) {
	now := time.Unix(1700000000, 0)
	names := defaultTestTableNames()
	ddb := newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{
		names.workspace: {
			seedWorkspaceAdmin(oauthRebindTestTeamID, oauthRebindOwnerUserID, oauthRebindOwnerUserID, now),
		},
	})
	adminStore := newStoreFromFake(t, ddb, names, nil)
	workspaceStore := &oauthRebindWorkspaceStore{}
	minter := &oauthRebindMinter{}
	secret := []byte("01234567890123456789012345678901")
	cfg := oauth.Config{
		Auth0Domain:       "auth0.example.test",
		Auth0ClientID:     "client-id",
		Auth0ClientSecret: "client-secret",
		Auth0Audience:     "aud",
		SlackBaseURL:      "https://slack.example.test",
		OAuthStateSecret:  secret,
		Provider:          workspaceStore,
		IDTokenVerifier:   oauthRebindIDTokenVerifier{},
		Minter:            minter,
		AdminStore:        NewOAuthAdminStoreAdapter(adminStore),
		BindClassifyError: ClassifyOAuthBindError,
		HTTPClient:        &http.Client{Transport: oauthRebindTokenTransport{}, Timeout: 5 * time.Second},
		Now:               func() time.Time { return now },
	}

	state, err := oauth.MintState(secret, oauthRebindTestTeamID, oauthRebindOtherUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}

	startRec := httptest.NewRecorder()
	oauth.Start(cfg)(startRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, oauth.StartPath+"?state="+url.QueryEscape(state), http.NoBody))
	if startRec.Code != http.StatusFound {
		t.Fatalf("start status: got %d want 302 (body=%s)", startRec.Code, startRec.Body.String())
	}

	startResult := startRec.Result()
	defer func() { _ = startResult.Body.Close() }()

	stateCookie := oauthRebindStateCookie(startResult.Cookies())
	if stateCookie == nil {
		t.Fatal("start response did not include the OAuth state cookie")
	}
	callbackReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	for _, c := range startResult.Cookies() {
		callbackReq.AddCookie(c)
	}
	callbackRec := httptest.NewRecorder()
	oauth.Callback(cfg)(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusConflict {
		t.Fatalf("callback status: got %d want 409 (body=%s)", callbackRec.Code, callbackRec.Body.String())
	}
	if body := callbackRec.Body.String(); !strings.Contains(body, "qURL setup blocked") {
		t.Fatalf("callback body missing rebind-refused headline: %s", body)
	}
	if workspaceStore.setCalls != 0 {
		t.Fatalf("SetAPIKeyWithMetadata calls: got %d want 0", workspaceStore.setCalls)
	}
	if minter.mintCalls != 0 {
		t.Fatalf("MintWorkspaceAPIKey calls: got %d want 0", minter.mintCalls)
	}
	callbackResult := callbackRec.Result()
	defer func() { _ = callbackResult.Body.Close() }()
	if !oauthRebindCookieCleared(callbackResult.Cookies(), stateCookie.Name) {
		t.Fatal("callback did not clear OAuth state cookie on refused rebind")
	}

	isAdmin, ownerID, err := adminStore.CheckAdmin(context.Background(), oauthRebindTestTeamID, oauthRebindOwnerUserID)
	if err != nil {
		t.Fatalf("CheckAdmin(owner): %v", err)
	}
	if !isAdmin {
		t.Fatal("original owner should remain an admin after refused rebind")
	}
	if ownerID != oauthRebindOwnerUserID {
		t.Fatalf("owner_id: got %q want %q", ownerID, oauthRebindOwnerUserID)
	}
	isAdmin, ownerID, err = adminStore.CheckAdmin(context.Background(), oauthRebindTestTeamID, oauthRebindOtherUserID)
	if err != nil {
		t.Fatalf("CheckAdmin(other): %v", err)
	}
	if isAdmin {
		t.Fatal("non-owner rebind caller should not be added to admin_slack_user_ids")
	}
	if ownerID != oauthRebindOwnerUserID {
		t.Fatalf("owner_id after non-owner check: got %q want %q", ownerID, oauthRebindOwnerUserID)
	}
}

func oauthRebindStateCookie(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Path == oauthRebindCookiePath && c.MaxAge > 0 {
			return c
		}
	}
	return nil
}

func oauthRebindCookieCleared(cookies []*http.Cookie, name string) bool {
	for _, c := range cookies {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

type oauthRebindWorkspaceStore struct {
	setCalls int
}

func (s *oauthRebindWorkspaceStore) APIKey(context.Context, string) (string, error) {
	return "", auth.ErrWorkspaceNotConfigured
}

func (s *oauthRebindWorkspaceStore) APIKeyID(context.Context, string) (string, error) {
	return "", auth.ErrWorkspaceNotConfigured
}

func (s *oauthRebindWorkspaceStore) APIKeyIdentity(context.Context, string) (keyID, qurlAccountID string, err error) {
	return "", "", auth.ErrWorkspaceNotConfigured
}

func (s *oauthRebindWorkspaceStore) SetAPIKeyWithMetadata(context.Context, string, string, string, string, string, string) error {
	s.setCalls++
	return nil
}

func (s *oauthRebindWorkspaceStore) DeleteAPIKey(context.Context, string) error {
	return nil
}

type oauthRebindMinter struct {
	mintCalls int
}

func (m *oauthRebindMinter) ValidateAPIKey(context.Context, string) error {
	return nil
}

func (m *oauthRebindMinter) MintWorkspaceAPIKey(context.Context, string, string) (oauth.WorkspaceAPIKeyMint, error) {
	m.mintCalls++
	return oauth.WorkspaceAPIKeyMint{}, nil
}

func (m *oauthRebindMinter) MintWorkspaceReplacementAPIKey(context.Context, string, string, string) (oauth.WorkspaceAPIKeyMint, error) {
	return oauth.WorkspaceAPIKeyMint{}, nil
}

func (m *oauthRebindMinter) RevokeAPIKey(context.Context, string, string) error {
	return nil
}

func (m *oauthRebindMinter) APIKeyRevoked(context.Context, string, string) (bool, error) {
	return false, nil
}

type oauthRebindIDTokenVerifier struct{}

func (oauthRebindIDTokenVerifier) VerifyEmail(context.Context, string) (string, error) {
	return oauthRebindAdminEmail, nil
}

func (oauthRebindIDTokenVerifier) VerifySub(context.Context, string) (string, error) {
	return "auth0|issue-526", nil
}

type oauthRebindTokenTransport struct{}

func (oauthRebindTokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"auth0-access","id_token":"auth0-id-token"}`)),
	}, nil
}
