// Package ciworkflows holds repo-wide CI contract tests: assertions about the
// shape of every workflow in .github/workflows, not about any one app.
//
// It lives here rather than under apps/<app>/ because an app workflow's paths
// filter decides when that app's tests run, and these tests read every
// workflow file. Sitting in apps/slack, they inherited slack.yml's filter,
// which matches `.github/workflows/slack.yml` alone — so a PR adding a new
// workflow skipped them entirely and shipped an unregistered aggregate green
// (#1081). `.github/workflows/workflow-contract.yml` runs this package
// unfiltered on every PR and merge group instead.
package ciworkflows

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

const (
	slackQualityGateCondition = "needs.changes.outputs.slack == 'true'"
	workflowContractCheckName = "Workflow Contract"
	workflowContractTestName  = "Test workflow contract"
	workflowContractTestRun   = "go test -count=1 ./internal/ciworkflows/..."
)

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
		name:                 "teams",
		path:                 "teams.yml",
		checkNamePrefix:      "teams / ",
		changeOutput:         "teams",
		changedEnv:           "TEAMS_CHANGED",
		qualityGateCondition: "needs.changes.outputs.teams == 'true'",
		detectChangesName:    "teams / detect changes",
		requiredName:         "teams / required",
		verifierStepName:     "Verify Teams CI result",
		unchangedOutput:      "No Teams-impacting changes detected",
	},
	{
		name:                 "cli",
		path:                 "cli.yml",
		checkNamePrefix:      "cli / ",
		changeOutput:         "cli",
		changedEnv:           "CLI_CHANGED",
		qualityGateCondition: "needs.changes.outputs.cli == 'true'",
		detectChangesName:    "cli / detect changes",
		requiredName:         "cli / required",
		verifierStepName:     "Verify CLI CI result",
		unchangedOutput:      "No CLI-impacting changes detected",
	},
	{
		name:                 "s3-static-connector",
		path:                 "s3-static-connector.yml",
		checkNamePrefix:      "s3-static-connector / ",
		changeOutput:         "s3_static_connector",
		changedEnv:           "CONNECTOR_CHANGED",
		qualityGateCondition: "needs.changes.outputs.s3_static_connector == 'true'",
		detectChangesName:    "s3-static-connector / detect changes",
		requiredName:         "s3-static-connector / required",
		verifierStepName:     "Verify connector CI result",
		unchangedOutput:      "No connector-impacting changes detected",
	},
	{
		name:                 "e2e",
		path:                 "e2e.yml",
		checkNamePrefix:      "e2e / ",
		changeOutput:         "e2e",
		changedEnv:           "E2E_CHANGED",
		qualityGateCondition: "needs.changes.outputs.e2e == 'true'",
		detectChangesName:    "e2e / detect changes",
		requiredName:         "e2e / required",
		verifierStepName:     "Verify e2e CI result",
		unchangedOutput:      "No e2e-impacting changes detected",
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
	On   any                  `yaml:"on"`
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

// TestWorkflowContractReportsOnEveryPullRequestAndMergeGroup pins the premise
// that makes these repo-wide tests useful. A paths filter or conditional job
// would put the check back behind the same green-when-broken hole this package
// exists to close: a workflow edit outside the filter could violate the
// contract without causing this check to report at all.
func TestWorkflowContractReportsOnEveryPullRequestAndMergeGroup(t *testing.T) {
	workflow := readWorkflow(t, "workflow-contract.yml")
	triggers := parseWorkflowTriggers(t, workflow.On)

	pullRequest, ok := triggers["pull_request"]
	if !ok {
		t.Fatal("workflow-contract.yml must run on pull_request")
	}
	if pullRequest != nil {
		config, ok := pullRequest.(map[string]any)
		if !ok {
			t.Fatalf("workflow-contract.yml pull_request trigger has unexpected type %T", pullRequest)
		}
		for _, filter := range []string{"paths", "paths-ignore"} {
			if _, ok := config[filter]; ok {
				t.Fatalf("workflow-contract.yml pull_request trigger must not define %s", filter)
			}
		}
	}
	if _, ok := triggers["merge_group"]; !ok {
		t.Fatal("workflow-contract.yml must run on merge_group")
	}

	contract, ok := workflow.Jobs["contract"]
	if !ok {
		t.Fatal("workflow-contract.yml is missing contract job")
	}
	if contract.Name != workflowContractCheckName {
		t.Fatalf("contract job name = %q, want %q", contract.Name, workflowContractCheckName)
	}
	if strings.TrimSpace(contract.If) != "" {
		t.Fatalf("contract job must be unconditional, got if = %q", contract.If)
	}
	if contract.Needs != nil {
		t.Fatalf("contract job must not depend on another job, got needs = %#v", contract.Needs)
	}

	for _, step := range contract.Steps {
		if step.Name != workflowContractTestName {
			continue
		}
		if run := strings.TrimSpace(step.Run); run != workflowContractTestRun {
			t.Fatalf("%s command = %q, want %q", workflowContractTestName, run, workflowContractTestRun)
		}
		return
	}
	t.Fatalf("contract job is missing %s step", workflowContractTestName)
}

func TestParseWorkflowTriggers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  []string
	}{
		{name: "scalar", value: "push", want: []string{"push"}},
		{name: "sequence", value: []any{"push", "pull_request"}, want: []string{"push", "pull_request"}},
		{name: "typed sequence", value: []string{"push", "merge_group"}, want: []string{"push", "merge_group"}},
		{name: "mapping", value: map[string]any{"pull_request": nil}, want: []string{"pull_request"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseWorkflowTriggers(t, test.value)
			if len(got) != len(test.want) {
				t.Fatalf("trigger count = %d, want %d", len(got), len(test.want))
			}
			for _, trigger := range test.want {
				if _, ok := got[trigger]; !ok {
					t.Errorf("missing trigger %q", trigger)
				}
			}
		})
	}
}

func parseWorkflowTriggers(t *testing.T, value any) map[string]any {
	t.Helper()

	switch typed := value.(type) {
	case string:
		return map[string]any{typed: nil}
	case []any:
		triggers := make(map[string]any, len(typed))
		for _, raw := range typed {
			trigger, ok := raw.(string)
			if !ok {
				t.Fatalf("workflow on sequence contains non-string value %T", raw)
			}
			triggers[trigger] = nil
		}
		return triggers
	case []string:
		triggers := make(map[string]any, len(typed))
		for _, trigger := range typed {
			triggers[trigger] = nil
		}
		return triggers
	case map[string]any:
		return typed
	default:
		t.Fatalf("workflow on has unexpected type %T", value)
		return nil
	}
}

// TestRequiredWorkflowSpecsCoverEveryAggregate keeps requiredWorkflowSpecs
// honest. The table above is maintained by hand, and nothing else notices when
// a workflow grows a required aggregate without a matching entry — the new
// aggregate then gets zero enforcement while looking fully covered. That is
// exactly how apps/teams shipped an aggregate-less workflow in #1001 and went
// unregistered until #1023.
func TestRequiredWorkflowSpecsCoverEveryAggregate(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	registered := make(map[string]bool, len(requiredWorkflowSpecs))
	for i := range requiredWorkflowSpecs {
		registered[requiredWorkflowSpecs[i].path] = true
	}

	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		if _, ok := readWorkflow(t, name).Jobs["required"]; !ok {
			continue
		}
		seen++
		if !registered[name] {
			t.Errorf("%s defines a required aggregate job but has no requiredWorkflowSpecs entry", name)
		}
	}

	// Guard against the scan silently matching nothing (renamed directory,
	// changed extension), which would make every assertion above vacuous.
	// This deliberately couples the two counts: a workflow that grows a job
	// keyed `required` must land its spec entry in the same change, or the
	// whole suite goes red rather than quietly under-enforcing the new
	// aggregate.
	if seen != len(requiredWorkflowSpecs) {
		t.Errorf("found %d workflows with a required aggregate, want %d (one per spec)", seen, len(requiredWorkflowSpecs))
	}
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
			// if: always() is the load-bearing line of this pattern. Without it the
			// aggregate inherits success() and is skipped whenever a gate is skipped
			// or fails. GitHub scores a skipped required check as satisfied, so a red
			// gate would stop blocking merges, and PRs that touch no app-impacting
			// path would never see the context report at all.
			if strings.TrimSpace(required.If) != "always()" {
				t.Fatalf("%s required.if = %q, want always()", spec.name, required.If)
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
			script := requiredVerifierScript(t, spec, workflow)

			qualityGates := requiredWorkflowQualityGates(t, spec, workflow)
			if len(qualityGates) == 0 {
				t.Fatalf("no %s quality gates found with if containing %q", spec.name, spec.qualityGateCondition)
			}

			for id := range qualityGates {
				if !strings.Contains(script, id+")") {
					t.Errorf("%s is missing a display_name case for %q", spec.verifierStepName, id)
				}
				// Matching only "<id>)" is satisfied by a comment that happens to
				// mention the job, so require the display name itself. Otherwise a
				// gate can reach needs with no working case arm and its failure
				// annotation renders the bare job id instead of "<app> / <gate>".
				if name := workflow.Jobs[id].Name; !strings.Contains(script, name) {
					t.Errorf("%s display_name case for %q omits display name %q", spec.verifierStepName, id, name)
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
			script := requiredVerifierScript(t, spec, workflow)
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

	// #nosec G304 -- callers pass checked-in workflow file names, either from
	// requiredWorkflowSpecs or from a ReadDir of .github/workflows itself.
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

func requiredVerifierScript(t *testing.T, spec *requiredWorkflowSpec, workflow githubWorkflow) string {
	t.Helper()

	required, ok := workflow.Jobs["required"]
	if !ok {
		t.Fatalf("%s workflow is missing required aggregate job", spec.name)
	}
	for _, step := range required.Steps {
		if step.Name != spec.verifierStepName {
			continue
		}
		if step.Shell != "bash" {
			t.Fatalf("%s shell = %q, want bash", spec.verifierStepName, step.Shell)
		}
		if strings.TrimSpace(step.Run) == "" {
			t.Fatalf("%s step has empty run script", spec.verifierStepName)
		}
		return step.Run
	}
	t.Fatalf("%s required job is missing %s step", spec.name, spec.verifierStepName)
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
