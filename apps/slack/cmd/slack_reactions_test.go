package main

// Tests for the reactions.add/remove seam (#661 working-on-it ack): request shape
// (URL routing, {channel,timestamp,name} body, bearer token), the Enterprise Grid
// token fallback (shared with the chat.postMessage poster), and the best-effort
// benign-error tolerance (already_reacted on add / no_reaction on remove read as
// success, so an idempotent re-ack isn't surfaced as a failure).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

// testBearerXoxb is the Authorization header for staticTokenLookup("xoxb-test").
const testBearerXoxb = "Bearer xoxb-test"

type capturedReaction struct {
	path, auth string
	body       map[string]string
}

// reactionTestServer records each request's path, Authorization header, and decoded
// body, replying with okJSON. Add and Remove are routed to /add and /remove.
func reactionTestServer(t *testing.T, okJSON string) (*httptest.Server, *[]capturedReaction) {
	t.Helper()
	var got []capturedReaction
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]string
		_ = json.Unmarshal(raw, &b)
		got = append(got, capturedReaction{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
		_, _ = w.Write([]byte(okJSON))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newTestReactionPort(lookup slackBotTokenLookup, addURL, removeURL string) internal.ReactionPort {
	return newSlackReactionPortWithTokenLookup(lookup, "qurl-slack/test", addURL, removeURL, nil)
}

func TestSlackReactionPort_AddPostsExpectedRequest(t *testing.T) {
	srv, got := reactionTestServer(t, `{"ok":true}`)
	port := newTestReactionPort(staticTokenLookup("xoxb-test"), srv.URL+"/add", srv.URL+"/remove")

	if err := port.Add(context.Background(), "T1", "", "C1", "100.1", "eyes"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("want 1 request, got %d", len(*got))
	}
	r := (*got)[0]
	if r.path != "/add" {
		t.Errorf("path = %q, want /add", r.path)
	}
	if r.auth != testBearerXoxb {
		t.Errorf("auth = %q, want %q", r.auth, testBearerXoxb)
	}
	if r.body["channel"] != "C1" || r.body["timestamp"] != "100.1" || r.body["name"] != "eyes" {
		t.Errorf("body = %+v, want channel=C1 timestamp=100.1 name=eyes", r.body)
	}
}

func TestSlackReactionPort_RemoveRoutesToRemoveURL(t *testing.T) {
	srv, got := reactionTestServer(t, `{"ok":true}`)
	port := newTestReactionPort(staticTokenLookup("xoxb-test"), srv.URL+"/add", srv.URL+"/remove")

	if err := port.Remove(context.Background(), "T1", "", "C1", "100.1", "eyes"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(*got) != 1 || (*got)[0].path != "/remove" {
		t.Fatalf("Remove should hit /remove, got %+v", *got)
	}
}

func TestSlackReactionPort_BenignErrorsTreatedAsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		okJSON string
		call   func(internal.ReactionPort) error
	}{
		{"add already_reacted", `{"ok":false,"error":"already_reacted"}`, func(p internal.ReactionPort) error {
			return p.Add(context.Background(), "T1", "", "C1", "100.1", "eyes")
		}},
		{"remove no_reaction", `{"ok":false,"error":"no_reaction"}`, func(p internal.ReactionPort) error {
			return p.Remove(context.Background(), "T1", "", "C1", "100.1", "eyes")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := reactionTestServer(t, c.okJSON)
			port := newTestReactionPort(staticTokenLookup("xoxb-test"), srv.URL+"/add", srv.URL+"/remove")
			if err := c.call(port); err != nil {
				t.Fatalf("benign idempotent outcome must read as success, got %v", err)
			}
		})
	}
}

func TestSlackReactionPort_RealErrorSurfaces(t *testing.T) {
	srv, _ := reactionTestServer(t, `{"ok":false,"error":"message_not_found"}`)
	port := newTestReactionPort(staticTokenLookup("xoxb-test"), srv.URL+"/add", srv.URL+"/remove")
	err := port.Add(context.Background(), "T1", "", "C1", "100.1", "eyes")
	if err == nil || !strings.Contains(err.Error(), "message_not_found") {
		t.Fatalf("a non-benign ok:false must surface, got %v", err)
	}
}

func TestSlackReactionPort_GridFallback(t *testing.T) {
	// Workspace token missing → retry with the Enterprise Grid org-install token, same
	// fallback the chat.postMessage poster uses (the seam shares the transport).
	srv, got := reactionTestServer(t, `{"ok":true}`)
	port := newTestReactionPort(func(_ context.Context, ownerID string) (string, error) {
		if ownerID == "T1" {
			return "", auth.ErrSlackBotTokenNotConfigured
		}
		return "xoxb-org", nil
	}, srv.URL+"/add", srv.URL+"/remove")

	if err := port.Add(context.Background(), "T1", "E1", "C1", "100.1", "eyes"); err != nil {
		t.Fatalf("Add with Grid fallback: %v", err)
	}
	if len(*got) != 1 || (*got)[0].auth != "Bearer xoxb-org" {
		t.Fatalf("fallback should post with the org-install token, got %+v", *got)
	}
}
