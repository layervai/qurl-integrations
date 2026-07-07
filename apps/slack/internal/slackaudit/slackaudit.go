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

	AgentSlack            = "slack"
	DependencyQURLService = "qurl_service"
)

// LogDependencyAuthFailure emits the same top-level audit shape Discord uses:
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

	log.LogAttrs(context.Background(), slog.LevelInfo, "dependency auth failure",
		slog.Attr{Key: "audit", Value: slog.GroupValue(auditAttrs...)})
}
