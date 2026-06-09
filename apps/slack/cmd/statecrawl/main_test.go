package main

import (
	"flag"
	"strings"
	"testing"
)

// baseArgs returns the minimal required flags so a test can focus on the rail
// under exercise without tripping the missing-config check. Tables/endpoint are
// passed explicitly so the result is hermetic regardless of the runner's env.
func baseArgs(extra ...string) []string {
	args := []string{
		"-channel-policies-table", "qurl-bot-slack-sandbox-channel-policies",
		"-workspace-mappings-table", "qurl-bot-slack-sandbox-workspace-mappings",
		"-workspace-state-table", "qurl-sandbox-workspace-state",
		"-kms-key-arn", "arn:aws:kms:us-east-1:111122223333:key/abc",
		"-qurl-endpoint", "https://sandbox.qurl.example",
	}
	return append(args, extra...)
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
// (-dry-run=false) against a prod-labelled deployment without -allow-prod-purge
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
func TestParseFlags_ProdDetectedByTableName(t *testing.T) {
	args := []string{
		"-channel-policies-table", "qurl-bot-slack-prod-channel-policies",
		"-workspace-mappings-table", "qurl-bot-slack-prod-workspace-mappings",
		"-workspace-state-table", "qurl-prod-workspace-state",
		"-kms-key-arn", "arn:aws:kms:us-east-1:111122223333:key/abc",
		"-qurl-endpoint", "https://qurl.example",
		"-dry-run=false",
	}
	_, err := parse(t, args)
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
		{"table prod", flags{channelPoliciesTable: "qurl-bot-slack-prod-cp"}, true},
		{"state table prod", flags{workspaceStateTable: "qurl-prod-state"}, true},
		{"all sandbox", flags{envLabel: "sandbox", channelPoliciesTable: "qurl-sandbox-cp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksProd(&tc.f); got != tc.want {
				t.Errorf("looksProd(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}
