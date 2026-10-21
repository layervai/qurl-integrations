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

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
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
	// Keep the public Hub bootstrap in sandbox without carrying publisher credentials.
	consumerState, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve anonymous consumer state directory: %v", err)
	}
	publisherDeployment, err := qurl.LoadDeployment(publisherEnv["QURL_DEPLOYMENT"])
	if err != nil {
		t.Fatal("publisher deployment settings are invalid")
	}
	anonymousDeployment, err := anonymousPublicDeployment(publisherDeployment, os.Getenv("QURL_SANDBOX_CRID_CELL_PUBLIC_KEY_B64"), os.Getenv("QURL_SANDBOX_QV2_RELAY_URL"))
	if err != nil {
		t.Fatal(err)
	}
	deploymentJSON, err := json.Marshal(anonymousDeployment)
	if err != nil {
		t.Fatal("anonymous deployment encoding failed")
	}
	deploymentPath := filepath.Join(t.TempDir(), "anonymous-deployment.json")
	if err := os.WriteFile(deploymentPath, deploymentJSON, 0o600); err != nil {
		t.Fatal("anonymous deployment could not be saved")
	}
	consumerEnv := map[string]string{
		"QURL_ENDPOINT":        publisherEnv["QURL_ENDPOINT"],
		"QURL_DEPLOYMENT":      deploymentPath,
		hub.EnvHost:            publisherEnv[hub.EnvHost],
		hub.EnvPort:            publisherEnv[hub.EnvPort],
		hub.EnvServerPublicKey: publisherEnv[hub.EnvServerPublicKey],
		"HOME":                 t.TempDir(),
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
	deniedState, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve deleted-resource consumer state directory: %v", err)
	}
	denied := runSandboxLocalCLI(t, binary, consumerEnv, deniedState, "get", resource.CRID, "--file", deniedDestination, "--session-duration", "5m")
	if denied.code != 5 {
		t.Fatalf("deleted public CRID exit = %d, want not found: %s", denied.code, denied.stderr.String())
	}
	mustEmptyStdout(t, denied)
	if _, err := os.Stat(deniedDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted public CRID created a file: %v", err)
	}
	t.Log("anonymous public publish, fetch and deletion passed with separate customer state")
}
