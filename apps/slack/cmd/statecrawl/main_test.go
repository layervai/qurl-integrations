package main

import (
	"flag"
	"strings"
	"testing"
)

// Deployment-shaped fixture names, mirroring what qurl-integrations-infra
// actually renders, so a case claiming to exercise "the sandbox wiring" asserts
// against the real string. modules/qurl-slack-ddb prefixes the two scanned
// tables with `qurl-bot-slack-<env>-`; workspace_state is env-agnostic
// (qurl-bot-slack/terraform/workspace_state.tf) and byte-identical in sandbox
// and prod, which is exactly why looksProd does not scan it.
const (
	sandboxPoliciesTable = "qurl-bot-slack-sandbox-channel-policies"
	sandboxMappingsTable = "qurl-bot-slack-sandbox-workspace-mappings"
	prodPoliciesTable    = "qurl-bot-slack-production-channel-policies"
	prodMappingsTable    = "qurl-bot-slack-production-workspace-mappings"
	stateTableName       = "qurl-bot-slack-workspace-state"
	testKMSKeyARN        = "arn:aws:kms:us-east-1:111122223333:key/abc"
)

// wiringArgs builds a complete flag set from an explicit deployment wiring, so
// a test can state the shape it exercises — which tables, which endpoint —
// without repeating the flag names or tripping the missing-config check. The
// workspace-state table and CMK are fixed: neither varies by environment, and
// no rail looks at them.
func wiringArgs(policiesTable, mappingsTable, endpoint string, extra ...string) []string {
	args := make([]string, 0, 10+len(extra))
	args = append(args,
		"-channel-policies-table", policiesTable,
		"-workspace-mappings-table", mappingsTable,
		"-workspace-state-table", stateTableName,
		"-kms-key-arn", testKMSKeyARN,
		"-qurl-endpoint", endpoint,
	)
	return append(args, extra...)
}

// baseArgs is the sandbox wiring — the minimal required flags so a test can
// focus on the rail under exercise. Passed explicitly rather than read from the
// environment so the result is hermetic regardless of the runner's env.
func baseArgs(extra ...string) []string {
	return wiringArgs(sandboxPoliciesTable, sandboxMappingsTable, "https://sandbox.qurl.example", extra...)
}

func parse(t *testing.T, args []string) (*flags, error) {
	t.Helper()
	return parseFlags(flag.NewFlagSet("test", flag.ContinueOnError), args)
}

// TestParseFlags_DefaultsToDryRun pins the safe default: with no -dry-run flag,
// the run is read-only.
func TestParseFlags_DefaultsToDryRun(t *testing.T) {
	f, err := parse(t, baseArgs())
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.dryRun {
		t.Error("dry-run must default to true (the safe default)")
	}
}

// TestParseFlags_MissingConfigRejected lists every missing required table when
// none are provided, so an operator sees the full set at once.
func TestParseFlags_MissingConfigRejected(t *testing.T) {
	t.Setenv("QURL_CHANNEL_POLICIES_TABLE", "")
	t.Setenv("QURL_WORKSPACE_MAPPINGS_TABLE", "")
	t.Setenv("WORKSPACE_STATE_TABLE", "")
	t.Setenv("WORKSPACE_STATE_KMS_KEY_ARN", "")
	t.Setenv("QURL_ENDPOINT", "")
	_, err := parse(t, nil)
	if err == nil {
		t.Fatal("parseFlags accepted a run with no required config")
	}
	if !strings.Contains(err.Error(), "QURL_CHANNEL_POLICIES_TABLE") {
		t.Errorf("error must name the missing vars; got: %v", err)
	}
}

// TestParseFlags_ProdPurgeWithoutAllowRejected is the core rail: a mutating run
// (-dry-run=false) against a prod-labeled deployment without -allow-prod-purge
// is refused, and the error MUST name the opt-in flag for operator triage.
func TestParseFlags_ProdPurgeWithoutAllowRejected(t *testing.T) {
	_, err := parse(t, baseArgs("-env", "prod", "-dry-run=false"))
	if err == nil {
		t.Fatal("parseFlags accepted a prod purge without -allow-prod-purge — the rail is missing")
	}
	if !strings.Contains(err.Error(), "-allow-prod-purge") {
		t.Errorf("error MUST surface the -allow-prod-purge opt-in; got: %v", err)
	}
}

// TestParseFlags_ProdPurgeWithAllowAccepted confirms the explicit opt-in lets a
// reviewed prod purge through.
func TestParseFlags_ProdPurgeWithAllowAccepted(t *testing.T) {
	f, err := parse(t, baseArgs("-env", "prod", "-dry-run=false", "-allow-prod-purge"))
	if err != nil {
		t.Fatalf("parseFlags rejected a properly opted-in prod purge: %v", err)
	}
	if f.dryRun || !f.allowProdPurge {
		t.Errorf("flags = %+v, want dryRun=false allowProdPurge=true", f)
	}
}

// TestParseFlags_ProdDetectedByTableName is defense-in-depth: a forgotten
// -env=prod still trips the rail when a resolved table name says prod.
//
// The workspace-state table deliberately carries its real, environment-agnostic
// infra name here rather than a "prod" one: it is not scanned (see looksProd),
// so a prod-flavored value would make this test pass for a reason it does not
// actually exercise, and would keep passing if the table-name scan broke.
func TestParseFlags_ProdDetectedByTableName(t *testing.T) {
	_, err := parse(t, wiringArgs(prodPoliciesTable, prodMappingsTable, "https://qurl.example", "-dry-run=false"))
	if err == nil {
		t.Fatal("parseFlags accepted a purge against prod-named tables without -allow-prod-purge")
	}
	if !strings.Contains(err.Error(), "-allow-prod-purge") {
		t.Errorf("error must name the opt-in flag; got: %v", err)
	}
}

// TestParseFlags_SandboxPurgeNoAllowAccepted confirms the rail is prod-only: a
// non-prod deployment can purge without the extra opt-in, so sandbox triage is
// frictionless.
func TestParseFlags_SandboxPurgeNoAllowAccepted(t *testing.T) {
	f, err := parse(t, baseArgs("-env", "sandbox", "-dry-run=false"))
	if err != nil {
		t.Fatalf("parseFlags rejected a sandbox purge without -allow-prod-purge: %v", err)
	}
	if f.dryRun {
		t.Error("dryRun = true, want false")
	}
}

// TestParseFlags_UnlabeledSandboxPurgeAccepted is the acceptance-path
// complement to TestParseFlags_ProdDetectedByTableName: the same shape of run
// with no -env label at all, wired entirely to sandbox, must be ACCEPTED
// without -allow-prod-purge.
//
// Worth its own case because the sibling acceptance test passes -env sandbox,
// so an explicit non-prod label is doing the work there. This one pins that an
// UNLABELED run is not prod-by-default — a plausible "hardening" (treat a
// missing label as prod) would break every unlabeled sandbox purge while
// leaving every rejection test green.
func TestParseFlags_UnlabeledSandboxPurgeAccepted(t *testing.T) {
	f, err := parse(t, wiringArgs(sandboxPoliciesTable, sandboxMappingsTable, "https://api.layerv.xyz", "-dry-run=false"))
	if err != nil {
		t.Fatalf("parseFlags rejected an unlabeled all-sandbox purge: %v", err)
	}
	if f.dryRun {
		t.Error("dryRun = true, want false")
	}
}

// TestParseFlags_BadLogFormatRejected guards the log-format enum.
func TestParseFlags_BadLogFormatRejected(t *testing.T) {
	if _, err := parse(t, baseArgs("-log-format", "yaml")); err == nil {
		t.Error("parseFlags accepted an invalid -log-format")
	}
}

func TestLooksProd(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    flags
		want bool
	}{
		{"env prod", flags{envLabel: "prod"}, true},
		{"env production", flags{envLabel: "Production"}, true},
		{"env sandbox", flags{envLabel: "sandbox"}, false},
		{"policies table prod", flags{channelPoliciesTable: "qurl-bot-slack-prod-cp"}, true},
		{"mappings table prod", flags{workspaceMappingsTable: "qurl-bot-slack-prod-wm"}, true},
		{"endpoint prod substring", flags{qurlEndpoint: "https://qurl-prod.example/v1"}, true},
		{"endpoint canonical prod origin", flags{qurlEndpoint: "https://api.layerv.ai/v1"}, true},
		// The endpoint check matches the whole layerv.ai domain, not the exact
		// api.layerv.ai host. Pin that breadth: narrowing it to the canonical
		// host would turn any other prod subdomain into a silent bypass of the
		// rail, and nothing else here would catch that.
		{"endpoint non-canonical layerv.ai host", flags{qurlEndpoint: "https://staging.layerv.ai/v1"}, true},
		{"endpoint sandbox origin", flags{qurlEndpoint: "https://api.layerv.xyz/v1"}, false},
		{"all sandbox", flags{envLabel: "sandbox", channelPoliciesTable: "qurl-sandbox-cp"}, false},

		// workspace_state is NOT scanned: its infra-rendered name carries no
		// environment in ANY deployment, so the check could never fire (see
		// looksProd's comment). Pin the absence — if someone re-adds the dead
		// loop entry, this case fails and points them at the reasoning rather
		// than letting it read as real coverage.
		{"state table is not scanned at all", flags{workspaceStateTable: "qurl-prod-state"}, false},

		// Real deployment wirings, with the -env label an operator forgot to
		// pass. Prod must still trip on what the rail actually guards; sandbox
		// must NOT, or operators learn to pass -allow-prod-purge reflexively
		// and the rail stops meaning anything.
		{"real prod wiring, unlabeled", flags{
			channelPoliciesTable:   prodPoliciesTable,
			workspaceMappingsTable: prodMappingsTable,
			workspaceStateTable:    stateTableName,
			qurlEndpoint:           "https://api.layerv.ai",
		}, true},
		{"real sandbox wiring, unlabeled", flags{
			channelPoliciesTable:   sandboxPoliciesTable,
			workspaceMappingsTable: sandboxMappingsTable,
			workspaceStateTable:    stateTableName,
			qurlEndpoint:           "https://api.layerv.xyz",
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksProd(&tc.f); got != tc.want {
				t.Errorf("looksProd(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}
