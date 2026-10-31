package internal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type workspaceLookupSpy struct {
	recordingAuthProvider
	calls atomic.Int32
}

func (p *workspaceLookupSpy) APIKey(ctx context.Context, workspace string) (string, error) {
	p.calls.Add(1)
	return p.recordingAuthProvider.APIKey(ctx, workspace)
}

func TestSlashMissingWorkspaceRejectsBeforeCredentialLookup(t *testing.T) {
	for _, workspace := range []string{"", "   "} {
		t.Run(workspace, func(t *testing.T) {
			server, hits := countingQURLServer(t)
			h := newTestHandler(t, server)
			provider := &workspaceLookupSpy{}
			h.cfg.AuthProvider = provider
			body := url.Values{fieldTeamID: {workspace}, fieldCommand: {commandUser}, fieldText: {"list"}}.Encode()
			response := httptest.NewRecorder()
			h.ServeHTTP(response, newSignedRequest(t, pathSlackCommands, body, body))
			h.Wait()
			var ack map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ack[respFieldText], "missing Slack workspace ID") {
				t.Fatalf("missing synchronous workspace error: %s", response.Body.String())
			}
			if provider.calls.Load() != 0 || hits.Load() != 0 {
				t.Fatal("missing workspace reached credentials or API")
			}
		})
	}
}

func TestSlashEmptyTextWithWorkspaceShowsHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := url.Values{fieldTeamID: {"T123"}, fieldCommand: {commandUser}, fieldText: {""}}.Encode()
	response := httptest.NewRecorder()
	h.ServeHTTP(response, newSignedRequest(t, pathSlackCommands, body, body))
	h.Wait()
	var ack map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ack[respFieldText], "/qurl help") {
		t.Fatalf("empty-text reply = %s", response.Body.String())
	}
}
