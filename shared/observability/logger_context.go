package observability

import (
	"context"
	"log/slog"
)

type loggerContextKey struct{}

// WithLogger carries a logger through delivery seams. Callers supply a redacting handler.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

// Logger returns the request logger, or the process logger outside a request.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := LoggerFromContext(ctx); ok {
		return logger
	}
	return slog.Default()
}

// LoggerFromContext reports whether a non-nil logger was supplied explicitly.
func LoggerFromContext(ctx context.Context) (*slog.Logger, bool) {
	logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger)
	return logger, ok && logger != nil
}
