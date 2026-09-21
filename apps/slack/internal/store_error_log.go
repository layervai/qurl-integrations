package internal

import (
	"errors"
	"log/slog"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// TODO(upstream-contract): infra#964 matches ERROR and [ddb_error]. Preserve
// expected conditional, quota, and not-found levels; only store outages page.
func storeErrorLogLevel(err error, fallback slog.Level) slog.Level {
	var storeErr *slackdata.Error
	if errors.As(err, &storeErr) && storeErr.Code == "ddb_error" {
		return slog.LevelError
	}
	return fallback
}
