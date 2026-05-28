package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

func TestSlackOpenViewFuncPostsViewsOpenPayload(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotUA string
	var gotBody struct {
		TriggerID string          `json:"trigger_id"`
		View      json.RawMessage `json:"view"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "qurl-slack/test", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
	if gotUA != "qurl-slack/test" {
		t.Fatalf("User-Agent = %q, want qurl-slack/test", gotUA)
	}
	if gotBody.TriggerID != "trigger_test" {
		t.Fatalf("trigger_id = %q, want trigger_test", gotBody.TriggerID)
	}
	if string(gotBody.View) != `{"type":"modal"}` {
		t.Fatalf("view = %s, want raw modal JSON", string(gotBody.View))
	}
}

func TestSlackOpenViewFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_trigger"}`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if !errors.Is(err, internal.ErrSlackTriggerExpired) || !strings.Contains(err.Error(), "invalid_trigger") {
		t.Fatalf("error = %v, want trigger-expired sentinel wrapping invalid_trigger", err)
	}
}

func TestSlackOpenViewFuncRejectsInvalidViewJSON(t *testing.T) {
	t.Parallel()

	err := slackOpenViewFuncWithURL("xoxb-test", "", "https://slack.invalid/views.open")(context.Background(), "T_test", "trigger_test", []byte(`not-json`))
	if err == nil || !strings.Contains(err.Error(), "invalid view JSON") {
		t.Fatalf("error = %v, want invalid view JSON", err)
	}
}

func TestSlackOpenViewFuncSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v, want HTTP 502", err)
	}
}

func TestSlackOpenViewFuncSurfacesMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want response JSON", err)
	}
}

func TestSlackOpenViewFuncSurfacesOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeded 4096 bytes") {
		t.Fatalf("error = %v, want oversized response", err)
	}
}
