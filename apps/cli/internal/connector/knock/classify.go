package knock

import (
	"errors"

	qurl "github.com/layervai/qurl-go/qurl"
)

// Log event names for the knock decision stream. The names and the reason
// vocabulary below are shared with the standalone qURL Connector's audit
// taxonomy so dashboards can pivot across both without a translation table.
const (
	eventKnockSuccess = "knock_success"
	eventKnockDeny    = "knock_deny"
	eventKnockError   = "knock_error"
)

// knockFailure classifies one failed native knock for the log stream.
type knockFailure struct {
	event  string
	reason string
}

// classifyKnockFailure buckets a native knock error into the stable
// event/reason vocabulary. Authenticated policy decisions are denials;
// transport, malformed-wire, and overload failures remain diagnostic errors.
// The classification is intentionally coarse — the error carries the original
// message verbatim for forensic detail; the reason exists for dashboard
// grouping.
func classifyKnockFailure(err error) knockFailure {
	var deny *qurl.ServerDenyError
	if errors.As(err, &deny) && deny != nil {
		reason := "knock_denied"
		if deny.ErrCode == nativeKnockResourceNotFoundCode {
			reason = "resource_not_found"
		}
		return knockFailure{event: eventKnockDeny, reason: reason}
	}

	reason := "knock_transport_error"
	switch {
	case errors.Is(err, qurl.ErrInvalidNativeKnockInput):
		reason = "knock_invalid_input"
	case errors.Is(err, qurl.ErrMalformedReply):
		reason = "knock_invalid_response"
	case errors.Is(err, qurl.ErrServerOverloaded):
		reason = "knock_server_overloaded"
	}
	return knockFailure{event: eventKnockError, reason: reason}
}
