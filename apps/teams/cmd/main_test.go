package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testConnectorImageRepo = "ghcr.io/layervai/qurl-connector"

func TestReadTunnelImageConfig(t *testing.T) {
	cases := []struct {
		name        string
		image       string
		fallback    string
		wantImage   string
		wantErrText string
	}{
		{
			name:      "explicit pinned tag accepted",
			image:     testConnectorImageRepo + ":v1.2.3",
			wantImage: testConnectorImageRepo + ":v1.2.3",
		},
		{
			name:        "latest tag rejected",
			image:       testConnectorImageRepo + ":latest",
			wantErrText: connectorImageErrFloating,
		},
		{
			name:        "latest digest rejected",
			image:       testConnectorImageRepo + ":latest@sha256:" + strings.Repeat("a", 64),
			wantErrText: connectorImageErrLatestDigest,
		},
		{
			name:        "empty image rejected without fallback opt in",
			wantErrText: envQURLConnectorImage + " is required",
		},
		{
			name:      "empty image allowed with sandbox fallback",
			fallback:  connectorImageFallbackSandbox,
			wantImage: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envQURLConnectorImage, tc.image)
			t.Setenv(envQURLConnectorImageFallback, tc.fallback)

			got, err := readTunnelImageConfig()

			if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("readTunnelImageConfig() err = %v, want substring %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("readTunnelImageConfig() err = %v", err)
			}
			if got != tc.wantImage {
				t.Fatalf("readTunnelImageConfig() = %q, want %q", got, tc.wantImage)
			}
		})
	}
}

func TestRunValidatesTunnelImageBeforeInfraSetup(t *testing.T) {
	t.Setenv(envQURLEndpoint, "https://api.qurl.invalid")
	t.Setenv(envTeamsAppID, "teams-app-id")
	t.Setenv(envTeamsAppPassword, "teams-app-password")
	t.Setenv(envQURLConnectorImage, "")
	t.Setenv(envQURLConnectorImageFallback, "")

	err := run()

	if err == nil || !strings.Contains(err.Error(), envQURLConnectorImage+" is required") {
		t.Fatalf("run() err = %v, want %s fail-closed error before infra/env setup", err, envQURLConnectorImage)
	}
}

func TestShutdownBudgetsLeaveLameduckDrainHeadroom(t *testing.T) {
	if lameduckDuration <= 0 {
		t.Fatalf("lameduckDuration = %s, want positive ALB drain head start", lameduckDuration)
	}
	if lameduckDuration >= shutdownTimeout {
		t.Fatalf("lameduckDuration = %s must be less than shutdownTimeout = %s", lameduckDuration, shutdownTimeout)
	}
	if got := shutdownTimeout - lameduckDuration; got < 10*time.Second {
		t.Fatalf("shutdown drain headroom = %s, want at least 10s after lameduck", got)
	}
}

func TestHealthHandlerReturns503WhenUnhealthy(t *testing.T) {
	health := newHealthHandler()

	rec := httptest.NewRecorder()
	health.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy status = %d, want %d", rec.Code, http.StatusOK)
	}

	health.SetHealthy(false)
	rec = httptest.NewRecorder()
	health.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestLameduckForSignal(t *testing.T) {
	if got := lameduckForSignal(syscall.SIGTERM); got != lameduckDuration {
		t.Fatalf("SIGTERM lameduck = %s, want %s", got, lameduckDuration)
	}
	if got := lameduckForSignal(syscall.SIGINT); got != 0 {
		t.Fatalf("SIGINT lameduck = %s, want immediate drain", got)
	}
}

func TestShutdownSignalSourceFirstSignalWinsAndCancelsContext(t *testing.T) {
	input := make(chan os.Signal, 2)
	stopCalls := 0
	source := newShutdownSignalSourceFromInput(input, func() {
		stopCalls++
	})
	defer source.stop()

	input <- syscall.SIGTERM

	select {
	case sig := <-source.first:
		if sig != syscall.SIGTERM {
			t.Fatalf("first signal = %v, want %v", sig, syscall.SIGTERM)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first signal")
	}

	select {
	case <-source.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context cancellation")
	}

	input <- syscall.SIGINT
	select {
	case sig := <-source.first:
		t.Fatalf("unexpected second signal delivered: %v", sig)
	default:
	}

	source.stop()
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestShutdownSequenceImmediateDrainSkipsLameduckHealth(t *testing.T) {
	srv := &recordingShutdownServer{}
	handler := &recordingShutdownHandler{}
	canceled := false
	sleepCalled := false

	runShutdownSequence(
		srv,
		handler,
		func() { canceled = true },
		0,
		time.Second,
		func(context.Context, time.Duration) bool {
			sleepCalled = true
			return true
		},
	)

	if sleepCalled {
		t.Fatal("immediate drain should not sleep")
	}
	if len(handler.healthyCalls) != 0 {
		t.Fatalf("SetHealthy calls = %v, want none for immediate drain", handler.healthyCalls)
	}
	if !canceled {
		t.Fatal("handler context was not canceled")
	}
	if !srv.shutdownCalled {
		t.Fatal("server Shutdown was not called")
	}
	if !handler.waitCalled {
		t.Fatal("handler WaitTimeout was not called")
	}
	if handler.waitBudget <= 0 {
		t.Fatalf("WaitTimeout budget = %s, want positive remaining budget", handler.waitBudget)
	}
}

func TestShutdownSequenceBudgetExhaustedDuringLameduckStillClosesServer(t *testing.T) {
	srv := &recordingShutdownServer{err: context.DeadlineExceeded}
	handler := &recordingShutdownHandler{}
	canceled := false

	runShutdownSequence(
		srv,
		handler,
		func() { canceled = true },
		time.Second,
		25*time.Millisecond,
		func(context.Context, time.Duration) bool {
			return false
		},
	)

	if len(handler.healthyCalls) != 1 || handler.healthyCalls[0] {
		t.Fatalf("SetHealthy calls = %v, want [false]", handler.healthyCalls)
	}
	if !canceled {
		t.Fatal("handler context was not canceled")
	}
	if !srv.shutdownCalled {
		t.Fatal("server Shutdown was not called")
	}
	if !handler.waitCalled {
		t.Fatal("handler WaitTimeout was not called")
	}
	if handler.waitBudget != 0 {
		t.Fatalf("WaitTimeout budget = %s, want 0 after exhausted lameduck", handler.waitBudget)
	}
}

type recordingShutdownServer struct {
	shutdownCalled bool
	err            error
}

func (s *recordingShutdownServer) Shutdown(context.Context) error {
	s.shutdownCalled = true
	return s.err
}

type recordingShutdownHandler struct {
	healthyCalls []bool
	waitCalled   bool
	waitBudget   time.Duration
}

func (h *recordingShutdownHandler) SetHealthy(healthy bool) {
	h.healthyCalls = append(h.healthyCalls, healthy)
}

func (h *recordingShutdownHandler) WaitTimeout(d time.Duration) bool {
	h.waitCalled = true
	h.waitBudget = d
	return true
}
