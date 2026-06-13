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
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/shared/auth"
)

const testConversationNameGeneral = "general"

// channelInfoServer replies with a named channel for "C_ok" and missing_scope for
// anything else, and records the bearer token + channel of each request.
func channelInfoServer(t *testing.T) (srv *httptest.Server, auths func() []string) {
	t.Helper()
	var mu sync.Mutex
	var captured []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured = append(captured, r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Query().Get("channel") {
		case "C_ok":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C_ok","name":"` + testConversationNameGeneral + `"}}`))
			return
		case "G_mpim":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"G_mpim","name":"mpdm","is_mpim":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), captured...)
	}
}

func TestResolveChannelNameSeam_OKAndError(t *testing.T) {
	srv, auths := channelInfoServer(t)
	resolveInfo := newSlackResolveConversationInfoFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
	resolve := slackResolveChannelNameFromConversationInfo(resolveInfo)

	name, err := resolve(context.Background(), "T1", "", "C_ok")
	if err != nil || name != testConversationNameGeneral {
		t.Fatalf("ok resolve: name=%q err=%v", name, err)
	}
	gotAuths := auths()
	if gotAuths[0] != testBearerXoxb {
		t.Errorf("auth = %q, want %q", gotAuths[0], testBearerXoxb)
	}
	// ok:false (missing the channels:read scope, a DM, not_found, ...) surfaces as an
	// error → the caller treats it as "no name" and falls back to the channel id.
	if _, err := resolve(context.Background(), "T1", "", "C_x"); err == nil || !strings.Contains(err.Error(), "missing_scope") {
		t.Fatalf("expected missing_scope error, got %v", err)
	}
}

func TestResolveConversationInfoSeam_OKAndMPIM(t *testing.T) {
	srv, _ := channelInfoServer(t)
	resolve := newSlackResolveConversationInfoFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	ch, err := resolve(context.Background(), "T1", "", "C_ok")
	if err != nil || ch.Name != testConversationNameGeneral || ch.IsMPIM {
		t.Fatalf("channel info: got %+v err=%v, want name general non-mpim", ch, err)
	}
	mpim, err := resolve(context.Background(), "T1", "", "G_mpim")
	if err != nil || mpim.Name != "mpdm" || !mpim.IsMPIM {
		t.Fatalf("mpim info: got %+v err=%v, want name mpdm is_mpim", mpim, err)
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
	resolveInfo := newSlackResolveConversationInfoFuncWithTokenLookup(lookup, "qurl-slack/test", srv.URL, srv.Client())
	resolve := slackResolveChannelNameFromConversationInfo(resolveInfo)

	name, err := resolve(context.Background(), "T1", "E1", "C_ok")
	if err != nil || name != testConversationNameGeneral {
		t.Fatalf("grid fallback resolve: name=%q err=%v", name, err)
	}
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("grid fallback should try team then enterprise, got owners=%v", owners)
	}
}
