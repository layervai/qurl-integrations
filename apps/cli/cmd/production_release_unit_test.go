//go:build clisandbox && cliproduction

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestProductionProofCommandEnvironment(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "QURL_API_KEY_FILE", "QURL_DEPLOYMENT", "QURL_CONNECTOR_HUB_HOST", "GH_TOKEN", "AUTOMATION_API_KEY", "HTTPS_PROXY"} {
		t.Setenv(name, "must-not-reach-candidate")
	}
	raw, code := productionCommand(t, os.Args[0], map[string]string{"PROOF_FIXTURE": "success"}, "one-time-fixture-key\n", "-test.run=^TestProductionProofCommandFixture$")
	if code != 0 {
		t.Fatalf("fixture command exit = %d", code)
	}
	var result struct {
		Isolated bool
		InputOK  bool
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.Isolated || !result.InputOK {
		t.Fatal("candidate command inherited standing authority or lost its one-time input")
	}
}

func TestProductionProofPreservesCLIErrorExit(t *testing.T) {
	_, code := productionCommand(t, os.Args[0], map[string]string{"PROOF_FIXTURE": "reject"}, "", "-test.run=^TestProductionProofCommandFixture$")
	if code != 8 {
		t.Fatalf("production HTTP 400 became exit %d, want 8", code)
	}
}

func TestProductionProofCommandFixture(t *testing.T) {
	mode := os.Getenv("PROOF_FIXTURE")
	if mode == "" {
		return
	}
	if mode == "reject" {
		_, _ = fmt.Fprintln(os.Stderr, "Validation Error (HTTP 400)")
		os.Exit(8)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	isolated := true
	for _, entry := range os.Environ() {
		if strings.Contains(entry, "must-not-reach-candidate") {
			isolated = false
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]bool{"Isolated": isolated, "InputOK": string(input) == "one-time-fixture-key\n"}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
