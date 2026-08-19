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
	"slices"
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

	releasePleaseWorkflow      = "release-please.yml"
	releasePleaseJobID         = "release-please"
	releasePleaseActionStepID  = "release"
	releasePleasePushCondition = "github.event_name == 'push'"
	cliReleaseVerifierStepName = "Verify the CLI release was created"
	cliReleaseVerifierScript   = "scripts/verify-cli-release.sh"
	checkoutActionPrefix       = "actions/checkout@"
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
	// pullRequestBranches is the intended `on.pull_request.branches` filter.
	// All nine carry "**" today, so a PR stacked on a feature branch runs the
	// same gates as one targeting main. It is read per spec rather than
	// assumed, so one that later earns a narrower filter records that decision
	// here; see TestAppWorkflowsRunOnStackedPRs.
	pullRequestBranches []string
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
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
		pullRequestBranches:  []string{"**"},
	},
}

type githubWorkflow struct {
	On   any                  `yaml:"on"`
	Jobs map[string]githubJob `yaml:"jobs"`
}

type githubJob struct {
	If          string `yaml:"if"`
	Name        string `yaml:"name"`
	Needs       any    `yaml:"needs"`
	Permissions any    `yaml:"permissions"`
	Steps       []step `yaml:"steps"`
	// Uses is set only on a job that calls a reusable workflow. Such a job
	// reports its checks as "<this job> / <inner job>" rather than under its
	// own name, which is why required_checks_test.go resolves it separately.
	Uses string `yaml:"uses"`
}

type step struct {
	ID    string `yaml:"id"`
	If    string `yaml:"if"`
	Name  string `yaml:"name"`
	Run   string `yaml:"run"`
	Shell string `yaml:"shell"`
	Uses  string `yaml:"uses"`
	// ContinueOnError accepts a bool or an expression, so it is read as `any`
	// and asserted absent rather than compared: either spelling would turn a
	// failing guard into a green one.
	ContinueOnError any `yaml:"continue-on-error"`
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

// TestReleasePleaseVerifiesTheCLIReleaseWasCreated pins the guard on the one
// release failure that reports success. release-please matches a merged release
// PR to a package before building that package's release; a PR body carrying a
// single componentless section takes the "standalone release PR" path, which
// compares the PR's *branch* component against getBranchComponent() — and
// getBranchComponent(), unlike getComponent(), ignores
// include-component-in-tag. The manifest release PR always sits on
// `release-please--branches--main`, whose branch component is undefined, and the
// bare-tagged CLI's section is componentless, so a `component` declared for
// apps/cli loses that comparison: the release is skipped and the action still
// exits 0. That dropped v1.1.0, v1.3.0 and v1.4.0 — a green run, no tag, no
// GitHub Release, and every component's next release PR blocked behind "There
// are untagged, merged release PRs outstanding".
//
// release-please-config.json no longer declares that component, and
// scripts/check-release-please-sync.sh keeps it that way. This pins the runtime
// half — the half that still reports if a drop ever returns by another route.
func TestReleasePleaseVerifiesTheCLIReleaseWasCreated(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, releasePleaseWorkflow)
	job, ok := workflow.Jobs[releasePleaseJobID]
	if !ok {
		t.Fatalf("%s is missing the %s job", releasePleaseWorkflow, releasePleaseJobID)
	}
	if !strings.Contains(job.If, releasePleasePushCondition) {
		t.Errorf("%s.if = %q, want it to contain %q — a recovery dispatch expects no new release and must not be verified as if it did",
			releasePleaseJobID, job.If, releasePleasePushCondition)
	}

	release, checkout, verify := -1, -1, -1
	for i := range job.Steps {
		current := &job.Steps[i]
		switch {
		case current.ID == releasePleaseActionStepID:
			release = i
		case checkout < 0 && strings.HasPrefix(current.Uses, checkoutActionPrefix):
			checkout = i
		case current.Name == cliReleaseVerifierStepName:
			verify = i
		}
	}

	if release < 0 {
		t.Fatalf("%s %s job has no step with id %q", releasePleaseWorkflow, releasePleaseJobID, releasePleaseActionStepID)
	}
	if verify < 0 {
		t.Fatalf("%s %s job is missing the %q step", releasePleaseWorkflow, releasePleaseJobID, cliReleaseVerifierStepName)
	}
	if checkout < 0 {
		t.Fatalf("%s %s job never checks out the repository, so %s is not on disk to run",
			releasePleaseWorkflow, releasePleaseJobID, cliReleaseVerifierScript)
	}

	// Order is the assertion, not decoration. Verifying before the action ran
	// would read the state the push arrived with and pass on exactly the push
	// that dropped a release.
	if release > verify {
		t.Errorf("%q is step %d, ahead of the release-please action at step %d — it would verify the state the push arrived with, not the state the action left",
			cliReleaseVerifierStepName, verify, release)
	}
	if checkout > verify {
		t.Errorf("the checkout is step %d, behind %q at step %d, so the script would not be on disk to run",
			checkout, cliReleaseVerifierStepName, verify)
	}

	verifyStep := &job.Steps[verify]
	if !strings.Contains(verifyStep.Run, cliReleaseVerifierScript) {
		t.Errorf("%q runs %q, want it to invoke %s", cliReleaseVerifierStepName, strings.TrimSpace(verifyStep.Run), cliReleaseVerifierScript)
	}
	// A conditional or continue-on-error guard reports green on the very run it
	// exists to redden, which is the silent pass this whole change removes.
	if condition := strings.TrimSpace(verifyStep.If); condition != "" {
		t.Errorf("%q is conditional (if = %q); the job's own push condition is the only gate it should carry",
			cliReleaseVerifierStepName, condition)
	}
	if verifyStep.ContinueOnError != nil {
		t.Errorf("%q sets continue-on-error = %#v, restoring the silent green it exists to remove",
			cliReleaseVerifierStepName, verifyStep.ContinueOnError)
	}

	assertExecutableRepoScript(t, cliReleaseVerifierScript)

	// A dropped release is a visibility problem, not a reason to widen the
	// token: reading a release needs no more than the contents access this job
	// already holds to create one.
	assertJobPermissions(t, releasePleaseJobID, job.Permissions, map[string]string{
		"contents":      "write",
		"pull-requests": "write",
	})
}

func assertExecutableRepoScript(t *testing.T, name string) {
	t.Helper()

	info, err := os.Stat(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is mode %v and not executable, but a workflow invokes it directly", name, info.Mode().Perm())
	}
}

func assertJobPermissions(t *testing.T, jobID string, permissions any, want map[string]string) {
	t.Helper()

	got, ok := permissions.(map[string]any)
	if !ok {
		t.Fatalf("%s.permissions has unexpected type %T, want a mapping", jobID, permissions)
	}

	extra, missing := []string{}, []string{}
	for scope, value := range got {
		wanted, documented := want[scope]
		if !documented {
			extra = append(extra, scope)
			continue
		}
		if value != wanted {
			t.Errorf("%s.permissions[%q] = %v, want %q", jobID, scope, value, wanted)
		}
	}
	for scope := range want {
		if _, ok := got[scope]; !ok {
			missing = append(missing, scope)
		}
	}

	slices.Sort(extra)
	slices.Sort(missing)
	if len(extra) > 0 {
		t.Errorf("%s.permissions grants %v beyond what this job is documented to need", jobID, extra)
	}
	if len(missing) > 0 {
		t.Errorf("%s.permissions is missing %v", jobID, missing)
	}
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

// pullRequestTriggers are the two events that run a workflow against a pull
// request and honor a base-branch filter. Both are in scope: what matters is
// not which event fires but that a gate the PR is judged on actually reports,
// and either one filtered to [main] goes missing on a stacked PR.
var pullRequestTriggers = []string{"pull_request", "pull_request_target"}

// pullRequestBranchSpec records the intended base-branch filter
// for a workflow that runs on pull requests but owns no required aggregate, and
// so has no requiredWorkflowSpecs entry.
type pullRequestBranchSpec struct {
	path string
	// branches is the intended filter. A nil value means the workflow must
	// declare no `branches:` key at all — the same reach as ["**"], arrived at
	// by omission rather than by a filter. The two are kept distinct so this
	// table pins the spelling each workflow actually uses.
	branches []string
	// producesRequiredContext records whether any job here reports a context
	// main's protection requires. It is what decides whether a narrow filter is
	// survivable, so it is verified against the tree by
	// TestPullRequestWorkflowsRecordWhetherTheyGateMerges rather than trusted.
	producesRequiredContext bool
	why                     string
}

// otherPullRequestWorkflows covers every remaining workflow with a
// pull-request trigger. Splitting the record in two is deliberate:
// requiredWorkflowSpecs already carries a completeness guard
// (TestRequiredWorkflowSpecsCoverEveryAggregate), and the workflows here are
// exactly the ones that guard cannot see.
var otherPullRequestWorkflows = []pullRequestBranchSpec{
	{
		path:     "codeql.yml",
		branches: []string{"main"},
		why: "Deliberately narrow. CodeQL produces no required context, so a stacked PR " +
			"that never runs it is not reading green over a gate it skipped — the honest-signal " +
			"argument that widened the app workflows does not apply. The code is still analyzed " +
			"before it reaches main: merging a base branch retargets the PRs stacked on it onto " +
			"main, and the analysis runs then. Against that second look sits a two-language " +
			"analysis matrix (30-minute timeout) on every stacked PR, and every PR run " +
			"re-anchors pre-existing alerts onto that PR, where they block merge until a human " +
			"resolves each conversation.",
	},
	{
		path:     "dependency-review.yml",
		branches: []string{"main"},
		why: "Deliberately narrow, on the same reasoning as codeql.yml: no required context, " +
			"and a stacked PR's dependency delta is reviewed again inside the combined diff once " +
			"it retargets to main. Cheaper to widen than CodeQL, so this is the entry to revisit " +
			"first — the cost is not runtime but noise, since `comment-summary-in-pr: always` " +
			"would post a summary onto every stacked PR.",
	},
	{
		path: "secrets-scan.yml",
		why:  "Already unfiltered, so it runs on every PR whatever its base. Recorded so that narrowing it to main fails here.",
	},
	{
		path: "scripts.yml",
		why:  "Already unfiltered. It gates the repo-wide scripts, including the extension lockstep and i18n parity checks, which a stacked PR can break exactly as a main-targeting one can.",
	},
	{
		path:                    "workflow-contract.yml",
		producesRequiredContext: true,
		why:                     "Already unfiltered, and must stay that way — it is what runs this package. A branches filter here would take the whole CI contract off stacked PRs, this test included.",
	},
	{
		path:                    "dependency-age-check-actions.yml",
		producesRequiredContext: true,
		why:                     "Already unfiltered, and produces a required context.",
	},
	{
		path:                    "dependency-age-check-docker.yml",
		producesRequiredContext: true,
		why:                     "Already unfiltered, and produces a required context.",
	},
	{
		path:                    "dependency-age-check-go.yml",
		producesRequiredContext: true,
		why:                     "Already unfiltered, and produces a required context.",
	},
	{
		path:                    "dependency-age-check-pip.yml",
		producesRequiredContext: true,
		why:                     "Already unfiltered, and produces a required context.",
	},
	{
		path: "pr-title.yml",
		why:  "Already unfiltered — it validates the PR title itself, which is worth checking on a stacked PR too.",
	},
	{
		path: "dependabot-pr-title.yml",
		why:  "Already unfiltered; same reasoning as pr-title.yml.",
	},
	{
		path: "validate-issue-templates.yml",
		why:  "Already unfiltered, and narrowed by `paths` rather than by base branch.",
	},
	{
		path:                    "claude-code-review.yml",
		producesRequiredContext: true,
		why: "Already unfiltered, and on `pull_request_target` rather than `pull_request` because it " +
			"holds ANTHROPIC_API_KEY and so must load its definition from the default branch. Its " +
			"`claude-review` context became required in #1185, which is what pulled that trigger into " +
			"scope here: narrowing it would leave every stacked PR waiting forever on a required check " +
			"that never registers, the failure this package already caught once on 2026-08-14.",
	},
}

// TestAppWorkflowsRunOnStackedPRs pins the `on.pull_request.branches` filter of
// each workflow that owns a required aggregate.
//
// A workflow filtered to `branches: [main]` does not merely skip a PR stacked on
// a feature branch — GitHub never registers it, so its checks are absent from
// the PR rather than reported as skipped. The PR then reads fully green having
// run none of them, and because branch protection guards only main, nothing
// stops it merging into its base on that showing.
//
// The fix landed one workflow at a time — slack.yml (#981), cli.yml (#1109),
// discord.yml (#1179) — and each time a one-line revert would have undone it
// with every check still green. This is what notices. It reads the intended
// value off each spec rather than asserting "**" across the board, so that a
// workflow which later earns a narrower filter records that decision here
// instead of being quietly blessed by a blanket assertion.
func TestAppWorkflowsRunOnStackedPRs(t *testing.T) {
	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			// An unset field would otherwise mean "declare no filter at all"
			// and fail further down with a message about the workflow rather
			// than about the missing entry.
			if len(spec.pullRequestBranches) == 0 {
				t.Fatalf("%s has no intended pull_request branches filter recorded", spec.path)
			}
			assertPullRequestBranches(t, spec.path, spec.pullRequestBranches)
		})
	}
}

// TestOtherPullRequestWorkflowsRecordTheirBranchFilter does the same for the
// workflows that own no aggregate. Two of them deliberately stay on main and
// the rest are already unfiltered; recording both kinds is what makes either a
// narrowing or an undocumented widening fail here rather than pass unnoticed.
func TestOtherPullRequestWorkflowsRecordTheirBranchFilter(t *testing.T) {
	for i := range otherPullRequestWorkflows {
		spec := &otherPullRequestWorkflows[i]
		t.Run(strings.TrimSuffix(spec.path, ".yml"), func(t *testing.T) {
			if strings.TrimSpace(spec.why) == "" {
				t.Fatalf("%s needs a why explaining its intended filter", spec.path)
			}
			assertPullRequestBranches(t, spec.path, spec.branches)
		})
	}
}

// TestEveryPullRequestWorkflowRecordsItsBranchFilter closes the hole the two
// tables above would otherwise leave: a workflow added later with
// `branches: [main]` and no entry would be silently unenforced, which is the
// same shape of gap that let an unregistered aggregate ship in #1081.
//
// Scope is both pull-request triggers. It was `pull_request` alone until
// #1185 made claude-code-review.yml's `claude-review` a required context —
// the exact condition this comment previously named as the one that would
// force the widening. GitHub honors `branches:` on `pull_request_target`
// identically, so that workflow narrowing to [main] would now leave every
// stacked PR waiting on a required check that never registers.
func TestEveryPullRequestWorkflowRecordsItsBranchFilter(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	recorded := make(map[string]bool, len(requiredWorkflowSpecs)+len(otherPullRequestWorkflows))
	for i := range requiredWorkflowSpecs {
		recorded[requiredWorkflowSpecs[i].path] = true
	}
	for i := range otherPullRequestWorkflows {
		path := otherPullRequestWorkflows[i].path
		if recorded[path] {
			t.Errorf("%s is recorded in both requiredWorkflowSpecs and otherPullRequestWorkflows", path)
		}
		recorded[path] = true
	}

	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		triggers := parseWorkflowTriggers(t, readWorkflow(t, name).On)
		runsOnPullRequests := false
		for _, trigger := range pullRequestTriggers {
			if _, ok := triggers[trigger]; ok {
				runsOnPullRequests = true
				break
			}
		}
		if !runsOnPullRequests {
			continue
		}
		seen++
		if !recorded[name] {
			t.Errorf("%s runs on a pull-request trigger but records no intended branches filter", name)
		}
	}

	// Couple the counts, so a scan that matches nothing (renamed directory,
	// changed extension) fails instead of passing every assertion vacuously,
	// and so an entry for a workflow that no longer runs on a pull-request
	// trigger is caught rather than left to rot.
	if want := len(recorded); seen != want {
		t.Errorf("found %d workflows running on a pull-request trigger, want %d (one per recorded entry)", seen, want)
	}
}

// TestPullRequestWorkflowsRecordWhetherTheyGateMerges checks each recorded
// producesRequiredContext against the tree, in both directions.
//
// The narrow entries' premise is already enforced below, but the converse was
// only prose: "produces a required context" was a claim no test read, free to
// rot. That is not hypothetical here — claude-code-review.yml's entry was
// written saying it produced none, and #1185 falsified it the same day.
//
// The check is exact for a job reporting under its own name and deliberately
// coarse for one reporting through a reusable call, where a context is
// "<caller job> / <inner job>" and the caller name is all that is visible
// here. The four dependency-age-check-*.yml workflows all name their caller
// `age-check`, so any one of the four `age-check / *` contexts marks all four
// as gating. It therefore catches a workflow gaining a required context, and a
// recorded claim that has become wholly false, but not the narrower case of
// one such workflow losing its own context while its siblings keep theirs.
// Pinning that needs the inner job names, which live in the called workflow —
// TestReusableCallerJobsCoverTheirDocumentedContexts is where that belongs.
func TestPullRequestWorkflowsRecordWhetherTheyGateMerges(t *testing.T) {
	reported := workflowReportedContexts(t)
	required := documentedRequiredContexts(t)

	for i := range otherPullRequestWorkflows {
		spec := &otherPullRequestWorkflows[i]
		t.Run(strings.TrimSuffix(spec.path, ".yml"), func(t *testing.T) {
			got := reportsRequiredContext(spec.path, reported, required)
			if got == spec.producesRequiredContext {
				return
			}
			if got {
				t.Errorf("%s reports a required context but is recorded as producing none; "+
					"a narrow filter would leave every stacked PR waiting on it", spec.path)
				return
			}
			t.Errorf("%s is recorded as producing a required context but reports none; "+
				"its why is now stale", spec.path)
		})
	}
}

// reportsRequiredContext reports whether any job in the workflow produces a
// context main's protection requires. Reusable-workflow calls report as
// "<caller job> / <inner job>", so a caller matches on the prefix.
func reportsRequiredContext(path string, reported workflowContexts, required []string) bool {
	for _, context := range required {
		if slices.Contains(reported.direct[context], path) {
			return true
		}
		if caller, _, ok := strings.Cut(context, contextSeparator); ok && slices.Contains(reported.reusable[caller], path) {
			return true
		}
	}
	return false
}

// TestNarrowPullRequestWorkflowsProduceNoRequiredContext enforces the premise
// the deliberately-narrow entries rest on, rather than leaving it to a comment
// nobody rereads.
//
// Keeping codeql.yml and dependency-review.yml on `[main]` is defensible only
// because neither produces a required context: a stacked PR that never runs
// them is not reading green over a gate that blocks its merge, which is the
// whole argument that widened the app workflows. Were either to become
// required, the reasoning would be void — an unregistered required check
// leaves the PR sitting on "Expected — Waiting for status to be reported"
// forever, the same silent shape as the 2026-08-14 typo. This turns that
// premise into something CI checks.
func TestNarrowPullRequestWorkflowsProduceNoRequiredContext(t *testing.T) {
	narrow := map[string]bool{}
	for i := range otherPullRequestWorkflows {
		spec := &otherPullRequestWorkflows[i]
		// A nil filter reaches every base branch already, and one naming "**"
		// is not narrow. Anything else keeps the workflow off stacked PRs.
		if spec.branches != nil && !slices.Contains(spec.branches, "**") {
			narrow[spec.path] = true
		}
	}
	if len(narrow) == 0 {
		t.Skip("no deliberately-narrow entries recorded, so there is no premise to enforce")
	}

	reported := workflowReportedContexts(t)
	for _, context := range documentedRequiredContexts(t) {
		for _, file := range reported.direct[context] {
			if narrow[file] {
				t.Errorf("%s is recorded as deliberately narrow but reports required context %q; "+
					"a required check that never registers leaves every stacked PR pending, so its entry needs revisiting", file, context)
			}
		}
		// Reusable-workflow calls report as "<caller job> / <inner job>".
		caller, _, ok := strings.Cut(context, contextSeparator)
		if !ok {
			continue
		}
		for _, file := range reported.reusable[caller] {
			if narrow[file] {
				t.Errorf("%s is recorded as deliberately narrow but its caller job reports required context %q; "+
					"a required check that never registers leaves every stacked PR pending, so its entry needs revisiting", file, context)
			}
		}
	}
}

// assertPullRequestBranches compares a workflow's declared pull-request
// branches filter against the intended one. A nil want means the workflow must
// declare no filter at all.
func assertPullRequestBranches(t *testing.T, path string, want []string) {
	t.Helper()

	triggers := parseWorkflowTriggers(t, readWorkflow(t, path).On)
	checked := 0
	for _, trigger := range pullRequestTriggers {
		config, ok := triggers[trigger]
		if !ok {
			continue
		}
		checked++

		got, declared := pullRequestBranchFilter(t, path, trigger, config)
		switch {
		case want == nil && declared:
			t.Errorf("%s %s declares branches %v, want no filter at all", path, trigger, got)
		case want == nil:
		case !declared:
			t.Errorf("%s %s declares no branches filter, want %v", path, trigger, want)
		case !slices.Equal(got, want):
			t.Errorf("%s %s.branches = %v, want %v", path, trigger, got, want)
		}
	}
	if checked == 0 {
		t.Fatalf("%s must run on one of %v", path, pullRequestTriggers)
	}
}

// pullRequestBranchFilter reads the `branches` filter off a parsed pull-request
// trigger, reporting whether one is declared at all. It accepts both YAML
// spellings of a single filter — a bare scalar and a sequence — so `main` and
// `[main]` are not treated as different decisions.
//
// The comparison this feeds is still order-sensitive, which matters only once a
// workflow earns a multi-element filter: ["main", "release/**"] and
// ["release/**", "main"] have identical reach but are not interchangeable here.
// That is deliberate — the table records the spelling a reader will find in the
// YAML — but it is stricter than reach alone, so record the order as written.
func pullRequestBranchFilter(t *testing.T, path, trigger string, pullRequest any) (branches []string, declared bool) {
	t.Helper()

	if pullRequest == nil {
		return nil, false
	}
	config, ok := pullRequest.(map[string]any)
	if !ok {
		t.Fatalf("%s %s trigger has unexpected type %T", path, trigger, pullRequest)
	}

	// `branches-ignore` is the one spelling these tables cannot express, and it
	// fails open rather than loudly: a workflow using it declares no `branches`
	// key, which reads below as full reach — while `branches-ignore:
	// ["justin/**"]` would take the workflow off exactly the stacked PRs this
	// suite exists to keep it on. Refuse it here instead, so adding one forces
	// the decision into the table. Checked before `branches` and independently
	// of it: GitHub rejects the two together, but this should not be the thing
	// that assumes so.
	if _, ok := config["branches-ignore"]; ok {
		t.Fatalf("%s %s declares branches-ignore, which these tables cannot record; "+
			"extend pullRequestBranchSpec to express it before using it", path, trigger)
	}

	raw, ok := config["branches"]
	if !ok {
		return nil, false
	}

	// No []string arm, unlike parseWorkflowTriggers and parseWorkflowNeeds: those
	// two also read hand-built literals from their own table tests, whereas this
	// value is always decoded by yaml.v3 into the `map[string]any` above, which
	// yields []any for every sequence. A []string arm here would be unreachable.
	switch typed := raw.(type) {
	case string:
		return []string{typed}, true
	case []any:
		for _, value := range typed {
			branch, ok := value.(string)
			if !ok {
				t.Fatalf("%s %s.branches contains non-string value %T", path, trigger, value)
			}
			branches = append(branches, branch)
		}
		return branches, true
	default:
		t.Fatalf("%s %s.branches has unexpected type %T", path, trigger, raw)
		return nil, false
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
		if !looksLikeRequiredWorkflowQualityGate(spec, &job, needs) {
			continue
		}
		if !slices.Contains(needs, "changes") {
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

func looksLikeRequiredWorkflowQualityGate(spec *requiredWorkflowSpec, job *githubJob, needs []string) bool {
	if !strings.HasPrefix(job.Name, spec.checkNamePrefix) {
		return false
	}
	if job.Name == spec.detectChangesName || job.Name == spec.requiredName {
		return false
	}
	return !slices.Contains(needs, "required")
}

func sortedQualityGateIDs(qualityGates map[string]bool) []string {
	ids := make([]string, 0, len(qualityGates))
	for id := range qualityGates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
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
