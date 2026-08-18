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

	var apiErr *qurlapi.Error
	if errors.As(err, &apiErr) {
		return apiErrorLines(p, head, apiErr)
	}
	return []string{head + " " + err.Error()}
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
