//go:build clisandbox && clisoak && (linux || darwin)

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
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
	foregroundOwned := true
	defer func() {
		if foregroundOwned {
			fixture.interruptAndValidate(t)
		}
	}()

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
	routeDest := filepath.Join(t.TempDir(), "payload")

	var warmDaemon *sandboxExactDaemonProcess
	defer func() {
		if warmDaemon != nil {
			warmDaemon.interruptAndValidate(t, fixture.key, fixture.cleanupJWT)
		}
	}()

	for time.Now().Before(deadline) {
		now := time.Now()
		if !warmRestartDone && !now.Before(warmRestartAt) {
			foregroundOwned = false
			fixture.interruptAndValidate(t)
			warmDaemon = startCredentialFreeSandboxDaemon(t, fixture)
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

		if err := sandboxLocalRouteOnce(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, fixture.marker, routeDest); err != nil {
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

type sandboxExactDaemonProcess struct {
	cmd            *exec.Cmd
	stdout, stderr lockedSandboxBuffer
	done           chan error
	stopped        bool
	reaped         bool
}

func startCredentialFreeSandboxDaemon(t *testing.T, fixture *sandboxLocalFixture) *sandboxExactDaemonProcess {
	t.Helper()
	env := map[string]string{
		"QURL_ENDPOINT":          fixture.env["QURL_ENDPOINT"],
		"QURL_DEPLOYMENT":        fixture.env["QURL_DEPLOYMENT"],
		sandboxRunIDEnv:          fixture.env[sandboxRunIDEnv],
		sandboxRunAttemptEnv:     fixture.env[sandboxRunAttemptEnv],
		sandboxRuntimeEnv:        fixture.env[sandboxRuntimeEnv],
		state.EnvStateDirPrimary: fixture.stateDir,
		hub.EnvHost:              fixture.env[hub.EnvHost],
		hub.EnvPort:              fixture.env[hub.EnvPort],
		hub.EnvServerPublicKey:   fixture.env[hub.EnvServerPublicKey],
	}
	process := &sandboxExactDaemonProcess{done: make(chan error, 1)}
	process.cmd = exec.CommandContext( //nolint:gosec // The protected test validates the exact binary and fixed arguments.
		context.Background(),
		fixture.binary,
		"--endpoint", env["QURL_ENDPOINT"],
		"daemon", "run", "--state-dir", fixture.stateDir,
	)
	process.cmd.Env = sandboxCommandEnv(env)
	process.cmd.Stdin = nil
	process.cmd.Stdout = &process.stdout
	process.cmd.Stderr = &process.stderr
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start exact credential-free warm daemon: %v", err)
	}
	go func() {
		process.done <- process.cmd.Wait()
	}()
	t.Cleanup(func() { process.forceStop(t) })
	return process
}

func TestExactWarmDaemonProcessContract(t *testing.T) {
	stateDir := t.TempDir()
	deployment := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(deployment, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "qurl")
	script := `#!/bin/sh
set -eu
state_dir=$6
{
  printf 'arg=%s\n' "$@"
  printf 'api_key=%s\n' "${QURL_API_KEY-unset}"
  printf 'agent=%s\n' "${QURL_CONNECTOR_AGENT_ID-unset}"
  printf 'deployment=%s\n' "${QURL_DEPLOYMENT-unset}"
  printf 'run=%s/%s/%s\n' "${QURL_SHARING_RUN_ID-unset}" "${QURL_SHARING_RUN_ATTEMPT-unset}" "${QURL_SHARING_RUNTIME-unset}"
  printf 'state=%s\n' "${QURL_CONNECTOR_STATE_DIR-unset}"
} > "$state_dir/invocation"
trap 'exit 130' INT TERM
: > "$state_dir/ready"
while :; do sleep 0.1; done
`
	if err := os.WriteFile(binary, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o500); err != nil { //nolint:gosec // The exact-binary fixture must be executable and non-writable.
		t.Fatal(err)
	}
	fixture := &sandboxLocalFixture{
		binary:   binary,
		stateDir: stateDir,
		env: map[string]string{
			"QURL_API_KEY":         "must-not-reach-daemon",
			"QURL_ENDPOINT":        "https://api.example.test",
			"QURL_DEPLOYMENT":      deployment,
			state.EnvAgentID:       "must-not-reach-daemon-agent",
			sandboxRunIDEnv:        "12345",
			sandboxRunAttemptEnv:   "2",
			sandboxRuntimeEnv:      "hardened_container",
			hub.EnvHost:            "hub.example.test",
			hub.EnvPort:            "443",
			hub.EnvServerPublicKey: "public-key",
		},
	}
	process := startCredentialFreeSandboxDaemon(t, fixture)
	ready := filepath.Join(stateDir, "ready")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Lstat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("exact warm daemon fixture did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	process.interruptAndValidate(t, "must-not-reach-daemon")
	invocation, err := os.ReadFile(filepath.Join(stateDir, "invocation")) //nolint:gosec // The path is inside this test's private temporary state directory.
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"arg=--endpoint",
		"arg=https://api.example.test",
		"arg=daemon",
		"arg=run",
		"arg=--state-dir",
		"arg=" + stateDir,
		"api_key=unset",
		"agent=unset",
		"deployment=" + deployment,
		"run=12345/2/hardened_container",
		"state=" + stateDir,
		"",
	}, "\n")
	if string(invocation) != want {
		t.Fatalf("exact warm daemon invocation = %q, want %q", invocation, want)
	}
}

func (p *sandboxExactDaemonProcess) interruptAndValidate(t *testing.T, secrets ...string) {
	t.Helper()
	if p.stopped {
		t.Fatal("exact warm daemon was stopped twice")
	}
	p.stopped = true
	select {
	case err := <-p.done:
		p.reaped = true
		t.Fatalf("exact warm daemon exited before the requested stop: %v", err)
	default:
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt exact warm daemon: %v", err)
	}
	select {
	case waitErr := <-p.done:
		p.reaped = true
		if err := validateSandboxInterruptedExit(waitErr); err != nil {
			t.Fatalf("exact warm daemon %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("exact warm daemon did not stop after interrupt")
	}
	if err := validateSandboxProtectedProcessOutput(p.stdout.String(), p.stderr.String(), secrets...); err != nil {
		t.Fatalf("exact warm daemon output: %v", err)
	}
}

func (p *sandboxExactDaemonProcess) forceStop(t *testing.T) {
	t.Helper()
	if p == nil || p.reaped || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		p.reaped = true
		return
	case <-time.After(5 * time.Second):
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
		p.reaped = true
	case <-time.After(5 * time.Second):
		t.Error("exact warm daemon could not be reaped after interrupt and kill")
	}
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
	defer func() {
		if err := file.Close(); err != nil {
			t.Logf("close process status: %v", err)
		}
	}()
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
