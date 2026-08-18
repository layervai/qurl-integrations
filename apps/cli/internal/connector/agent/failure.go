package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

// registrationStalledError enriches qurl-go's own transport diagnostic for a
// first-time native registration that did not complete because the NHP Hub or
// assigned cell stopped answering. qurl-go already bounds and reports the
// transport failure precisely (which endpoint stayed silent and for how
// long); this adds the Connector-specific things to check and unwraps to the
// qurl-go cause.
//
// It deliberately does NOT assert a bind mismatch. Empirically the failure is
// dominated by transport (an unreachable endpoint, blocked UDP egress, a
// wrong Hub pin); the assignment leg that validates the credential is
// consistent, and the enrollment credential is an opaque server-minted string
// the client cannot introspect anyway. The token line is a secondary "also
// check" — a consumed or expired one-shot token is a real, common cause — not
// a claim.
type registrationStalledError struct {
	cause error
}

func (e *registrationStalledError) Error() string {
	var b strings.Builder
	b.WriteString("native NHP registration did not complete: the NHP Hub or assigned cell stopped answering. ")
	fmt.Fprintf(&b, "This is usually transport — blocked UDP/443 egress, a wrong %s pin, or a transiently unreachable endpoint; ", hub.EnvHost)
	fmt.Fprintf(&b, "if it persists, also confirm the enrollment --token / %s was minted for this Connector and is not expired or already consumed (it is one-shot). ", EnvEnrollmentToken)
	b.WriteString("Increase log verbosity to see which leg stalled")
	if e.cause != nil {
		fmt.Fprintf(&b, ": %v", e.cause)
	}
	return b.String()
}

func (e *registrationStalledError) Unwrap() error { return e.cause }

// isSilentRegistrationStall reports whether a first-registration failure is a
// silent transport stall rather than an authenticated policy denial:
// qurl-go's no-reply signal, one of its bounded-budget recovery-required
// sentinels, or a deadline exceeded (qurl-go's own per-leg budget firing,
// surfaced through the context this client passed). An authenticated denial
// already carries its own specific message and is deliberately left
// untouched.
func isSilentRegistrationStall(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, qurl.ErrEndpointNoReply) {
		return true
	}
	for _, sentinel := range registrationRecoveryErrors {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

type registrationDenial struct {
	err    error
	reason string
}

// registrationDenials maps qurl-go's authenticated registration/assignment
// policy denials to the stable reason vocabulary shared with the standalone
// qURL Connector's decision stream, so dashboards can pivot across both.
var registrationDenials = []registrationDenial{
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

// registrationRecoveryErrors are qurl-go's explicit bounded-budget recovery
// sentinels: not denials, but "the transaction did not conclude; recovery or
// retry is required".
var registrationRecoveryErrors = []error{
	qurl.ErrAssignmentUnavailable,
	qurl.ErrAssignmentRecoveryRequired,
	qurl.ErrAssignmentReassignmentRequired,
	qurl.ErrAssignmentEndpointContinuity,
	qurl.ErrCompletionUnavailable,
	qurl.ErrCompletionRecoveryRequired,
	qurl.ErrRegistrationRecoveryRequired,
	qurl.ErrCredentialRecoveryRequired,
}

// logRegistrationFailure preserves the stable outcome split from the
// standalone Connector's decision stream: authenticated policy decisions are
// denials and intentionally omit the free-form SDK error text; transport,
// malformed-wire, crypto, persistence, local configuration, and
// explicit-recovery failures remain diagnostic errors. The enrollment
// credential is redacted from anything logged.
func logRegistrationFailure(ctx context.Context, logger *slog.Logger, err error, enrollmentCredential string) {
	for _, sentinel := range registrationRecoveryErrors {
		if errors.Is(err, sentinel) {
			logger.WarnContext(ctx, "connector: native registration failed",
				"event", "native_registration_error",
				"reason", "native_registration_failed",
				"err", redactSecret(errorString(err), enrollmentCredential))
			return
		}
	}
	for _, denial := range registrationDenials {
		if errors.Is(err, denial.err) {
			logger.WarnContext(ctx, "connector: native registration denied",
				"event", "native_registration_deny",
				"reason", denial.reason)
			return
		}
	}
	logger.WarnContext(ctx, "connector: native registration failed",
		"event", "native_registration_error",
		"reason", "native_registration_failed",
		"err", redactSecret(errorString(err), enrollmentCredential))
}

func errorString(err error) string {
	if err == nil {
		return "native registration failed without an error"
	}
	return err.Error()
}

func redactSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[REDACTED]")
}
