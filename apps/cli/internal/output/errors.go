package output

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionconfig"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// RenderError writes the customer-facing rendering of err to w (stderr).
// Structured qURL API problems get the full anatomy — headline, detail,
// per-field errors, an actionable hint, and the request id for support.
// Everything passes through redaction so a credential echoed back by a
// server (or embedded in a wrapped error) never reaches the terminal.
func RenderError(w io.Writer, err error, color bool) {
	p := &Printer{err: w, color: color}
	for _, line := range renderErrorLines(p, err) {
		// Best-effort: error rendering must never mask the original failure.
		_, _ = fmt.Fprintln(w, qurlapi.Redact(line))
	}
}

func renderErrorLines(p *Printer, err error) []string {
	head := p.style(ansiRed+ansiBold, errorPrefix)

	// Typed service postures come before the generic API-problem rendering:
	// their chains contain an API error too, but the posture is the message.
	var apiErr *qurlapi.Error
	// TODO(upstream-contract): This code is owned by qurl-service. Keep the
	// fixed redacted rendering in lockstep with that API contract.
	if errors.As(err, &apiErr) && strings.EqualFold(apiErr.Code, "connector_stopped") {
		return []string{head + " " + msgConnectorStopped, "", "  " + p.dim(hintConnectorStopped)}
	}
	if errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		return []string{head + " " + msgLinksUnavailable}
	}
	if errors.Is(err, auth.ErrNoCredential) {
		return []string{head + " " + msgNoCredential, "", "  " + p.dim(hintNoCredential)}
	}
	if lines, ok := connectorErrorLines(p, head, err); ok {
		return lines
	}

	var userMessage interface{ UserMessage() string }
	if errors.As(err, &userMessage) {
		lines := []string{head + " " + userMessage.UserMessage()}
		var apiErr *qurlapi.Error
		if errors.As(err, &apiErr) && apiErr.RequestID != "" {
			lines = append(lines, "  "+p.dim("Request ID: "+apiErr.RequestID))
		}
		return lines
	}

	if errors.As(err, &apiErr) {
		return apiErrorLines(p, head, apiErr)
	}
	return []string{head + " " + err.Error()}
}

// connectorErrorLines is the customer-language translation of the Connector
// lifecycle sentinels and of qurl-go's enrollment/assignment taxonomy: a
// plain-language headline, an optional operator-facing detail, and the one
// §17.1 hint. Errors whose SDK text can contain private routing, identity, or
// protocol details omit that block and use only fixed customer language.
//
// Every case matches with errors.Is, so a sentinel keeps rendering after the
// enroll/refresh path wraps it (`refresh native assignment binding: %w`) or
// after the SDK returns it inside an *AssignmentError. A mapping that only
// fired on a bare sentinel would be dead code on the real path.
func connectorErrorLines(p *Printer, head string, err error) ([]string, bool) { //nolint:gocyclo // Keep ordered terminal-cause rendering in one boundary.
	headline, hint, includeDetail := "", "", true
	if resourceHeadline, resourceHint, ok := connectorResourceErrorPosture(err); ok {
		return renderConnectorResourcePosture(p, head, err, resourceHeadline, resourceHint), true
	}
	switch {
	case errors.Is(err, hub.ErrConfig):
		headline, hint, includeDetail = msgConnectorHubConfig, hintConnectorHubConfig, false
	case errors.Is(err, sessionconfig.ErrConfig):
		headline, hint = msgConnectorSessionConfig, hintConnectorSessionConfig
	case errors.Is(err, qurl.ErrDeviceCredentialMissing),
		errors.Is(err, qurl.ErrCredentialRecoveryRequired):
		// NativeCredentialRecoveryRequiredError names the durable agent and Go
		// recovery APIs. Neither belongs on the CLI customer surface.
		headline, hint, includeDetail = msgConnectorDeviceCredential, hintConnectorDeviceCredential, false
	case errors.Is(err, qurl.ErrEndpointNoReply):
		// EndpointNoReplyError names the private logical destination and offers
		// topology-specific SDK guidance. Match it before wrappers such as an
		// assignment recovery error so that detail cannot leak through them.
		headline, hint, includeDetail = msgConnectorPeerTimeout, hintConnectorPeerTimeout, false
	case errors.Is(err, qurl.ErrRecoveryCredentialRejected):
		// The SDK error contains its internal recovery phase and numeric wire
		// code. Neither helps a customer fix the rejected account credential.
		headline, hint, includeDetail = msgConnectorRecoveryCredentialRejected, hintConnectorRecoveryCredentialRejected, false
	case errors.Is(err, qurl.ErrCredentialRecoveryIdentityRejected):
		headline, hint, includeDetail = msgConnectorRecoveryIdentityRejected, hintConnectorRecoveryIdentityRejected, false
	case errors.Is(err, qurl.ErrCredentialRecoveryRevokeRequired):
		headline, hint, includeDetail = msgConnectorRecoveryRevokeRequired, hintConnectorRecoveryRevokeRequired, false
	case errors.Is(err, qurl.ErrCredentialRecoveryExpired):
		headline, hint, includeDetail = msgConnectorRecoveryExpired, hintConnectorRecoveryExpired, false
	case errors.Is(err, qurl.ErrCredentialRecoveryCandidateConflict):
		headline, hint, includeDetail = msgConnectorRecoveryConflict, hintConnectorRecoveryConflict, false
	case errors.Is(err, qurl.ErrCredentialRecoveryCandidatePersistence):
		headline, hint, includeDetail = msgConnectorRecoveryPersistence, hintConnectorRecoveryPersistence, false
	case errors.Is(err, qurl.ErrCredentialRecoveryRequestRejected),
		errors.Is(err, qurl.ErrCredentialRecoveryInvalidResponse):
		headline, hint, includeDetail = msgConnectorRecoveryInvalid, hintConnectorRecoveryInvalid, false
	case errors.Is(err, qurl.ErrCredentialRecoveryUnavailable),
		errors.Is(err, qurl.ErrCredentialRecoveryRateLimited),
		errors.Is(err, qurl.ErrCredentialReplacementUnavailable),
		errors.Is(err, qurl.ErrCredentialRecoveryAssignmentRequired),
		errors.Is(err, qurl.ErrCredentialRecoveryGrantRejected),
		errors.Is(err, qurl.ErrCredentialRecoveryRetryRequired),
		errors.Is(err, qurl.ErrCredentialRecoveredAssignmentRefreshRequired):
		headline, hint, includeDetail = msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable, false
	case errors.Is(err, qurl.ErrInvalidRegisterConfig):
		headline, hint, includeDetail = msgConnectorEnrollmentConfig, hintConnectorEnrollmentConfig, false
	case errors.Is(err, qurl.ErrAgentBindingPersistence),
		errors.Is(err, qurl.ErrAgentCompletionCandidatePersistence),
		errors.Is(err, qurl.ErrAgentSetupLock):
		headline, hint, includeDetail = msgConnectorEnrollmentPersistence, hintConnectorEnrollmentPersistence, false
	case errors.Is(err, qurl.ErrRegistrationRecoveryRequired),
		errors.Is(err, qurl.ErrRegistrationRateLimited),
		errors.Is(err, qurl.ErrAssignmentTicketExpired),
		errors.Is(err, qurl.ErrCompletionUnavailable),
		errors.Is(err, qurl.ErrCompletionRecoveryRequired):
		headline, hint, includeDetail = msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable, false
	case errors.Is(err, qurl.ErrKeyRejected):
		headline, hint, includeDetail = msgConnectorTokenRejected, hintConnectorTokenRejected, false
	case errors.Is(err, qurl.ErrBootstrapSetupKeyConsumed):
		headline, hint, includeDetail = msgConnectorTokenConsumed, hintConnectorTokenConsumed, false
	case errors.Is(err, qurl.ErrCompletionIdentityRejected):
		headline, hint, includeDetail = msgConnectorEnrollmentIdentity, hintConnectorEnrollmentIdentity, false
	case errors.Is(err, qurl.ErrAgentIdentityConflict),
		errors.Is(err, qurl.ErrCompletionCredentialConflict):
		headline, hint, includeDetail = msgConnectorEnrollmentConflict, hintConnectorEnrollmentConflict, false
	case errors.Is(err, qurl.ErrRegistrationInvalidInput),
		errors.Is(err, qurl.ErrCompletionRequestRejected):
		headline, hint, includeDetail = msgConnectorEnrollmentInvalid, hintConnectorEnrollmentInvalid, false
	case errors.Is(err, qurl.ErrRegistrationDisabled):
		headline, hint, includeDetail = msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled, false
	case errors.Is(err, qurl.ErrDeviceKeyQuotaExceeded):
		headline, hint, includeDetail = msgConnectorDeviceQuota, hintConnectorDeviceQuota, false
	case errors.Is(err, qurl.ErrAssignmentTicketInvalid),
		errors.Is(err, qurl.ErrRegisterReplyMalformed),
		errors.Is(err, qurl.ErrRegistrationKeyKindDisallowed):
		headline, hint, includeDetail = msgConnectorEnrollmentMismatch, hintConnectorEnrollmentMismatch, false
	// qurl-go's assignment taxonomy follows the local configuration posture.
	case errors.Is(err, qurl.ErrAssignmentBootstrapConsumed):
		headline, hint = msgConnectorTokenConsumed, hintConnectorTokenConsumed
	case errors.Is(err, qurl.ErrAssignmentKeyRejected):
		headline, hint = msgConnectorTokenRejected, hintConnectorTokenRejected
	case errors.Is(err, qurl.ErrAssignmentRequestRejected):
		// The one case that drops the detail block. For 52109 that block is
		// the SDK's own sentence telling the reader to "correct
		// WithAgentRuntimeIdentity" — a Go option name, and the wrong remedy
		// for the enrollment-token mismatch that actually produces this. A
		// detail that misdirects is worse than no detail; the stable reason
		// (assignment_request_rejected) is still on the structured log line
		// that agent.logRegistrationFailure emits, so support keeps it.
		headline, hint, includeDetail = msgConnectorEnrollmentRejected, hintConnectorEnrollmentRejected, false
	case errors.Is(err, qurl.ErrAssignmentRegistrationDisabled):
		headline, hint = msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled
	case errors.Is(err, qurl.ErrAssignmentIdentityRejected):
		headline, hint = msgConnectorIdentityRejected, hintConnectorIdentityRejected
	case errors.Is(err, qurl.ErrAssignmentQuotaExceeded):
		headline, hint = msgConnectorQuotaExceeded, hintConnectorQuotaExceeded
	case errors.Is(err, qurl.ErrAssignmentRateLimited),
		errors.Is(err, qurl.ErrAssignmentUnavailable),
		errors.Is(err, qurl.ErrAssignmentReassignmentRequired),
		errors.Is(err, qurl.ErrAssignmentRecoveryRequired):
		// One story, four sentinels: the platform could not place this
		// Connector and the next step is the same for all of them. The
		// rate-limited member carries a RetryAfter, which is deliberately not
		// printed: the SDK already honors it inside the bounded operation, so
		// by the time this renders the wait has been spent and quoting it
		// would ask the customer to wait it a second time.
		headline, hint = msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable
	case errors.Is(err, qurl.ErrAssignmentLeaseExpired):
		// Ordering trap: AgentAssignment.Validate wraps an expired lease with
		// BOTH ErrAssignmentInvalidResponse and ErrAssignmentLeaseExpired, so
		// the narrower lease case must be tested first or every expiry would
		// render as a platform-contract violation.
		headline, hint = msgConnectorAssignmentExpired, hintConnectorAssignmentExpired
	case errors.Is(err, qurl.ErrAssignmentInvalidResponse):
		headline, hint = msgConnectorAssignmentInvalid, hintConnectorAssignmentInvalid
	default:
		return nil, false
	}
	return renderConnectorPosture(p, head, err, headline, hint, includeDetail), true
}

func renderConnectorPosture(p *Printer, head string, err error, headline, hint string, includeDetail bool) []string {
	lines := []string{head + " " + headline}
	if includeDetail {
		lines = append(lines, "")
		// errors.Join renders multi-line; keep every line inside the indented
		// detail block.
		for _, detail := range strings.Split(err.Error(), "\n") {
			lines = append(lines, "  "+detail)
		}
	}
	lines = append(lines, "", "  "+p.dim(hint))
	return lines
}

// renderConnectorResourcePosture keeps the SDK and local continuity detail
// behind the customer boundary. The native result code is the only dynamic
// value admitted here: qurl-go defines it as a non-secret five-digit support
// code and does not attach a peer, request nonce, credential, or endpoint.
func renderConnectorResourcePosture(p *Printer, head string, err error, headline, hint string) []string {
	lines := []string{head + " " + headline}
	if code := connectorResourceCode(err); code != "" {
		lines = append(lines, "", "  "+p.dim(labelConnectorErrorCode+" "+code))
	}
	return append(lines, "", "  "+p.dim(hint))
}

func connectorResourceCode(err error) string {
	var discovery *qurl.ConnectorResourceDiscoveryError
	if !errors.As(err, &discovery) || discovery == nil || len(discovery.Code) != 5 {
		return ""
	}
	for _, character := range discovery.Code {
		if character < '0' || character > '9' {
			return ""
		}
	}
	return discovery.Code
}

func connectorResourceErrorPosture(err error) (headline, hint string, ok bool) {
	switch {
	case errors.Is(err, state.ErrConnectorResourceVerification):
		return msgConnectorResourceLocalVerification, hintConnectorResourceLocalVerification, true
	case errors.Is(err, state.ErrConnectorResourceStateConflict):
		return msgConnectorResourceLocalConflict, hintConnectorResourceLocalConflict, true
	case errors.Is(err, qurl.ErrInvalidNativeConnectorResourceRequest),
		errors.Is(err, qurl.ErrConnectorResourceRequestRejected):
		return msgConnectorResourceInvalidRequest, hintConnectorResourceInvalidRequest, true
	case errors.Is(err, qurl.ErrConnectorResourceIdentityRejected):
		return msgConnectorIdentityRejected, hintConnectorIdentityRejected, true
	case errors.Is(err, qurl.ErrConnectorResourceEntitlementDenied):
		return msgConnectorResourceEntitlement, hintConnectorResourceEntitlement, true
	case errors.Is(err, qurl.ErrConnectorResourceIdentityConflict):
		return msgConnectorResourceConflict, hintConnectorResourceConflict, true
	case errors.Is(err, qurl.ErrConnectorResourceQuotaExceeded):
		return msgConnectorResourceQuota, hintConnectorResourceQuota, true
	case errors.Is(err, qurl.ErrConnectorResourceRateLimited),
		errors.Is(err, qurl.ErrConnectorResourceUnavailable):
		return msgConnectorResourceUnavailable, hintConnectorResourceUnavailable, true
	case errors.Is(err, qurl.ErrInvalidNativeConnectorResourceResponse):
		return msgConnectorResourceInvalidResponse, hintConnectorResourceInvalidResponse, true
	default:
		return "", "", false
	}
}

// apiErrorLines is the RFC 7807 anatomy: headline with status, detail
// paragraph, sorted invalid fields, one hint, request id. Title, Detail, and
// InvalidFields are the qURL service's public error contract and are rendered
// as supplied. Credential redaction still applies at RenderError; do not add a
// heuristic topology sanitizer here because it would silently rewrite that
// public contract.
func apiErrorLines(p *Printer, head string, apiErr *qurlapi.Error) []string {
	headline := apiErr.Title
	if headline == "" {
		headline = "the qURL service reported a problem"
	}
	lines := []string{fmt.Sprintf("%s %s (HTTP %d)", head, headline, apiErr.StatusCode)}

	if apiErr.Detail != "" && apiErr.Detail != headline {
		lines = append(lines, "", "  "+apiErr.Detail)
	}

	if len(apiErr.InvalidFields) > 0 {
		fields := make([]string, 0, len(apiErr.InvalidFields))
		for name := range apiErr.InvalidFields {
			fields = append(fields, name)
		}
		sort.Strings(fields)
		lines = append(lines, "", "  Invalid fields:")
		for _, name := range fields {
			lines = append(lines, fmt.Sprintf("    %s: %s", name, apiErr.InvalidFields[name]))
		}
	}

	if hint := errorHint(apiErr); hint != "" {
		lines = append(lines, "", "  "+p.dim(hint))
	}
	if apiErr.RequestID != "" {
		lines = append(lines, "  "+p.dim("Request ID: "+apiErr.RequestID))
	}
	return lines
}

// errorHint picks the one §17.1 hint. Programmatic matching is on the
// problem code (the platform's stable contract; titles and details are
// prose), with status classes as the fallback. The "gone" family shares
// exit code 5 but deliberately differs here: 404 stays ambiguous, while
// the owner-visible revoked and retired states are told the truth. The 403
// family splits the same way: account_frozen is an account-standing
// condition, not a permissions problem, and says so.
func errorHint(apiErr *qurlapi.Error) string {
	switch {
	case apiErr.AgentEnrollmentScopeRequired():
		return hintEnrollmentScope
	case apiErr.ConnectorEnrollmentScopeRequired():
		return hintConnectorEnrollmentScope
	case strings.EqualFold(apiErr.Code, "revoked"):
		return hintRevoked
	case strings.EqualFold(apiErr.Code, "resource_tombstoned"):
		return hintRetired
	case strings.EqualFold(apiErr.Code, "insufficient_scope"):
		return hintScope
	case strings.EqualFold(apiErr.Code, "account_frozen"):
		return hintFrozen
	case strings.EqualFold(apiErr.Code, "api_key_expired"):
		return hintExpired
	case strings.EqualFold(apiErr.Code, "api_key_invalid"):
		return hintKeyInvalid
	case strings.EqualFold(apiErr.Code, "quota_exceeded"):
		return hintQuotaExceeded
	case apiErr.StatusCode == http.StatusUnauthorized:
		return hintUnauthorized
	case apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusGone:
		return hintNotFound
	case apiErr.StatusCode == http.StatusTooManyRequests && apiErr.RetryAfter > 0:
		return fmt.Sprintf(hintRetryAfter, apiErr.RetryAfter)
	default:
		return ""
	}
}
