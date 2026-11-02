package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestPolicyMutationReply(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		message    string
	}{
		{"stale authority", slackdata.ErrCodePolicyWorkspaceChanged, http.StatusConflict, "Workspace permissions changed"},
		{"operator repair", slackdata.ErrCodePolicyReconciliationRequired, http.StatusConflict, "operator reconciliation"},
		{"other conflict", slackdata.ErrCodeAdminAlreadyExists, http.StatusConflict, ""},
		{"wrong status", slackdata.ErrCodePolicyWorkspaceChanged, http.StatusServiceUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &slackdata.Error{Code: tc.code, StatusCode: tc.status, Title: "private internal detail"})
			reply := policyMutationReply(err)
			if tc.message == "" {
				if reply != "" {
					t.Fatal("unrelated error was intercepted")
				}
				return
			}
			if !strings.Contains(reply, tc.message) || strings.Contains(reply, "private internal detail") {
				t.Fatalf("reply=%q", reply)
			}
			if storeErrorLogLevel(context.Background(), err, slog.LevelError) != slog.LevelWarn {
				t.Fatal("expected policy conflict would page as outage")
			}
			for _, render := range []func(error) string{authErrorMessage, agentToggleSetError, func(e error) string { return sanitizeAPIError(e, "unrelated operation") }} {
				if render(err) != reply {
					t.Fatal("shared error renderer lost policy next step")
				}
			}
		})
	}
	if policyMutationReply(nil) != "" || policyMutationReply(errors.New("unavailable")) != "" {
		t.Fatal("non-policy error intercepted")
	}
}
