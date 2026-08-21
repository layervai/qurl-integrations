package ciworkflows

import (
	"strings"
	"testing"
)

const connectorResourceProofWorkflow = "cli-connector-resource-proof.yml"

// TestConnectorResourceProofWorkflowKeepsItsSafetyGates makes the attended
// live lane fail the always-on Workflow Contract check if it stops being
// main-only/current-main, loses its protected Environment or arming inputs,
// weakens cleanup, or removes either literal runtime HTTP connection trap.
func TestConnectorResourceProofWorkflowKeepsItsSafetyGates(t *testing.T) {
	workflow := readWorkflow(t, connectorResourceProofWorkflow)
	triggers := parseWorkflowTriggers(t, connectorResourceProofWorkflow, workflow.On)
	if _, ok := triggers["workflow_dispatch"]; len(triggers) != 1 || !ok {
		t.Fatalf("%s triggers = %v, want workflow_dispatch only", connectorResourceProofWorkflow, triggers)
	}
	for _, jobID := range []string{"preflight", "proof"} {
		if _, ok := workflow.Jobs[jobID]; !ok {
			t.Errorf("%s is missing %q job", connectorResourceProofWorkflow, jobID)
		}
	}

	source := readWorkflowSource(t, connectorResourceProofWorkflow)
	for _, fragment := range []string{
		`if [ "$GITHUB_REF" != "refs/heads/main" ]; then`,
		`if [ "$EXPECTED_MAIN_SHA" != "$current_main" ] || [ "$checked_out" != "$current_main" ]; then`,
		`Reverify current-main after Environment approval`,
		`Proof approval went stale: expected, checked-out, and current origin/main must remain identical`,
		`"Build and Deploy NHP" run for the NHP merge SHA`,
		`qurl-agent-key-inventory for us-east-2, layerv-nhp-sandbox-control, account`,
		`767397897469 and require exit 0 plus result: PASS`,
		`^https://github\.com/layervai/nhp/actions/runs/[0-9]+$`,
		"environment:\n      name: cli-connector-resource-sandbox",
		"QURL_CLI_SANDBOX_CONNECTOR_RESOURCE_PROOF: enabled",
		`QURL_CLI_SANDBOX_CLEANUP_JWT: ${{ secrets.QURL_CLI_SANDBOX_CLEANUP_JWT }}`,
		`QURL_CLI_SANDBOX_ENDPOINT_SHA256: ${{ vars.QURL_SANDBOX_ENDPOINT_SHA256 }}`,
		`QURL_SANDBOX_ENDPOINT is the canonical API origin https://api.layerv.xyz`,
		`the CLI appends versioned paths such as /v1/api-keys itself`,
		`6365fdb1bc9a2fcdeb40e9ba3c76154fb2b6a02c60dea9b4536b7bd8dd9a1bd4`,
		`HTTP_PROXY: ""`,
		`HTTPS_PROXY: ""`,
		`ALL_PROXY: ""`,
		`NO_PROXY: "*"`,
		`unset http_proxy https_proxy all_proxy no_proxy`,
		"'127.0.0.1 api.layerv.ai'",
		"'::1 api.layerv.ai'",
		"'127.0.0.1 bootstrap.layerv.ai'",
		"'::1 bootstrap.layerv.ai'",
		`(socket.AF_INET, ("127.0.0.1", 443))`,
		`(socket.AF_INET6, ("::1", 443, 0, 0))`,
		`socket.IPV6_V6ONLY`,
		`if sudo -n test -s "$api_trap_log"; then`,
		`-run '^TestSandboxConnectorResourceNativeProof$'`,
		`-run '^TestResolveResourcePendingPolicy$'`,
		`-run '^TestSentinelMapping$'`,
	} {
		if !strings.Contains(source, fragment) {
			t.Errorf("%s is missing load-bearing gate %q", connectorResourceProofWorkflow, fragment)
		}
	}
	if strings.Contains(source, "continue-on-error:") {
		t.Errorf("%s makes a proof step non-blocking", connectorResourceProofWorkflow)
	}
	if count := strings.Count(source, `if [ "$EXPECTED_MAIN_SHA" != "$current_main" ] || [ "$checked_out" != "$current_main" ]; then`); count != 2 {
		t.Errorf("%s current-main equality barriers = %d, want preflight plus post-approval", connectorResourceProofWorkflow, count)
	}
}
