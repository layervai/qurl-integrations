package observability

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRequestLoggerPreservesContextAndFallback(t *testing.T) {
	if Logger(context.Background()) != slog.Default() {
		t.Fatal("missing request logger must use the process logger")
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil)).With("event_id", "event-test")
	ctx := WithLogger(parent, logger)
	if Logger(ctx) != logger {
		t.Fatal("request logger was lost")
	}
	want, _ := parent.Deadline()
	got, ok := ctx.Deadline()
	if !ok || got != want {
		t.Fatal("logger changed the delivery deadline")
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("logger lost cancellation")
	}
	if Logger(WithLogger(context.Background(), nil)) != slog.Default() {
		t.Fatal("nil request logger must fall back")
	}
}

func TestLoggerFromContextReportsPresenceInsteadOfLoggerIdentity(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), WithLogger(context.Background(), nil)} {
		if _, ok := LoggerFromContext(ctx); ok {
			t.Fatal("missing logger reported present")
		}
	}
	expected := slog.Default()
	logger, ok := LoggerFromContext(WithLogger(context.Background(), expected))
	if !ok || logger != expected {
		t.Fatal("explicit process logger lost its context presence")
	}
}
