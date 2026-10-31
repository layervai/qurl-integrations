package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

func TestSlackAuthFailureInvalidatesOnlyRejectedOwner(t *testing.T) {
	ctx := context.Background()
	seams := map[string]func(slackBotTokenLookup, func(string), string, *http.Client) error{
		"views.open": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			return newSlackOpenViewFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "trigger", []byte(`{"type":"modal"}`))
		},
		"chat.postMessage": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			return newSlackPostMessageFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "", "C1", "", "hello")
		},
		"DM": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			return newSlackPostDMFuncWithTokenLookup(lookup, "test", url, url, client, purge)(ctx, "T_ROTATE", "", "U_TARGET", "hello")
		},
		"conversations.info": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			_, err := newSlackResolveConversationInfoFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "", "C1")
			return err
		},
		"conversations.replies": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			_, err := newSlackAgentThreadHistoryFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "", "C1", "1.0", "")
			return err
		},
		"users.info": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			_, err := newSlackUserLookupFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "", "U_TARGET")
			return err
		},
		"conversations.members": func(lookup slackBotTokenLookup, purge func(string), url string, client *http.Client) error {
			_, err := newSlackChannelMembershipFuncWithTokenLookup(lookup, "test", url, client, purge)(ctx, "T_ROTATE", "", "C1", "U_TARGET")
			return err
		},
	}
	for name, call := range seams {
		for _, code := range []string{"token_revoked", "invalid_auth", "account_inactive", "missing_scope", "ratelimited", "invalid_trigger"} {
			t.Run(name+"/"+code, func(t *testing.T) {
				shouldPurge := code == "token_revoked" || code == "invalid_auth" || code == "account_inactive"
				provider := &fakeSlackBotTokenProvider{token: testWorkspaceSlackBotToken}
				lookup, purge := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)
				for _, owner := range []string{"T_ROTATE", "T_KEEP"} {
					if _, err := lookup(ctx, owner); err != nil {
						t.Fatal(err)
					}
				}
				provider.token = testRotatedWorkspaceSlackBotToken
				var requests atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := requests.Add(1)
					want := testWorkspaceSlackBotToken
					if n > 1 && shouldPurge {
						want = testRotatedWorkspaceSlackBotToken
					}
					if r.Header.Get("Authorization") != "Bearer "+want {
						t.Error("request used the wrong token generation")
					}
					if n == 1 {
						_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
						return
					}
					_, _ = fmt.Fprint(w, `{"ok":true,"channel":{"id":"D1","name":"channel"},"user":{"id":"U_TARGET"},"members":["U_TARGET"],"messages":[]}`)
				}))
				defer srv.Close()
				if err := call(lookup, purge, srv.URL, slackWebAPITestClient(srv)); err == nil {
					t.Fatal("rejected API call returned success")
				}
				if requests.Load() != 1 {
					t.Fatal("failed call was replayed")
				}
				if err := call(lookup, purge, srv.URL, slackWebAPITestClient(srv)); err != nil {
					t.Fatalf("next API call: %v", err)
				}
				wantCalls := int64(2)
				if shouldPurge {
					wantCalls++
				}
				if provider.calls.Load() != wantCalls {
					t.Fatalf("provider reads = %d, want %d", provider.calls.Load(), wantCalls)
				}
				token, err := lookup(ctx, "T_KEEP")
				if err != nil || token != testWorkspaceSlackBotToken {
					t.Fatal("unrelated owner cache changed")
				}
				if provider.calls.Load() != wantCalls {
					t.Fatal("unrelated owner was fetched again")
				}
			})
		}
	}
}

func TestSlackAuthFailurePurgesGridOwner(t *testing.T) {
	ctx := context.Background()
	provider := &fakeSlackBotTokenProvider{token: testWorkspaceSlackBotToken}
	cached, purge := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)
	for _, owner := range []string{"E_ORG", "T_KEEP"} {
		if _, err := cached(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	provider.token = testRotatedWorkspaceSlackBotToken
	lookup := func(ctx context.Context, owner string) (string, error) {
		if owner == "T_MISSING" {
			return "", auth.ErrSlackBotTokenNotConfigured
		}
		return cached(ctx, owner)
	}
	var purged []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"token_revoked"}`)
	}))
	defer srv.Close()
	post := newSlackPostMessageFuncWithTokenLookup(lookup, "test", srv.URL, slackWebAPITestClient(srv), func(owner string) { purged = append(purged, owner); purge(owner) })
	if err := post(ctx, "T_MISSING", "E_ORG", "C1", "", "hello"); err == nil {
		t.Fatal("rejected Grid call returned success")
	}
	if len(purged) != 1 || purged[0] != "E_ORG" {
		t.Fatalf("purged owners = %v, want only the Grid owner", purged)
	}
	token, err := cached(ctx, "E_ORG")
	if err != nil || token != testRotatedWorkspaceSlackBotToken {
		t.Fatal("Grid token was not refreshed")
	}
	token, err = cached(ctx, "T_KEEP")
	if err != nil || token != testWorkspaceSlackBotToken {
		t.Fatal("unrelated owner cache changed")
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("provider reads = %d, want 3", provider.calls.Load())
	}
}
