package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

var errOptionalProbeDeadline = errors.New("optional list probe deadline")

var errCredentialLookup = errors.New("workspace credential lookup failed")

func callerStopped(ctx context.Context, err error) bool {
	return ctx.Err() == context.Canceled && errors.Is(err, context.Canceled)
}

// TODO(upstream-contract): infra#964 matches ERROR and [ddb_error]. Preserve
// expected conditional, quota, and not-found levels; only store outages page.
func storeErrorLogLevel(ctx context.Context, err error, fallback slog.Level) slog.Level {
	if policyMutationReply(err) != "" {
		return slog.LevelWarn
	}
	if callerStopped(ctx, err) || (errors.Is(context.Cause(ctx), errOptionalProbeDeadline) && errors.Is(err, context.DeadlineExceeded)) {
		return fallback
	}
	if errors.Is(err, errCredentialLookup) {
		return slog.LevelError
	}
	var storeErr *slackdata.Error
	if errors.As(err, &storeErr) && storeErr.Code == "ddb_error" {
		return slog.LevelError
	}
	return fallback
}

func policyMutationReply(err error) string {
	var se *slackdata.Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		return ""
	}
	switch se.Code {
	case slackdata.ErrCodePolicyWorkspaceChanged:
		return "Workspace permissions changed while this was running. Run the command again."
	case slackdata.ErrCodePolicyReconciliationRequired:
		return "This channel's settings need operator reconciliation. Contact your qURL operator."
	default:
		return ""
	}
}
