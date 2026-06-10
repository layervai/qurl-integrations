package main

// Tests for the conversations.info channel-name seam (#659): a successful name
// read, an ok:false (e.g. missing_scope) surfaced as an error so the agent falls
// back to the channel id, and the Enterprise Grid org-token fallback shared with
// the chat.postMessage poster.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// channelInfoServer replies with a named channel for "C_ok" and missing_scope for
// anything else, and records the bearer token + channel of each request.
func channelInfoServer(t *testing.T) (srv *httptest.Server, auths *[]string) {
	t.Helper()
	var captured []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Header.Get("Authorization"))
		if r.URL.Query().Get("channel") == "C_ok" {
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C_ok","name":"general"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func TestResolveChannelNameSeam_OKAndError(t *testing.T) {
	srv, auths := channelInfoServer(t)
	resolve := newSlackResolveChannelNameFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	name, err := resolve(context.Background(), "T1", "", "C_ok")
	if err != nil || name != "general" {
		t.Fatalf("ok resolve: name=%q err=%v", name, err)
	}
	if (*auths)[0] != testBearerXoxb {
		t.Errorf("auth = %q, want %q", (*auths)[0], testBearerXoxb)
	}
	// ok:false (missing the channels:read scope, a DM, not_found, ...) surfaces as an
	// error → the caller treats it as "no name" and falls back to the channel id.
	if _, err := resolve(context.Background(), "T1", "", "C_x"); err == nil || !strings.Contains(err.Error(), "missing_scope") {
		t.Fatalf("expected missing_scope error, got %v", err)
	}
}

func TestResolveChannelNameSeam_GridFallback(t *testing.T) {
	srv, _ := channelInfoServer(t)
	var owners []string
	lookup := func(_ context.Context, ownerID string) (string, error) {
		owners = append(owners, ownerID)
		if ownerID == "T1" {
			return "", auth.ErrSlackBotTokenNotConfigured // workspace itself has no bot token
		}
		return "xoxb-org", nil
	}
	resolve := newSlackResolveChannelNameFuncWithTokenLookup(lookup, "qurl-slack/test", srv.URL, srv.Client())

	name, err := resolve(context.Background(), "T1", "E1", "C_ok")
	if err != nil || name != "general" {
		t.Fatalf("grid fallback resolve: name=%q err=%v", name, err)
	}
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("grid fallback should try team then enterprise, got owners=%v", owners)
	}
}
