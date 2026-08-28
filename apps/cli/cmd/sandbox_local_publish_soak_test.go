//go:build clisandbox && clisoak

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

const (
	localPublishSoakArming  = "QURL_CLI_SANDBOX_LOCAL_PUBLISH_SOAK"
	localPublishSoakLength  = "QURL_CLI_SANDBOX_SOAK_DURATION"
	defaultLocalPublishSoak = 80 * time.Minute
	minimumLocalPublishSoak = 75 * time.Minute
)

// TestSandboxLocalPublishSoak keeps the customer qURL path serving across
// multiple native authorization lifetimes, a credential-free warm daemon
// restart, and an explicit epoch restart. The scheduled private orchestrator
// runs the default 80-minute duration; a shorter run would not cross the
// one-hour enrollment/qURL lifetime boundary this validation is intended to
// catch.
func TestSandboxLocalPublishSoak(t *testing.T) {
	if os.Getenv(localPublishSoakArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: local-publish soak is disarmed — %s != enabled", localPublishSoakArming)
	}
	duration := sandboxSoakDuration(t)
	fixture := startSandboxLocalPublish(t, "soak")
	defer fixture.stopAndValidate(t)

	initialAgent := loadSandboxAgentState(t, fixture.stateDir)
	if initialAgent == nil {
		t.Fatal("soak enrollment produced no durable agent state")
	}
	waitSandboxSharingState(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, "on", "serving", 2*time.Minute)
	assertSandboxLocalRoute(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, fixture.marker, 2*time.Minute)

	startFDs, startRSS := sandboxProcessUsage(t)
	started := time.Now()
	warmRestartAt := started.Add(duration / 3)
	epochRestartAt := started.Add(2 * duration / 3)
	deadline := started.Add(duration)
	warmRestartDone := false
	epochRestartDone := false
	requestCount := 1

	var daemonCancel context.CancelFunc
	var daemonDone <-chan *runResult
	defer func() {
		if daemonCancel != nil {
			daemonCancel()
			select {
			case res := <-daemonDone:
				assertSandboxStreamsDoNotContainSecrets(t, res, fixture.key, fixture.cleanupJWT)
			case <-time.After(15 * time.Second):
				t.Error("warm daemon did not stop after soak cancellation")
			}
		}
	}()

	for time.Now().Before(deadline) {
		now := time.Now()
		if !warmRestartDone && !now.Before(warmRestartAt) {
			fixture.stopAndValidate(t)
			daemonCancel, daemonDone = startCredentialFreeSandboxDaemon(t, fixture)
			waitSandboxSharingState(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, "on", "serving", 2*time.Minute)
			resumed := loadSandboxAgentState(t, fixture.stateDir)
			if resumed == nil || resumed.AgentID != initialAgent.AgentID || resumed.DeviceAPIKeyID != initialAgent.DeviceAPIKeyID {
				t.Fatalf("warm daemon restart changed durable agent identity: before=%s/%s after=%v", initialAgent.AgentID, initialAgent.DeviceAPIKeyID, resumed)
			}
			warmRestartDone = true
		}
		if !epochRestartDone && !now.Before(epochRestartAt) {
			before := waitSandboxSharingState(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, "on", "serving", 30*time.Second)
			res := runSandboxLocalCLI(t, fixture.binary, fixture.env, fixture.stateDir, "-o", "json", "restart", fixture.local.CRID)
			after := decodeSandboxSharing(t, res)
			if after.ConnectionState != "serving" || after.ServingEpoch <= before.ServingEpoch {
				t.Fatalf("soak restart did not advance a serving epoch: before=%+v after=%+v", before, after)
			}
			epochRestartDone = true
		}

		if err := sandboxLocalRouteOnce(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, fixture.marker); err != nil {
			t.Fatalf("continuous customer request %d failed after %s: %v", requestCount, time.Since(started).Round(time.Second), err)
		}
		requestCount++
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if remaining > 30*time.Second {
			remaining = 30 * time.Second
		}
		time.Sleep(remaining)
	}
	if !warmRestartDone || !epochRestartDone {
		t.Fatalf("soak did not execute both lifecycle checkpoints (warm=%v epoch=%v)", warmRestartDone, epochRestartDone)
	}
	if requestCount < 3 {
		t.Fatalf("soak completed only %d public-route requests", requestCount)
	}
	endFDs, endRSS := sandboxProcessUsage(t)
	if startFDs >= 0 && endFDs-startFDs > 32 {
		t.Errorf("file descriptors grew from %d to %d during soak", startFDs, endFDs)
	}
	if startRSS >= 0 && endRSS-startRSS > 128*1024*1024 {
		t.Errorf("resident memory grew from %d to %d bytes during soak", startRSS, endRSS)
	}
	t.Logf("soak served %d customer requests over %s; fd=%d->%d rss=%d->%d", requestCount, time.Since(started).Round(time.Second), startFDs, endFDs, startRSS, endRSS)
}

func sandboxSoakDuration(t *testing.T) time.Duration {
	t.Helper()
	duration, err := parseSandboxSoakDuration(os.Getenv(localPublishSoakLength))
	if err != nil {
		t.Fatalf("invalid %s: %v", localPublishSoakLength, err)
	}
	return duration
}

func parseSandboxSoakDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultLocalPublishSoak, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a Go duration: %w", value, err)
	}
	if duration < minimumLocalPublishSoak {
		return 0, fmt.Errorf("%s is too short; want at least %s", duration, minimumLocalPublishSoak)
	}
	if duration > 4*time.Hour {
		return 0, fmt.Errorf("%s exceeds the four-hour CI bound", duration)
	}
	return duration, nil
}

func startCredentialFreeSandboxDaemon(t *testing.T, fixture *sandboxLocalFixture) (context.CancelFunc, <-chan *runResult) {
	t.Helper()
	env := map[string]string{
		"QURL_ENDPOINT": fixture.env["QURL_ENDPOINT"],
		hub.EnvHost:     fixture.env[hub.EnvHost], hub.EnvPort: fixture.env[hub.EnvPort],
		hub.EnvServerPublicKey: fixture.env[hub.EnvServerPublicKey],
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runResult, 1)
	configDir := t.TempDir()
	go func() {
		done <- runCLI(t, &runOpts{
			args: []string{"--endpoint", env["QURL_ENDPOINT"], "daemon", "run", "--state-dir", fixture.stateDir},
			env:  env, ctx: ctx, configDir: configDir, shareStateDir: fixture.stateDir, syncStreams: true,
		})
	}()
	return cancel, done
}

func sandboxProcessUsage(t *testing.T) (fds int, rssBytes int64) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return -1, -1
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Logf("read fd usage: %v", err)
		fds = -1
	} else {
		fds = len(entries)
	}
	file, err := os.Open("/proc/self/status")
	if err != nil {
		t.Logf("read rss usage: %v", err)
		return fds, -1
	}
	defer file.Close()
	rssBytes = -1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
			kb, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil {
				t.Logf("parse rss usage: %v", parseErr)
				break
			}
			rssBytes = kb * 1024
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Logf("scan rss usage: %v", err)
	}
	return fds, rssBytes
}

func TestSandboxSoakDurationContract(t *testing.T) {
	for _, test := range []struct {
		name, value string
		want        time.Duration
		valid       bool
	}{
		{name: "default", want: defaultLocalPublishSoak, valid: true},
		{name: "minimum", value: "75m", want: 75 * time.Minute, valid: true},
		{name: "maximum", value: "4h", want: 4 * time.Hour, valid: true},
		{name: "short", value: "74m59s"},
		{name: "long", value: "4h1s"},
		{name: "malformed", value: "eighty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(localPublishSoakLength, test.value)
			got, err := parseSandboxSoakDuration(test.value)
			if !test.valid {
				if err == nil {
					t.Fatalf("parseSandboxSoakDuration(%q) succeeded with %s", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("duration = (%s, %v), want %s", got, err, test.want)
			}
		})
	}
}
