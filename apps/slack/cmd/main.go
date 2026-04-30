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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run holds the full server lifecycle so deferred cleanup (signal handler
// release, etc.) executes before main returns to os.Exit.
func run() error {
	qurlEndpoint := os.Getenv("QURL_ENDPOINT")
	if qurlEndpoint == "" {
		qurlEndpoint = "https://api.layerv.xyz"
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		return errors.New("SLACK_SIGNING_SECRET is required")
	}

	authProvider := auth.EnvProvider{EnvVar: "QURL_API_KEY"}

	handler := internal.NewHandler(internal.Config{
		AuthProvider:       &authProvider,
		SlackSigningSecret: slackSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlEndpoint, apiKey,
				client.WithUserAgent("qurl-slack/dev"),
				// Synchronous ack path — Slack's 3s budget rules out retries here.
				client.WithRetry(0),
			)
		},
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}

	<-shutdownDone
	slog.Info("server stopped cleanly")
	return nil
}
