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
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/supervisor"
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
	if errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		return []string{head + " " + msgLinksUnavailable}
	}
	if errors.Is(err, auth.ErrNoCredential) {
		return []string{head + " " + msgNoCredential, "", "  " + p.dim(hintNoCredential)}
	}
	if lines, ok := connectorErrorLines(p, head, err); ok {
		return lines
	}

	var apiErr *qurlapi.Error
	if errors.As(err, &apiErr) {
		return apiErrorLines(p, head, apiErr)
	}
	return []string{head + " " + err.Error()}
}

// connectorErrorLines is the customer-language translation of the Connector
// lifecycle sentinels and of qurl-go's enrollment/assignment taxonomy: a
// plain-language headline, the operator-facing detail (the wrapped error text,
// which stays technical and is where env names and reasons live), and the one
// §17.1 hint. Two postures omit the detail block — token-required, whose
// headline and hint already carry everything the wrapped message says, and
// request-rejected, whose SDK text prescribes the wrong remedy in SDK
// vocabulary (see its case).
//
// Every case matches with errors.Is, so a sentinel keeps rendering after the
// enroll/refresh path wraps it (`refresh native assignment binding: %w`) or
// after the SDK returns it inside an *AssignmentError. A mapping that only
// fired on a bare sentinel would be dead code on the real path.
func connectorErrorLines(p *Printer, head string, err error) ([]string, bool) {
	headline, hint, includeDetail := "", "", true
	switch {
	case errors.Is(err, agent.ErrEnrollmentTokenRequired):
		headline, hint, includeDetail = msgConnectorTokenRequired, hintConnectorTokenRequired, false
	case errors.Is(err, agent.ErrIdentityConflict):
		headline, hint = msgConnectorIdentityConflict, hintConnectorIdentityConflict
	case errors.Is(err, agent.ErrRefreshApprovalRequired):
		headline, hint = msgConnectorRefreshApproval, hintConnectorRefreshApproval
	case errors.Is(err, agent.ErrRefreshDisabled):
		headline, hint = msgConnectorRefreshDisabled, hintConnectorRefreshDisabled
	case errors.Is(err, agent.ErrRefreshModeInvalid):
		headline, hint = msgConnectorRefreshModeInvalid, hintConnectorRefreshModeInvalid
	case errors.Is(err, agent.ErrRefreshAlreadyAttempted):
		headline, hint = msgConnectorRefreshExhausted, hintConnectorRefreshExhausted
	case errors.Is(err, hub.ErrConfig):
		headline, hint = msgConnectorHubConfig, hintConnectorHubConfig
	case supervisor.IsTooManyKnockFailures(err):
		headline, hint = msgConnectorRetryBudget, hintConnectorRetryBudget

	// qurl-go's assignment taxonomy comes last so the CLI's own lifecycle
	// reading always wins: agent.ErrRefreshAlreadyAttempted, for one, is
	// joined with the warm-open cause and therefore also matches
	// ErrAssignmentLeaseExpired below.
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
	return lines, true
}

// apiErrorLines is the RFC 7807 anatomy: headline with status, detail

// paragraph, sorted invalid fields, one hint, request id.
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
