// Package slackaudit emits machine-filterable audit records for Slack runtime
// dependencies.
package slackaudit

import (
	"context"
	"log/slog"
)

const (
	// DependencyAuthFailure is the CloudWatch metric-filtered event emitted when
	// qurl-service rejects a Slack dependency request with an unexpected auth-
	// class status.
	DependencyAuthFailure = "dependency_auth_failure"

	// QURLMintReason is the CloudWatch metric-filtered event emitted when an
	// operator mints a qURL with an explicit reason
	// (`/qurl get <$id|$alias> reason:"…"`).
	//
	// This event IS the audit log that flag's help text promises. The reason
	// used to ride in the mint request body instead, where it was never
	// recorded: `reason` has never been a property of CreateQurlRequest or
	// CreateQurlForResourceRequest, so qurl-service dropped it on arrival.
	// qurl-service#1402 turns that silent drop into a 400, so the record has
	// to live on this side — and now actually exists.
	QURLMintReason = "qurl_mint_reason"

	// AgentSlack is the audit.agent value for Slack-originated dependency events.
	AgentSlack = "slack"
	// DependencyQURLService is the audit.dependency value for qurl-service calls.
	DependencyQURLService = "qurl_service"
)

// DependencyAuthFailureAttrs returns the fixed per-event field set for
// dependency auth failures. Route is a caller-owned origin label for humans,
// not a closed enum; CloudWatch metric filters should key on event, agent, and
// dependency instead.
func DependencyAuthFailureAttrs(route, method, path string, status int, code, requestID string) []slog.Attr {
	return []slog.Attr{
		slog.String("route", route),
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("request_id", requestID),
	}
}

// LogDependencyAuthFailure emits Slack's CloudWatch-filtered audit shape:
// {"audit":{"event":"dependency_auth_failure","agent":"slack",...}}.
func LogDependencyAuthFailure(log *slog.Logger, attrs ...slog.Attr) {
	logAudit(log, slog.LevelWarn, "dependency auth failure", DependencyAuthFailure,
		append([]slog.Attr{slog.String("dependency", DependencyQURLService)}, attrs...)...)
}

// QURLMintReasonAttrs returns the fixed per-event field set for a reasoned
// mint. The ids identify WHICH mint the reason belongs to, so an auditor can
// join this record to the mint itself.
//
// reason is operator prose — the parser accepts any non-empty value for
// `reason:` — so callers MUST bound it before passing it here (the
// `/qurl get` path truncates to getReasonAuditMaxRunes). It is emitted as a
// discrete slog attribute rather than interpolated into the message, so the
// handler quotes/escapes it and a reason carrying newlines cannot forge a
// surrounding log record.
func QURLMintReasonAttrs(teamID, channelID, userID, resourceID, reason string) []slog.Attr {
	return []slog.Attr{
		slog.String("team_id", teamID),
		slog.String("channel_id", channelID),
		slog.String("user_id", userID),
		slog.String("resource_id", resourceID),
		slog.String("reason", reason),
	}
}

// LogQURLMintReason emits Slack's CloudWatch-filtered audit shape:
// {"audit":{"event":"qurl_mint_reason","agent":"slack",...}}.
//
// Info, not Warn: a reasoned mint is a normal recorded action, not a fault.
func LogQURLMintReason(log *slog.Logger, attrs ...slog.Attr) {
	logAudit(log, slog.LevelInfo, "qurl mint reason", QURLMintReason, attrs...)
}

// logAudit is the shared emitter: every record is one "audit" group opening
// with event + agent, so CloudWatch metric filters key on a stable prefix
// regardless of which event follows.
func logAudit(log *slog.Logger, level slog.Level, msg, event string, attrs ...slog.Attr) {
	if log == nil {
		log = slog.Default()
	}

	auditAttrs := make([]slog.Attr, 0, len(attrs)+2)
	auditAttrs = append(auditAttrs,
		slog.String("event", event),
		slog.String("agent", AgentSlack),
	)
	auditAttrs = append(auditAttrs, attrs...)

	log.LogAttrs(context.Background(), level, msg,
		slog.Attr{Key: "audit", Value: slog.GroupValue(auditAttrs...)})
}
