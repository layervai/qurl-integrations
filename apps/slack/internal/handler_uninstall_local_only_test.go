package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

func TestUninstallLocalOnlySignedCommandAndClick(t *testing.T) {
	for _, tc := range []struct {
		name, slashUser, clickUser, keyID, want string
		keyErr, deleteErr                       error
		wantCard, wantDelete                    bool
	}{
		{name: "disconnect despite upstream outage", keyID: "key_workspace_001", want: "key_workspace_001", wantCard: true, wantDelete: true},
		{name: "unknown legacy key", want: "No upstream key ID is available", wantCard: true, wantDelete: true},
		{name: "no configured key", keyErr: auth.ErrWorkspaceNotConfigured, deleteErr: auth.ErrWorkspaceNotConfigured, want: "NOT revoked", wantCard: true, wantDelete: true},
		{name: "key read failure preserves credentials", keyErr: errors.New("store unavailable"), want: "Nothing was disconnected", wantCard: true},
		{name: "local delete failure", keyID: "key_workspace_001", deleteErr: errors.New("delete unavailable"), want: "could not disconnect", wantCard: true, wantDelete: true},
		{name: "non-admin slash refused", slashUser: "USTRANGER000", want: "can only be run"},
		{name: "non-admin click refused", clickUser: "USTRANGER000", want: "can disconnect", wantCard: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &revokingAuthProvider{recordingAuthProvider: recordingAuthProvider{apiKey: "test-key", deleteErr: tc.deleteErr}, keyID: tc.keyID, keyIDErr: tc.keyErr}
			h, upstream := newUninstallRevokeTestHandler(t, provider, http.StatusServiceUnavailable)
			slashUser := tc.slashUser
			if slashUser == "" {
				slashUser = testAdminUserID
			}
			values := url.Values{fieldCommand: {commandUser}, fieldText: {"uninstall --local-only"}, fieldTeamID: {testAdminTeamID}, fieldUserID: {slashUser}, fieldChannelID: {testExposeChannel}}
			encoded := values.Encode()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackCommands, encoded, encoded))
			if w.Code != http.StatusOK {
				t.Fatalf("slash status=%d", w.Code)
			}
			var card map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
				t.Fatal(err)
			}
			rendered := w.Body.String()
			if !tc.wantCard {
				if strings.Contains(rendered, uninstallLocalOnlyConfirmActionID) || !strings.Contains(rendered, tc.want) {
					t.Fatalf("refusal=%s", rendered)
				}
			} else {
				if !strings.Contains(rendered, uninstallLocalOnlyConfirmActionID) || !strings.Contains(rendered, "NOT be revoked") || !strings.Contains(rendered, `"confirm"`) {
					t.Fatalf("missing local-only warning/confirmation: %s", rendered)
				}
				if provider.deleteCalls != 0 || provider.keyIDCalls != 0 {
					t.Fatal("slash command mutated or read the key before confirmation")
				}
				captured := &capturedResponseURL{}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					captured.record(b)
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(srv.Close)
				clickUser := tc.clickUser
				if clickUser == "" {
					clickUser = testAdminUserID
				}
				body := exposeBlockActionsBodyWithEnterprise(t, testAdminTeamID, "", clickUser, testExposeChannel, srv.URL, uninstallLocalOnlyConfirmActionID, testAdminTeamID)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
				if w.Code != http.StatusOK {
					t.Fatalf("click status=%d", w.Code)
				}
				reply := parseSlackText(t, captured.waitForBody(t, 2*time.Second))
				if !strings.Contains(reply, tc.want) {
					t.Fatalf("reply=%q want %q", reply, tc.want)
				}
				if tc.wantDelete && tc.deleteErr == nil && (!strings.Contains(reply, "NOT revoked") || !strings.Contains(reply, "account/API-key management")) {
					t.Fatalf("missing manual revoke warning: %q", reply)
				}
			}
			h.Wait()
			expected := 0
			if tc.wantDelete {
				expected = 1
			}
			if provider.deleteCalls != expected {
				t.Fatalf("local deletes=%d want%d", provider.deleteCalls, expected)
			}
			if tc.wantDelete && tc.deleteErr == nil {
				if provider.apiKey != "" {
					t.Fatal("local credential survived successful disconnect")
				}
			} else if provider.apiKey != "test-key" {
				t.Fatal("refused or failed disconnect removed the credential")
			}
			if calls, _ := upstream.snapshot(); calls != 0 {
				t.Fatalf("local-only attempted%d upstream DELETEs", calls)
			}
		})
	}
}

func TestUninstallLocalOnlyKeepsGuardedLifecyclePurge(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	reply, scheduled := h.uninstallWorkspaceReply(context.Background(), testAdminTeamID, testAdminUserID, []string{testAdminTeamID}, true)
	if !scheduled || !strings.Contains(reply, "NOT revoked") {
		t.Fatalf("local-only purge not scheduled: %q", reply)
	}
	h.Wait()
	if provider.deleteCalls != 1 || provider.deleteStateCalls != 0 {
		t.Fatalf("local deletes=%d state deletes=%d", provider.deleteCalls, provider.deleteStateCalls)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testAdminTeamID, "C_one")
	if err != nil || len(entries) != 0 {
		t.Fatalf("channel policy remains: %v %v", entries, err)
	}
}

func TestUninstallLocalOnlyRejectsExtraArguments(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	h := newUninstallAdminTestHandler(t, provider)
	reply := slashResponseForWorkspaceUser(t, h, commandUser, "uninstall --local-only extra", testAdminTeamID, testAdminUserID)
	h.Wait()
	if provider.deleteCalls != 0 || provider.apiKey != "test-key" || !strings.Contains(reply[respFieldText], "Usage:") {
		t.Fatalf("unexpected argument mutation or reply: %v", reply)
	}
}
