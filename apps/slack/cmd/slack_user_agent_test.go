package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSlackDefaultUserAgentWireProduct(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("User-Agent"); got != "qurl-bot-slack/unknown" {
			t.Errorf("User-Agent = %q, want the Slack bot wire product", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", server.URL, server.Client())
	if err := post(context.Background(), "T1", "", "C1", "", "hello"); err != nil {
		t.Fatal(err)
	}
}
