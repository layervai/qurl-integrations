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
	if log == nil {
		log = slog.Default()
	}

	auditAttrs := make([]slog.Attr, 0, len(attrs)+3)
	auditAttrs = append(auditAttrs,
		slog.String("event", DependencyAuthFailure),
		slog.String("agent", AgentSlack),
		slog.String("dependency", DependencyQURLService),
	)
	auditAttrs = append(auditAttrs, attrs...)

	log.LogAttrs(context.Background(), slog.LevelWarn, "dependency auth failure",
		slog.Attr{Key: "audit", Value: slog.GroupValue(auditAttrs...)})
}
