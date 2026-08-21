package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
)

// captureHandler records slog output for classification assertions.
type captureHandler struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error { //nolint:gocritic // hugeParam: slog.Handler pins this signature.
	rec := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]string(nil), h.records...)
}

func TestLogRegistrationFailureClassifiesPolicyDenials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err    error
		reason string
	}{
		{qurl.ErrOTPIncorrect, "otp_incorrect"},
		{qurl.ErrOTPExpired, "otp_expired"},
		{qurl.ErrRegistrationRateLimited, "registration_rate_limited"},
		{qurl.ErrDeviceKeyQuotaExceeded, "device_key_quota_exceeded"},
		{qurl.ErrKeyRejected, "registration_key_rejected"},
		{qurl.ErrAgentIdentityConflict, "agent_identity_conflict"},
		{qurl.ErrNoAccountEmail, "account_email_missing"},
		{qurl.ErrRegistrationKeyKindDisallowed, "registration_key_kind_disallowed"},
		{qurl.ErrRegistrationInvalidInput, "registration_invalid_input"},
		{qurl.ErrRegistrationDisabled, "registration_disabled"},
		{qurl.ErrAssignmentIdentityRejected, "assignment_identity_rejected"},
		{qurl.ErrAssignmentQuotaExceeded, "assignment_quota_exceeded"},
		{qurl.ErrAssignmentRateLimited, "assignment_rate_limited"},
		{qurl.ErrAssignmentRequestRejected, "assignment_request_rejected"},
		{qurl.ErrAssignmentKeyRejected, "assignment_key_rejected"},
		{qurl.ErrAssignmentRegistrationDisabled, "assignment_registration_disabled"},
		{qurl.ErrAssignmentBootstrapConsumed, "assignment_bootstrap_consumed"},
		{qurl.ErrAssignmentTicketInvalid, "assignment_ticket_invalid"},
		{qurl.ErrAssignmentTicketExpired, "assignment_ticket_expired"},
		{qurl.ErrCompletionIdentityRejected, "completion_identity_rejected"},
		{qurl.ErrCompletionCredentialConflict, "completion_credential_conflict"},
		{qurl.ErrCompletionRequestRejected, "completion_request_rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			h := &captureHandler{}
			logRegistrationFailure(context.Background(), slog.New(h), fmt.Errorf("wrap: %w", tt.err), "secret-token")
			recs := h.snapshot()
			if len(recs) != 1 {
				t.Fatalf("log records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec["event"] != "native_registration_deny" || rec["reason"] != tt.reason {
				t.Fatalf("denial record = %v, want deny/%s", rec, tt.reason)
			}
			// Denials intentionally omit the free-form SDK error text.
			if _, ok := rec["err"]; ok {
				t.Fatalf("denial record carries err text: %v", rec)
			}
		})
	}
}

func TestLogRegistrationFailureRedactsCredentialInDiagnostics(t *testing.T) {
	t.Parallel()
	h := &captureHandler{}
	err := fmt.Errorf("transport said no for token secret-token: %w", qurl.ErrEndpointNoReply)
	logRegistrationFailure(context.Background(), slog.New(h), err, "secret-token")
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("log records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec["event"] != "native_registration_error" || rec["reason"] != "native_registration_failed" {
		t.Fatalf("diagnostic record = %v", rec)
	}
	if got := rec["err"]; got == "" || !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "secret-token") {
		t.Fatalf("diagnostic err = %q, want the credential redacted", got)
	}
}

func TestIsSilentRegistrationStall(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		context.DeadlineExceeded,
		qurl.ErrEndpointNoReply,
		qurl.ErrAssignmentUnavailable,
		qurl.ErrAssignmentRecoveryRequired,
		qurl.ErrAssignmentReassignmentRequired,
		qurl.ErrAssignmentEndpointContinuity,
		qurl.ErrCompletionUnavailable,
		qurl.ErrCompletionRecoveryRequired,
		qurl.ErrRegistrationRecoveryRequired,
		qurl.ErrCredentialRecoveryRequired,
	} {
		if !isSilentRegistrationStall(fmt.Errorf("wrap: %w", err)) {
			t.Errorf("isSilentRegistrationStall(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		qurl.ErrKeyRejected,
		errors.New("some other failure"),
		context.Canceled, // parent-liveness is the caller's judgment, not this helper's
	} {
		if isSilentRegistrationStall(err) {
			t.Errorf("isSilentRegistrationStall(%v) = true, want false", err)
		}
	}
}

func TestRegistrationStalledErrorShape(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("leg 2: %w", qurl.ErrEndpointNoReply)
	err := &registrationStalledError{cause: cause}
	if !errors.Is(err, qurl.ErrEndpointNoReply) {
		t.Fatal("stalled error does not unwrap to its cause")
	}
	msg := err.Error()
	for _, want := range []string{"did not complete", "UDP/443", EnvEnrollmentToken, "one-shot"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stalled error %q missing %q", msg, want)
		}
	}
}
