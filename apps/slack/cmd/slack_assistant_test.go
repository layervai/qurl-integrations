package main

// Tests for the assistant.threads.* seam (container Slice 1): request shape (URL
// routing, {channel_id, thread_ts, title} for setTitle and {…, prompts:[{title,
// message}]} for setSuggestedPrompts, bearer token).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const testAssistantThreadTS = "100.1"

func TestSlackAssistantThreadsPort_RequestShapes(t *testing.T) {
	type captured struct {
		path, auth string
		body       map[string]any
	}
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		got = append(got, captured{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	port := newSlackAssistantThreadsPortWithTokenLookup(
		staticTokenLookup("xoxb-test"), "qurl-slack/test",
		srv.URL+"/setTitle", srv.URL+"/setSuggestedPrompts", srv.Client())

	if err := port.SetTitle(context.Background(), "T1", "", "D1", testAssistantThreadTS, "qURL Secure Access Agent"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := port.SetSuggestedPrompts(context.Background(), "T1", "", "D1", testAssistantThreadTS,
		[]internal.SuggestedPrompt{{Title: "What can I reach?", Message: "What can I reach?"}}); err != nil {
		t.Fatalf("SetSuggestedPrompts: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	// setTitle
	if tt := got[0]; tt.path != "/setTitle" || tt.auth != testBearerXoxb ||
		tt.body["channel_id"] != "D1" || tt.body["thread_ts"] != testAssistantThreadTS || tt.body["title"] != "qURL Secure Access Agent" {
		t.Errorf("setTitle request = %+v", tt)
	}
	// setSuggestedPrompts
	sp := got[1]
	if sp.path != "/setSuggestedPrompts" || sp.body["channel_id"] != "D1" || sp.body["thread_ts"] != testAssistantThreadTS {
		t.Errorf("setSuggestedPrompts request = %+v", sp)
	}
	prompts, ok := sp.body["prompts"].([]any)
	if !ok || len(prompts) != 1 {
		t.Fatalf("prompts = %+v", sp.body["prompts"])
	}
	if first, _ := prompts[0].(map[string]any); first["title"] != "What can I reach?" || first["message"] != "What can I reach?" {
		t.Errorf("prompt[0] = %+v", prompts[0])
	}
}
