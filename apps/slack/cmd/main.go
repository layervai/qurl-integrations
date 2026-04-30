// Package main is the HTTP entrypoint for the Slack integration.
//
// Runs as a long-lived process behind an ALB on Fargate. Listens on
// :8080, terminates gracefully on SIGTERM (Fargate's task-stop signal,
// sent 30s before SIGKILL).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	listenAddr = ":8080"
	// shutdownTimeout sits inside Fargate's 30s SIGTERM→SIGKILL window with
	// 5s of headroom for the container runtime to actually deliver SIGKILL
	// and reap the process.
	shutdownTimeout = 25 * time.Second
)

// version is set at build time via `-ldflags "-X main.version=<sha>"`.
// Used in the qURL client User-Agent so server-side traces can pin a
// failure to a specific bot release.
var version = "dev"

func main() {
	// JSON handler is load-bearing for log-injection safety: the G706
	// gosec suppressions in apps/slack/internal/handler.go assume slog's
	// JSON output escapes control characters in tainted attribute
	// values. Don't swap to TextHandler without revisiting those sites.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run holds the full server lifecycle so `defer stop()` releases the
// signal handler before main reaches os.Exit on the error path.
func run() error {
	// Required env vars are explicit by design: a missing QURL_ENDPOINT
	// previously fell back to the sandbox URL, which is the kind of silent
	// misconfiguration that ships a prod deploy at sandbox.
	qurlEndpoint := os.Getenv("QURL_ENDPOINT")
	if qurlEndpoint == "" {
		return errors.New("QURL_ENDPOINT is required")
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		return errors.New("SLACK_SIGNING_SECRET is required")
	}

	authProvider := auth.EnvProvider{EnvVar: "QURL_API_KEY"}
	userAgent := "qurl-slack/" + version

	handler := internal.NewHandler(internal.Config{
		AuthProvider:       &authProvider,
		SlackSigningSecret: slackSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlEndpoint, apiKey,
				client.WithUserAgent(userAgent),
				// Synchronous ack path — Slack's 3s budget rules out retries here.
				client.WithRetry(0),
			)
		},
	})

	srv := &http.Server{
		// Addr intentionally omitted: srv.Serve(ln) ignores it, and we
		// bind via net.ListenConfig below. Setting it would mislead a
		// future reader into thinking it controls the bind.
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Bind first so a port-already-in-use failure returns before the
	// drain goroutine spawns — keeps the "received shutdown signal"
	// log line off the bind-failure path. Use a fresh background ctx
	// for the bind so a SIGTERM arriving in the gap between
	// signal.NotifyContext and Listen doesn't surface as
	// "listen: context canceled".
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		slog.Info("received shutdown signal — draining HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
		close(shutdownDone)
	}()

	slog.Info("starting Slack bot HTTP server", "addr", listenAddr)
	serveErr := srv.Serve(ln)

	// Always release the signal handler and wait for the drain goroutine
	// regardless of how Serve returned — keeps the cleanup deterministic
	// even if Serve fails with a non-ErrServerClosed error.
	stop()
	<-shutdownDone

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	slog.Info("server stopped cleanly")
	return nil
}
