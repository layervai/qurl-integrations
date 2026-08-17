package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestAgentThreadHistorySeam_FullFileObjectShape runs the seam over file entries
// carrying the field set the 2026-08-14 measurement recorded — hosted, external,
// snippet, canvas and access_denied — instead of the {"id":"F1"} stub the tests above
// use. The fixture is built to that documented shape rather than captured from Slack:
// reading the live wire format is cmd/slack-history-upload-smoke's job, and this is
// what keeps the offline decode chain honest between runs of it.
//
// What it adds over the stub is depth: entries with nested objects, explicit nulls and
// keys this app has never heard of. {"id":"F1"} is a shape Slack never sends, so a
// decode that handles it proves nothing about one that arrives. Measured, not assumed —
// a decoder mutated to skip entries whose metadata came back explicitly null fails only
// this test; TestAgentThreadHistorySeam_ReportsAttachments above stays green, because
// its stub has no null to skip.
//
// The access_denied entry is what does that work. Every piece of its metadata is null
// and it still occupies an array slot, which is exactly why presence detection survives
// on a token without files:read.
func TestAgentThreadHistorySeam_FullFileObjectShape(t *testing.T) {
	t.Parallel()

	payload, err := os.ReadFile(filepath.Join("testdata", "conversations_replies_uploads.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	read := newSlackAgentThreadHistoryFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())
	messages, err := read(context.Background(), "T1", "", "C1", "1723600000.000100", "")
	if err != nil {
		t.Fatalf("read history: %v", err)
	}

	want := []struct {
		note     string
		hasFiles bool
	}{
		{"plain question", false},
		{"caption plus a hosted PDF", true},
		{"external Drive file", true},
		{"snippet, which is what Slack makes of a long paste", true},
		{"canvas, which history reports even though the event does not", true},
		{"a file the token may not read", true},
		{"one readable and one denied entry", true},
		{"an empty files array", false},
		{"the agent's own reply", false},
	}
	if len(messages) != len(want) {
		t.Fatalf("messages = %d, want %d", len(messages), len(want))
	}
	for i, expected := range want {
		if messages[i].HasFiles != expected.hasFiles {
			t.Errorf("message %d (%s) HasFiles = %v, want %v", i, expected.note, messages[i].HasFiles, expected.hasFiles)
		}
	}
	// The file-only turn keeps its empty text rather than picking one up from the decode;
	// noteAgentHistoryAttachment is what gives it something to say.
	if messages[3].Text != "" {
		t.Errorf("file-only message text = %q, want it left empty for the note to fill", messages[3].Text)
	}
	if messages[1].Text != "protect everything in this" {
		t.Errorf("caption = %q, want the text carried alongside the files decode", messages[1].Text)
	}

	// Pin the fixture to the surface it claims to describe. The measurement found
	// file_share ZERO times in 4,668 history messages: on this surface the array is the
	// whole signal. A fixture that quietly grew a subtype would start proving the
	// classifier works on a shape this API does not send, which is worse than no fixture.
	var raw struct {
		Messages []struct {
			Subtype string          `json:"subtype"`
			Files   json.RawMessage `json:"files"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for i, message := range raw.Messages {
		if message.Subtype != "" {
			t.Errorf("fixture message %d has subtype %q; this surface was measured to send none", i, message.Subtype)
		}
		if want[i].hasFiles && len(message.Files) == 0 {
			t.Errorf("fixture message %d is expected to carry files but has no files value", i)
		}
	}
}
