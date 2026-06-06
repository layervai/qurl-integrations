package slack

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const slackQualityGateCondition = "needs.changes.outputs.slack == 'true'"

// Slack check names with this prefix are quality gates unless explicitly
// excluded by depending on the aggregate.
const slackCheckNamePrefix = "slack / "

type githubWorkflow struct {
	Jobs map[string]githubJob `yaml:"jobs"`
}

type githubJob struct {
	If    string `yaml:"if"`
	Name  string `yaml:"name"`
	Needs any    `yaml:"needs"`
	Steps []step `yaml:"steps"`
}

type step struct {
	Name  string `yaml:"name"`
	Run   string `yaml:"run"`
	Shell string `yaml:"shell"`
}

func TestSlackRequiredNeedsAllSlackQualityGates(t *testing.T) {
	workflow := readSlackWorkflow(t)

	required, ok := workflow.Jobs["required"]
	if !ok {
		t.Fatal("slack workflow is missing required aggregate job")
	}

	requiredNeeds := stringSet(parseWorkflowNeeds(t, "required", required.Needs))
	qualityGates := slackQualityGates(t, workflow)

	if len(qualityGates) == 0 {
		t.Fatalf("no Slack quality gates found with if containing %q", slackQualityGateCondition)
	}
	for id := range qualityGates {
		if !requiredNeeds[id] {
			t.Errorf("Slack quality gate %q is missing from required.needs", id)
		}
	}

	for need := range requiredNeeds {
		if need == "changes" {
			continue
		}
		if !qualityGates[need] {
			t.Errorf("required.needs includes %q, but no Slack quality gate with that job id exists", need)
		}
	}
}

func TestSlackRequiredVerifierDisplayNamesCoverQualityGates(t *testing.T) {
	workflow := readSlackWorkflow(t)
	script := requiredVerifierScript(t, workflow)

	for id := range slackQualityGates(t, workflow) {
		if !strings.Contains(script, id+")") {
			t.Errorf("Verify Slack CI result is missing a display_name case for %q", id)
		}
	}
}

func TestSlackRequiredVerifierScript(t *testing.T) {
	requireCommand(t, "bash")
	requireCommand(t, "jq")

	script := requiredVerifierScript(t, readSlackWorkflow(t))

	cases := []struct {
		name         string
		changes      string
		slackChanged string
		needs        map[string]string
		wantExit     bool
		wantOutput   string
	}{
		{
			name:         "no Slack changes",
			changes:      "success",
			slackChanged: "false",
			needs: map[string]string{
				"changes":            "success",
				"lint":               "skipped",
				"test":               "skipped",
				"vulnerability-scan": "skipped",
				"docker-check":       "skipped",
			},
			wantOutput: "No Slack-impacting changes detected",
		},
		{
			name:         "all Slack gates pass",
			changes:      "success",
			slackChanged: "true",
			needs: map[string]string{
				"changes":            "success",
				"lint":               "success",
				"test":               "success",
				"vulnerability-scan": "success",
				"docker-check":       "success",
			},
		},
		{
			name:         "detector fails closed",
			changes:      "failure",
			slackChanged: "",
			needs: map[string]string{
				"changes": "failure",
			},
			wantExit:   true,
			wantOutput: "slack / detect changes concluded failure",
		},
		{
			name:         "unexpected detector output fails closed",
			changes:      "success",
			slackChanged: "",
			needs: map[string]string{
				"changes": "success",
			},
			wantExit:   true,
			wantOutput: "unexpected slack output: <empty>",
		},
		{
			name:         "skipped Slack gate fails",
			changes:      "success",
			slackChanged: "true",
			needs: map[string]string{
				"changes":            "success",
				"lint":               "success",
				"test":               "skipped",
				"vulnerability-scan": "success",
				"docker-check":       "success",
			},
			wantExit:   true,
			wantOutput: "slack / test concluded skipped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runVerifierScript(t, script, tc.changes, tc.slackChanged, tc.needs)
			if tc.wantExit && err == nil {
				t.Fatalf("verifier succeeded, want failure\noutput:\n%s", output)
			}
			if !tc.wantExit && err != nil {
				t.Fatalf("verifier failed: %v\noutput:\n%s", err, output)
			}
			if tc.wantOutput != "" && !strings.Contains(output, tc.wantOutput) {
				t.Fatalf("verifier output = %q, want substring %q", output, tc.wantOutput)
			}
		})
	}
}

func looksLikeSlackQualityGate(job githubJob, needs []string) bool {
	if !strings.HasPrefix(job.Name, slackCheckNamePrefix) {
		return false
	}
	if job.Name == "slack / detect changes" || job.Name == "slack / required" {
		return false
	}
	return !containsString(needs, "required")
}

func slackQualityGates(t *testing.T, workflow githubWorkflow) map[string]bool {
	t.Helper()

	qualityGates := map[string]bool{}
	for id, job := range workflow.Jobs {
		needs := parseWorkflowNeeds(t, id, job.Needs)
		if !looksLikeSlackQualityGate(job, needs) {
			continue
		}
		if !containsString(needs, "changes") {
			t.Errorf("Slack quality gate %q must include changes in needs", id)
			continue
		}
		if !strings.Contains(job.If, slackQualityGateCondition) {
			t.Errorf("Slack quality gate %q must include if condition %q", id, slackQualityGateCondition)
			continue
		}
		qualityGates[id] = true
	}
	return qualityGates
}

func readSlackWorkflow(t *testing.T) githubWorkflow {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "slack.yml"))
	if err != nil {
		t.Fatalf("read slack workflow: %v", err)
	}

	var workflow githubWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse slack workflow: %v", err)
	}
	return workflow
}

func parseWorkflowNeeds(t *testing.T, jobID string, needs any) []string {
	t.Helper()

	switch typed := needs.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, raw := range typed {
			need, ok := raw.(string)
			if !ok {
				t.Fatalf("%s.needs contains non-string value %T", jobID, raw)
			}
			out = append(out, need)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		t.Fatalf("%s.needs has unexpected type %T", jobID, needs)
		return nil
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func requiredVerifierScript(t *testing.T, workflow githubWorkflow) string {
	t.Helper()

	required, ok := workflow.Jobs["required"]
	if !ok {
		t.Fatal("slack workflow is missing required aggregate job")
	}
	for _, step := range required.Steps {
		if step.Name != "Verify Slack CI result" {
			continue
		}
		if step.Shell != "bash" {
			t.Fatalf("Verify Slack CI result shell = %q, want bash", step.Shell)
		}
		if strings.TrimSpace(step.Run) == "" {
			t.Fatal("Verify Slack CI result step has empty run script")
		}
		return step.Run
	}
	t.Fatal("required job is missing Verify Slack CI result step")
	return ""
}

func runVerifierScript(t *testing.T, script, changesResult, slackChanged string, needs map[string]string) (string, error) {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "verify-slack-ci-result.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write verifier script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// #nosec G204 -- scriptPath is a test-created file containing the checked-in workflow step.
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", scriptPath)
	cmd.Env = append(os.Environ(),
		"CHANGES_RESULT="+changesResult,
		"SLACK_CHANGED="+slackChanged,
		"NEEDS_JSON="+needsJSON(t, needs),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func needsJSON(t *testing.T, results map[string]string) string {
	t.Helper()

	type need struct {
		Result string `json:"result"`
	}
	needs := make(map[string]need, len(results))
	for job, result := range results {
		needs[job] = need{Result: result}
	}
	data, err := json.Marshal(needs)
	if err != nil {
		t.Fatalf("marshal needs: %v", err)
	}
	return string(data)
}

func requireCommand(t *testing.T, name string) {
	t.Helper()

	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found in PATH: %v", name, err)
	}
}
