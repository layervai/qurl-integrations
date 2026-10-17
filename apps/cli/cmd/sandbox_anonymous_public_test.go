//go:build clisandbox && (linux || darwin)

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func assertSandboxAnonymousPublicJourney(t *testing.T, binary string, publisherEnv map[string]string, publisherState string) {
	t.Helper()
	target := fmt.Sprintf("https://example.com/?qurl-anonymous-public-journey=%d", time.Now().UnixNano())
	published := runSandboxLocalCLI(t, binary, publisherEnv, publisherState, "-o", "json", "publish", target, "--public", "--description", sandboxJourneyResourceDescription(t, publisherEnv))
	if published.code != 0 {
		t.Fatalf("public publish exit = %d: %s", published.code, published.stderr.String())
	}
	var resource journeyPublishDoc
	if err := json.Unmarshal(published.stdout.Bytes(), &resource); err != nil {
		t.Fatalf("decode public publish: %v", err)
	}
	if resource.CRID == "" || resource.FoundExisting {
		t.Fatal("publish did not confirm one new resource")
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			result := runSandboxLocalCLI(t, binary, publisherEnv, publisherState, "delete", resource.CRID, "--yes")
			if result.code != 0 {
				t.Errorf("public resource cleanup exit = %d: %s", result.code, result.stderr.String())
			}
		}
	})
	if resource.Private == nil || *resource.Private {
		t.Fatal("publish did not confirm public access")
	}
	// A closed environment and empty state prevent accidental publisher authentication.
	consumerState := t.TempDir()
	consumerEnv := map[string]string{
		"QURL_ENDPOINT":   publisherEnv["QURL_ENDPOINT"],
		"QURL_DEPLOYMENT": publisherEnv["QURL_DEPLOYMENT"],
		"HOME":            t.TempDir(),
	}
	destination := filepath.Join(t.TempDir(), "anonymous-content")
	fetched := runSandboxLocalCLI(t, binary, consumerEnv, consumerState, "get", resource.CRID, "--file", destination, "--session-duration", "5m")
	if fetched.code != 0 {
		t.Fatalf("anonymous public get exit = %d: %s", fetched.code, fetched.stderr.String())
	}
	mustEmptyStdout(t, fetched)
	payload, err := os.ReadFile(destination) // #nosec G304 -- Exact test-owned destination.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(journeyTargetMarker)) || bytes.Contains(payload, []byte(apitest.InterstitialTitle)) {
		t.Fatalf("anonymous public get returned %d unexpected bytes", len(payload))
	}
	removed := runSandboxLocalCLI(t, binary, publisherEnv, publisherState, "delete", resource.CRID, "--yes")
	if removed.code != 0 {
		t.Fatalf("public delete exit = %d: %s", removed.code, removed.stderr.String())
	}
	deleted = true
	deniedDestination := filepath.Join(t.TempDir(), "deleted-content")
	denied := runSandboxLocalCLI(t, binary, consumerEnv, t.TempDir(), "get", resource.CRID, "--file", deniedDestination, "--session-duration", "5m")
	if denied.code != 5 {
		t.Fatalf("deleted public CRID exit = %d, want not found: %s", denied.code, denied.stderr.String())
	}
	mustEmptyStdout(t, denied)
	if _, err := os.Stat(deniedDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted public CRID created a file: %v", err)
	}
	t.Log("anonymous public publish, fetch and deletion passed with separate customer state")
}
