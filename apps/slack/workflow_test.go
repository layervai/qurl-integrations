package slack

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const slackQualityGateCondition = "needs.changes.outputs.slack == 'true'"

type requiredWorkflowSpec struct {
	name                 string
	path                 string
	checkNamePrefix      string
	changeOutput         string
	changedEnv           string
	qualityGateCondition string
	detectChangesName    string
	requiredName         string
	verifierStepName     string
	unchangedOutput      string
}

var requiredWorkflowSpecs = []requiredWorkflowSpec{
	{
		name:                 "slack",
		path:                 "slack.yml",
		checkNamePrefix:      "slack / ",
		changeOutput:         "slack",
		changedEnv:           "SLACK_CHANGED",
		qualityGateCondition: slackQualityGateCondition,
		detectChangesName:    "slack / detect changes",
		requiredName:         "slack / required",
		verifierStepName:     "Verify Slack CI result",
		unchangedOutput:      "No Slack-impacting changes detected",
	},
	{
		name:                 "discord",
		path:                 "discord.yml",
		checkNamePrefix:      "discord / ",
		changeOutput:         "discord",
		changedEnv:           "DISCORD_CHANGED",
		qualityGateCondition: "needs.changes.outputs.discord == 'true'",
		detectChangesName:    "discord / detect changes",
		requiredName:         "discord / required",
		verifierStepName:     "Verify Discord CI result",
		unchangedOutput:      "No Discord-impacting changes detected",
	},
	{
		name:                 "chrome-extension",
		path:                 "chrome-extension.yml",
		checkNamePrefix:      "chrome-extension / ",
		changeOutput:         "chrome_extension",
		changedEnv:           "CHROME_EXTENSION_CHANGED",
		qualityGateCondition: "needs.changes.outputs.chrome_extension == 'true'",
		detectChangesName:    "chrome-extension / detect changes",
		requiredName:         "chrome-extension / required",
		verifierStepName:     "Verify Chrome extension CI result",
		unchangedOutput:      "No Chrome extension-impacting changes detected",
	},
	{
		name:                 "edge-extension",
		path:                 "edge-extension.yml",
		checkNamePrefix:      "edge-extension / ",
		changeOutput:         "edge_extension",
		changedEnv:           "EDGE_EXTENSION_CHANGED",
		qualityGateCondition: "needs.changes.outputs.edge_extension == 'true'",
		detectChangesName:    "edge-extension / detect changes",
		requiredName:         "edge-extension / required",
		verifierStepName:     "Verify Edge extension CI result",
		unchangedOutput:      "No Edge extension-impacting changes detected",
	},
	{
		name:                 "shared",
		path:                 "shared-test.yml",
		checkNamePrefix:      "shared / ",
		changeOutput:         "shared",
		changedEnv:           "SHARED_CHANGED",
		qualityGateCondition: "needs.changes.outputs.shared == 'true'",
		detectChangesName:    "shared / detect changes",
		requiredName:         "shared / required",
		verifierStepName:     "Verify shared CI result",
		unchangedOutput:      "No shared-impacting changes detected",
	},
}

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

func TestRequiredWorkflowsNeedAllQualityGates(t *testing.T) {
	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			workflow := readWorkflow(t, spec.path)

			required, ok := workflow.Jobs["required"]
			if !ok {
				t.Fatalf("%s workflow is missing required aggregate job", spec.name)
			}
			if required.Name != spec.requiredName {
				t.Fatalf("required job name = %q, want %q", required.Name, spec.requiredName)
			}

			requiredNeeds := stringSet(parseWorkflowNeeds(t, "required", required.Needs))
			if !requiredNeeds["changes"] {
				t.Fatal("required.needs is missing changes detector")
			}

			qualityGates := requiredWorkflowQualityGates(t, spec, workflow)
			if len(qualityGates) == 0 {
				t.Fatalf("no %s quality gates found with if containing %q", spec.name, spec.qualityGateCondition)
			}
			for id := range qualityGates {
				if !requiredNeeds[id] {
					t.Errorf("%s quality gate %q is missing from required.needs", spec.name, id)
				}
			}

			for need := range requiredNeeds {
				if need == "changes" {
					continue
				}
				if !qualityGates[need] {
					t.Errorf("required.needs includes %q, but no %s quality gate with that job id exists", need, spec.name)
				}
			}
		})
	}
}

func TestRequiredWorkflowVerifierDisplayNamesCoverQualityGates(t *testing.T) {
	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			workflow := readWorkflow(t, spec.path)
			script := requiredVerifierScriptNamed(t, workflow, spec.verifierStepName)

			for id := range requiredWorkflowQualityGates(t, spec, workflow) {
				if !strings.Contains(script, id+")") {
					t.Errorf("%s is missing a display_name case for %q", spec.verifierStepName, id)
				}
			}
		})
	}
}

func TestRequiredWorkflowVerifierScripts(t *testing.T) {
	requireCommand(t, "bash")
	requireCommand(t, "jq")

	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			workflow := readWorkflow(t, spec.path)
			script := requiredVerifierScriptNamed(t, workflow, spec.verifierStepName)
			qualityGates := sortedQualityGateIDs(requiredWorkflowQualityGates(t, spec, workflow))
			if len(qualityGates) == 0 {
				t.Fatal("no quality gates found")
			}

			needs := map[string]string{"changes": "success"}
			for _, id := range qualityGates {
				needs[id] = "success"
			}

			unchangedNeeds := map[string]string{"changes": "success"}
			for _, id := range qualityGates {
				unchangedNeeds[id] = "skipped"
			}
			output, err := runVerifierScriptWithEnv(t, script, map[string]string{
				"CHANGES_RESULT": "success",
				spec.changedEnv:  "false",
				"NEEDS_JSON":     needsJSON(t, unchangedNeeds),
			})
			if err != nil {
				t.Fatalf("unchanged verifier failed: %v\noutput:\n%s", err, output)
			}
			if !strings.Contains(output, spec.unchangedOutput) {
				t.Fatalf("unchanged verifier output = %q, want substring %q", output, spec.unchangedOutput)
			}

			output, err = runVerifierScriptWithEnv(t, script, map[string]string{
				"CHANGES_RESULT": "success",
				spec.changedEnv:  "true",
				"NEEDS_JSON":     needsJSON(t, needs),
			})
			if err != nil {
				t.Fatalf("changed verifier failed: %v\noutput:\n%s", err, output)
			}

			output, err = runVerifierScriptWithEnv(t, script, map[string]string{
				"CHANGES_RESULT": "failure",
				spec.changedEnv:  "",
				"NEEDS_JSON":     needsJSON(t, map[string]string{"changes": "failure"}),
			})
			if err == nil {
				t.Fatalf("detector failure verifier succeeded, want failure\noutput:\n%s", output)
			}
			if !strings.Contains(output, spec.detectChangesName+" concluded failure") {
				t.Fatalf("detector failure output = %q, want %q", output, spec.detectChangesName+" concluded failure")
			}

			output, err = runVerifierScriptWithEnv(t, script, map[string]string{
				"CHANGES_RESULT": "success",
				spec.changedEnv:  "",
				"NEEDS_JSON":     needsJSON(t, map[string]string{"changes": "success"}),
			})
			if err == nil {
				t.Fatalf("unexpected output verifier succeeded, want failure\noutput:\n%s", output)
			}
			if !strings.Contains(output, "unexpected "+spec.changeOutput+" output: <empty>") {
				t.Fatalf("unexpected output message = %q, want unexpected %s output", output, spec.changeOutput)
			}

			skippedGate := qualityGates[0]
			needs[skippedGate] = "skipped"
			output, err = runVerifierScriptWithEnv(t, script, map[string]string{
				"CHANGES_RESULT": "success",
				spec.changedEnv:  "true",
				"NEEDS_JSON":     needsJSON(t, needs),
			})
			if err == nil {
				t.Fatalf("skipped gate verifier succeeded, want failure\noutput:\n%s", output)
			}
			wantGateOutput := workflow.Jobs[skippedGate].Name + " concluded skipped"
			if !strings.Contains(output, wantGateOutput) {
				t.Fatalf("skipped gate output = %q, want substring %q", output, wantGateOutput)
			}
		})
	}
}

func readWorkflow(t *testing.T, name string) githubWorkflow {
	t.Helper()

	// #nosec G304 -- callers pass checked-in workflow names from requiredWorkflowSpecs.
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s workflow: %v", name, err)
	}

	var workflow githubWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s workflow: %v", name, err)
	}
	return workflow
}

func requiredWorkflowQualityGates(t *testing.T, spec *requiredWorkflowSpec, workflow githubWorkflow) map[string]bool {
	t.Helper()

	qualityGates := map[string]bool{}
	for id, job := range workflow.Jobs {
		needs := parseWorkflowNeeds(t, id, job.Needs)
		if !looksLikeRequiredWorkflowQualityGate(spec, job, needs) {
			continue
		}
		if !containsString(needs, "changes") {
			t.Errorf("%s quality gate %q must include changes in needs", spec.name, id)
			continue
		}
		if !strings.Contains(job.If, spec.qualityGateCondition) {
			t.Errorf("%s quality gate %q must include if condition %q", spec.name, id, spec.qualityGateCondition)
			continue
		}
		qualityGates[id] = true
	}
	return qualityGates
}

func looksLikeRequiredWorkflowQualityGate(spec *requiredWorkflowSpec, job githubJob, needs []string) bool {
	if !strings.HasPrefix(job.Name, spec.checkNamePrefix) {
		return false
	}
	if job.Name == spec.detectChangesName || job.Name == spec.requiredName {
		return false
	}
	return !containsString(needs, "required")
}

func sortedQualityGateIDs(qualityGates map[string]bool) []string {
	ids := make([]string, 0, len(qualityGates))
	for id := range qualityGates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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

func requiredVerifierScriptNamed(t *testing.T, workflow githubWorkflow, stepName string) string {
	t.Helper()

	required, ok := workflow.Jobs["required"]
	if !ok {
		t.Fatal("slack workflow is missing required aggregate job")
	}
	for _, step := range required.Steps {
		if step.Name != stepName {
			continue
		}
		if step.Shell != "bash" {
			t.Fatalf("%s shell = %q, want bash", stepName, step.Shell)
		}
		if strings.TrimSpace(step.Run) == "" {
			t.Fatalf("%s step has empty run script", stepName)
		}
		return step.Run
	}
	t.Fatalf("required job is missing %s step", stepName)
	return ""
}

func runVerifierScriptWithEnv(t *testing.T, script string, env map[string]string) (string, error) {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "verify-required-ci-result.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write verifier script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// #nosec G204 -- scriptPath is a test-created file containing the checked-in workflow step.
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", scriptPath)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
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
