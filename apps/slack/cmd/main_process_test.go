//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSlackShutdownProcessHelper(t *testing.T) {
	if os.Getenv("QURL_SLACK_SHUTDOWN_TEST") == "1" {
		main()
	}
}

func TestSlackProcessSIGTERM(t *testing.T) {
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// #nosec G204 -- Run this test binary, never a caller-supplied executable.
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSlackShutdownProcessHelper$")
	cmd.Dir = dir
	// No inherited credentials or config; all optional external clients stay dark.
	cmd.Env = []string{
		"QURL_SLACK_SHUTDOWN_TEST=1", fmt.Sprintf("PORT=%d", port),
		"QURL_ENDPOINT=https://127.0.0.1:9", "SLACK_SIGNING_SECRET=synthetic-test-secret",
		"QURL_IMAGE_FALLBACK=dev-sandbox", "WORKSPACE_STATE_TABLE=synthetic-workspace-state",
		"WORKSPACE_STATE_KMS_KEY_ARN=arn:aws:kms:us-east-2:123456789012:key/00000000-0000-0000-0000-000000000000",
		"AWS_REGION=us-east-2", "AWS_ACCESS_KEY_ID=synthetic", "AWS_SECRET_ACCESS_KEY=synthetic",
		"AWS_EC2_METADATA_DISABLED=true", "AWS_ENDPOINT_URL=http://127.0.0.1:9",
		"AWS_CONFIG_FILE=" + filepath.Join(dir, "aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(dir, "aws-credentials"),
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	stopChild := func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
			waited = true
		}
	}
	t.Cleanup(stopChild)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 200 * time.Millisecond}
	healthy := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-done:
			waited = true
			t.Fatalf("process exited before health: %v\n%s", err, output.String())
		default:
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(request)
		if err == nil {
			_ = resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
		}
		if healthy {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !healthy {
		stopChild()
		t.Fatalf("process health never became ready\n%s", output.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Fatalf("SIGTERM exit: %v\n%s", err, output.String())
		}
	case <-time.After(shutdownTimeout + time.Second):
		stopChild()
		t.Fatalf("SIGTERM exceeded shutdown budget\n%s", output.String())
	}
	for _, message := range []string{"received shutdown signal", "server stopped cleanly"} {
		if !strings.Contains(output.String(), message) {
			t.Errorf("missing drain log %q\n%s", message, output.String())
		}
	}
}
