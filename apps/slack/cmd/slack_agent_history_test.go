package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/shared/auth"
)

func TestAgentThreadHistorySeam_PaginatesAndMapsMessages(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var queries []string
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()

		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"app_id":"A1","bot_id":"B1","user":"UQURL","text":"first","ts":"100.1"}],"response_metadata":{"next_cursor":"next page"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"user":"U1","text":"second","ts":"100.2"}],"response_metadata":{"next_cursor":""}}`))
	}))
	t.Cleanup(srv.Close)

	read := newSlackAgentThreadHistoryFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
	messages, err := read(context.Background(), "T1", "", "C1", "100.0", "70.000000")
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want 2", messages)
	}
	if messages[0].AppID != "A1" || messages[0].BotID != "B1" || messages[0].UserID != "UQURL" || messages[0].Text != "first" || messages[0].TS != "100.1" {
		t.Fatalf("first message = %#v", messages[0])
	}
	if messages[1].UserID != "U1" || messages[1].Text != "second" {
		t.Fatalf("second message = %#v", messages[1])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("queries = %v, want 2", queries)
	}
	for _, rawQuery := range queries {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/?"+rawQuery, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		query := req.URL.Query()
		if query.Get("channel") != "C1" || query.Get("ts") != "100.0" || query.Get("oldest") != "70.000000" || query.Get("limit") != "200" {
			t.Errorf("query = %v", query)
		}
	}
	if !strings.Contains(queries[1], "cursor=next+page") {
		t.Errorf("second query = %q, want escaped cursor", queries[1])
	}
	if len(authHeaders) != 2 || authHeaders[0] != testBearerXoxb || authHeaders[1] != testBearerXoxb {
		t.Errorf("authorization headers = %v", authHeaders)
	}
}

func TestAgentThreadHistorySeam_GridFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	}))
	t.Cleanup(srv.Close)

	var owners []string
	var mu sync.Mutex
	lookup := func(_ context.Context, ownerID string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		owners = append(owners, ownerID)
		if ownerID == "T1" {
			return "", auth.ErrSlackBotTokenNotConfigured
		}
		return testTokenXoxbOrg, nil
	}
	read := newSlackAgentThreadHistoryFuncWithTokenLookup(lookup, "qurl-slack/test", srv.URL, srv.Client())
	if _, err := read(context.Background(), "T1", "E1", "C1", "100.0", ""); err != nil {
		t.Fatalf("read history: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("owners = %v, want [T1 E1]", owners)
	}
}

// TestAgentThreadHistorySeam_ReportsAttachments pins that the seam carries the
// attachment signal through, not just the text. The upload's own turn is refused
// with the text-only limitation, so a caption whose HasFiles is lost here comes
// back on the next turn in that thread as an ordinary message. Both signals Slack
// can send are checked, because it does not promise to send both, and a shape the
// classifier cannot count must still report an attachment rather than fall through
// to "text only".
func TestAgentThreadHistorySeam_ReportsAttachments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"messages":[` +
			`{"user":"U1","text":"plain","ts":"100.1"},` +
			`{"user":"U1","text":"files array","ts":"100.2","files":[{"id":"F1"}]},` +
			`{"user":"U1","text":"subtype only","ts":"100.3","subtype":"file_share"},` +
			`{"user":"U1","text":"uncountable shape","ts":"100.4","files":{"id":"F1"}},` +
			`{"user":"U1","text":"empty array","ts":"100.5","files":[]},` +
			`{"user":"U1","text":"null files","ts":"100.6","files":null}` +
			`]}`))
	}))
	t.Cleanup(srv.Close)

	read := newSlackAgentThreadHistoryFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
	messages, err := read(context.Background(), "T1", "", "C1", "100.0", "")
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	want := []bool{false, true, true, true, false, false}
	if len(messages) != len(want) {
		t.Fatalf("messages = %#v, want %d", messages, len(want))
	}
	for i, wantFiles := range want {
		if messages[i].HasFiles != wantFiles {
			t.Errorf("message %q HasFiles = %v, want %v", messages[i].Text, messages[i].HasFiles, wantFiles)
		}
		if messages[i].Text == "" {
			t.Errorf("message %d lost its text alongside the files decode: %#v", i, messages[i])
		}
	}
}

func TestAgentThreadHistorySeam_RejectsSlackErrorAndUnboundedHistory(t *testing.T) {
	t.Parallel()

	t.Run("Slack error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
		}))
		t.Cleanup(srv.Close)
		read := newSlackAgentThreadHistoryFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
		if _, err := read(context.Background(), "T1", "", "C1", "100.0", ""); err == nil || !strings.Contains(err.Error(), "missing_scope") {
			t.Fatalf("error = %v, want missing_scope", err)
		}
	})

	t.Run("page cap", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []any{},
				"response_metadata": map[string]string{
					"next_cursor": "more",
				},
			})
		}))
		t.Cleanup(srv.Close)
		read := newSlackAgentThreadHistoryFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
		if _, err := read(context.Background(), "T1", "", "C1", "100.0", ""); err == nil || !strings.Contains(err.Error(), "exceeded 5 pages") {
			t.Fatalf("error = %v, want page-cap error", err)
		}
	})
}
