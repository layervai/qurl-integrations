package internal

import (
	"context"
	"errors"
	"log/slog"

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
