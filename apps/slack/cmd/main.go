// Package main is the long-running HTTP entrypoint for the Slack
// integration. It runs as a single process on ECS Fargate behind an ALB.
//
// Why ECS instead of Lambda: a long-running process unblocks the
// `response_url` async-defer pattern Slack publishes for slash commands
// (ack within 3s, complete the work in a goroutine, post the final result
// back to the response_url within 30 minutes). Cold-start variance and
// the 3s ack budget were the binding constraints under the previous
// API-Gateway-fronted Lambda.
package main

import (
	"context"
	"errors"
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
	// listenAddr is fixed at :8080 so the Dockerfile's EXPOSE, the ECS
	// task's container port, and the ALB target group all line up.
	// Changing it requires updating all four together.
	listenAddr = ":8080"

	// readHeaderTimeout caps how long we'll wait for a client to finish
	// sending request headers — the standard mitigation for Slowloris-style
	// attacks against a long-running process. Slack's edge sends headers
	// within milliseconds, so 5s is generous for the legitimate path.
	readHeaderTimeout = 5 * time.Second

	// readTimeout bounds the full request read. Slack payloads are tiny;
	// 15s is well above any realistic ALB-to-task hop.
	readTimeout = 15 * time.Second

	// writeTimeout bounds response writing. The handler synchronously
	// produces its response (the response_url goroutine pattern lives
	// outside the request lifecycle), so this only needs to cover network
	// flush time.
	writeTimeout = 15 * time.Second

	// idleTimeout reaps idle keep-alive connections. ALBs reuse
	// connections aggressively; the AWS default idle timeout is 60s, so
	// 65s on the task side avoids the race where the ALB sends on a
	// connection the task just closed.
	idleTimeout = 65 * time.Second

	// shutdownTimeout caps graceful drain on SIGTERM. ECS sends SIGTERM,
	// then SIGKILL after the task's stopTimeout (default 30s). Keeping
	// drain shorter than that leaves margin for the runtime to exit
	// cleanly before the kill.
	shutdownTimeout = 25 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	qurlEndpoint := os.Getenv("QURL_ENDPOINT")
	if qurlEndpoint == "" {
		qurlEndpoint = "https://api.layerv.xyz"
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		slog.Error("SLACK_SIGNING_SECRET is required")
		os.Exit(1)
	}

	authProvider := auth.EnvProvider{EnvVar: "QURL_API_KEY"}

	handler := internal.NewHandler(internal.Config{
		QURLEndpoint:       qurlEndpoint,
		AuthProvider:       &authProvider,
		SlackSigningSecret: slackSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			// Retry is enabled now that we're long-running — the 3s
			// ack budget only governs the synchronous response, not the
			// async response_url goroutine where retried calls land.
			return client.New(qurlEndpoint, apiKey,
				client.WithUserAgent("qurl-slack/dev"),
			)
		},
	})

	mux := http.NewServeMux()
	// /health is the ALB and ECS health probe target. It must NOT go
	// through Slack signature verification — the probe never carries
	// a Slack signature header.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			slog.Warn("health write failed", "error", err)
		}
	})
	// All Slack-facing routes share the same adapter: ServeHTTP buffers
	// the body, copies headers into the API-Gateway-shaped value the
	// existing dispatch logic understands, and writes the response
	// envelope back. Path-based routing happens inside Handle so we keep
	// one source of truth across both runtime shapes.
	mux.Handle("/slack/commands", handler)
	mux.Handle("/slack/events", handler)
	mux.Handle("/slack/interactions", handler)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	// Run the server in a goroutine so the main goroutine can wait on
	// signals. ListenAndServe always returns a non-nil error; we
	// distinguish ErrServerClosed (graceful shutdown) from anything else.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("slack server listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// SIGTERM is what ECS sends on task stop; SIGINT is for local Ctrl-C.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())
	case err := <-serverErr:
		// ListenAndServe returned a real error before any shutdown signal.
		// Exit non-zero so ECS replaces the task instead of letting it
		// linger in a half-broken state.
		if err != nil {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := gracefulShutdown(srv); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}

// gracefulShutdown owns the shutdown context lifetime so the deferred
// cancel actually runs even on the failure path. Inlining
// `defer cancel()` next to an `os.Exit(1)` would skip the cancel —
// gocritic flags this as `exitAfterDefer`.
func gracefulShutdown(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}
