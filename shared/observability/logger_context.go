package observability

import (
	"context"
	"log/slog"
)

type loggerContextKey struct{}

// WithLogger carries the existing redacting request logger through delivery seams.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

// Logger returns the request logger, or the process logger outside a request.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
