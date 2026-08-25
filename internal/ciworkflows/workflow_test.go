// Package ciworkflows holds repo-wide CI contract tests: assertions about the
// shape of every workflow in .github/workflows, not about any one app.
//
// It lives here rather than under apps/<app>/ because an app workflow's paths
// filter decides when that app's tests run, and these tests read every
// workflow file. Sitting in apps/slack, they inherited slack.yml's filter,
// which matches `.github/workflows/slack.yml` alone — so a PR adding a new
// workflow skipped them entirely and shipped an unregistered aggregate green
// (#1081). `.github/workflows/workflow-contract.yml` runs this package
// unfiltered on every PR instead.
package ciworkflows

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// The two job ids the app-workflow aggregate pattern is built on: a
	// `changes` detector, and a `required` aggregate that needs it and reports
	// the gating context. Spelled as constants only where an id is read
	// structurally — a key into a workflow's Jobs map, or an entry in a
	// `needs` list. The hand-written needs maps further down stay literal:
	// they are fixtures standing in for what a workflow author typed, so
	// renaming the real job must not silently rewrite them too.
	changesJobID  = "changes"
	requiredJobID = "required"

	releasePleaseWorkflow      = "release-please.yml"
	releasePleaseJobID         = "release-please"
	releasePleaseActionStepID  = "release"
	releasePleasePushCondition = "github.event_name == 'push'"
	cliReleaseVerifierStepName = "Verify the CLI release was created"
	cliReleaseVerifierScript   = "scripts/verify-cli-release.sh"
	checkoutActionPrefix       = "actions/checkout@"
	cliWorkflow                = "cli.yml"
	cliMatrixJobID             = "matrix"
	cliSandboxArtifactsJobID   = "sandbox-customer-artifacts"
	cliMacOSKeychainSetupStep  = "Set up macOS keychain"
	cliLinuxKeyringSetupStep   = "Set up Linux keyring (gnome-keyring over D-Bus)"
	cliMatrixTestStep          = "Run tests"

	workflowContractWorkflow = "workflow-contract.yml"
)

// TestCLISandboxCustomerArtifactsAreExactAndHermetic pins the bootstrap
// producer consumed by the later NHP customer-journey gate. This first stage
// builds and uploads only reviewed bytes. It has no token, dispatch, live
// sandbox, or deployment authority. Forks keep a visible failed gate instead
// of publishing executable input under the trusted repository identity.
func TestCLISandboxCustomerArtifactsAreExactAndHermetic(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, cliWorkflow)
	job, ok := workflow.Jobs[cliSandboxArtifactsJobID]
	if !ok {
		t.Fatalf("%s is missing %q", cliWorkflow, cliSandboxArtifactsJobID)
	}
	if job.Name != "cli / sandbox matched-cohort artifacts" ||
		job.If != "(github.event_name == 'push' && github.ref == 'refs/heads/main') || needs.changes.outputs.cli == 'true'" {
		t.Errorf("artifact job name/if = %q / %q", job.Name, job.If)
	}
	for _, fixture := range []struct {
		name, event, ref string
		cliChanged       bool
		want             bool
	}{
		{name: "non-CLI main push", event: "push", ref: "refs/heads/main", want: true},
		{name: "CLI pull request", event: "pull_request", ref: "refs/pull/1/merge", cliChanged: true, want: true},
		{name: "non-CLI pull request", event: "pull_request", ref: "refs/pull/1/merge", want: false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			got := (fixture.event == "push" && fixture.ref == "refs/heads/main") || fixture.cliChanged
			if got != fixture.want {
				t.Fatalf("artifact producer run decision = %t, want %t", got, fixture.want)
			}
		})
	}
	if timeout, ok := job.TimeoutMinutes.(int); !ok || timeout != 15 {
		t.Errorf("artifact job timeout = %#v, want 15", job.TimeoutMinutes)
	}
	assertJobPermissions(t, cliSandboxArtifactsJobID, job.Permissions, map[string]string{"contents": "read"})

	indices := map[string]int{}
	steps := map[string]*step{}
	for index := range job.Steps {
		current := &job.Steps[index]
		if current.Name != "" {
			indices[current.Name], steps[current.Name] = index, current
		}
	}
	requiredNames := []string{
		"Require the trusted artifact producer",
		"Build exact sandbox customer artifacts",
		"Upload exact sandbox customer binaries",
		"Upload exact sandbox customer source receipt",
	}
	for _, name := range requiredNames {
		if steps[name] == nil {
			t.Fatalf("artifact job is missing %q", name)
		}
	}

	guard := steps[requiredNames[0]]
	if indices[requiredNames[0]] != 0 || !strings.Contains(guard.Run, "pull_request|push") ||
		!strings.Contains(guard.Run, "layervai/qurl-integrations") || !strings.Contains(guard.Run, "does not accept a fork") {
		t.Errorf("trusted producer guard is not the first exact fail-closed step")
	}

	checkoutIndex := -1
	var checkout *step
	for index := range job.Steps {
		current := &job.Steps[index]
		if strings.HasPrefix(current.Uses, checkoutActionPrefix) {
			checkout, checkoutIndex = current, index
			break
		}
	}
	if checkout == nil || checkoutIndex <= 0 || checkout.With["persist-credentials"] != false ||
		checkout.With["ref"] != "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}" {
		t.Errorf("artifact checkout does not bind the exact caller head without credentials: %#v", checkout)
	}

	build := steps[requiredNames[1]]
	if !strings.Contains(build.Run, "scripts/build-sandbox-matched-cohort-pr-artifacts.sh") ||
		!strings.Contains(build.Run, `"$GITHUB_REPOSITORY" "$CALLER_HEAD_SHA" "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT"`) ||
		!strings.Contains(build.Run, `"$QURL_GO_SOURCE_SHA"`) {
		t.Errorf("artifact build does not bind the exact repository/head/run identity")
	}
	for _, expected := range []string{
		`scripts/resolve-sandbox-artifact-qurl-go-source.sh`,
	} {
		if !strings.Contains(build.Run, expected) {
			t.Errorf("artifact build is missing moving qurl-go source authority %q", expected)
		}
	}

	for label, current := range map[string]*step{
		"binaries": steps[requiredNames[2]],
		"receipt":  steps[requiredNames[3]],
	} {
		if current.Uses != "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" ||
			current.With["if-no-files-found"] != "error" || current.With["retention-days"] != 30 ||
			current.With["compression-level"] != 0 || current.With["include-hidden-files"] != false {
			t.Errorf("%s artifact upload is not exact: %#v", label, current.With)
		}
	}
	if steps[requiredNames[2]].With["name"] != "sandbox-matched-cohort-binaries" ||
		steps[requiredNames[3]].With["name"] != "sandbox-matched-cohort-source-receipt" {
		t.Errorf("customer artifact names are not exact")
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", cliWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	jobText, err := yaml.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"actions/create-github-app-token", "DEPLOY_DISPATCHER", "nhp-smoke-tests",
		"blue-green-deploy", "qurl-live-env-lock", "run-sandbox-matched-cohort-pr-journey",
	} {
		if strings.Contains(string(jobText), forbidden) {
			t.Errorf("artifact producer retains forbidden live authority %q", forbidden)
		}
	}
	if !strings.Contains(string(raw), "scripts/build-sandbox-matched-cohort-pr-artifacts_test.sh") {
		t.Errorf("CLI hermetic test job does not execute the artifact contract test")
	}
	for _, script := range []string{
		"scripts/build-sandbox-matched-cohort-pr-artifacts.sh",
		"scripts/build-sandbox-matched-cohort-pr-artifacts_test.sh",
		"scripts/resolve-sandbox-artifact-qurl-go-source.sh",
		"scripts/resolve-sandbox-artifact-qurl-go-source_test.sh",
	} {
		assertExecutableRepoScript(t, script)
	}
}

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
	//
	// Recording a narrower filter here does not by itself authorize one. The
	// aggregate each spec names reports a context CONTRIBUTING.md documents as
	// required, and TestNarrowPullRequestWorkflowsProduceNoRequiredContext
	// weighs a narrow filter against that block rather than against this
	// field. Unsetting the field is no way around it either:
	// TestAppWorkflowsRunOnStackedPRs fails an unrecorded intent by name.
	pullRequestBranches []string
	// pullRequestTypes and pullRequestPaths are the intended
	// `on.pull_request.types` and `on.pull_request.paths` filters, the two
	// sibling keys that also decide whether the workflow starts at all. All
	// nine leave both unset: they narrow by diff with `dorny/paths-filter`
	// inside the `changes` job, which keeps the workflow starting on every pull
	// request so the `if: always()` aggregate still reports. Lifting that
	// narrowing up to the trigger reads like the same intent spelled more
	// cheaply and is not — see
	// TestPullRequestWorkflowsRecordTheirTypeAndPathFilters, and
	// TestNarrowTypeAndPathFiltersProduceNoRequiredContext for the same
	// weighing against CONTRIBUTING.md the paragraph above describes.
	pullRequestTypes []string
	pullRequestPaths []string
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

// githubWorkflow, githubJob and step model the workflow keys this package
// asserts on.
//
// githubJob and step have both reached gocritic's rangeValCopy threshold — 128
// and 136 bytes against a >= 128 default — so range over them by index and take
// a pointer, `for i := range job.Steps { current := &job.Steps[i] }`, and reach
// a job through the *githubJob that Jobs holds: a map value cannot be
// addressed, so a value map would force exactly the copy that finding names.
// gocritic's skipTestFuncs default spares the loops inside Test functions,
// which silences the finding and not the copy, so those are written the same
// way rather than left as the exception. There is no headroom left in either
// struct to spend, and none to reclaim by reordering: every field is
// pointer-aligned and pointer-sized, so neither carries any padding.
type githubWorkflow struct {
	On          any                   `yaml:"on"`
	Permissions any                   `yaml:"permissions"`
	Jobs        map[string]*githubJob `yaml:"jobs"`
}

type githubJob struct {
	If          string `yaml:"if"`
	Name        string `yaml:"name"`
	Needs       any    `yaml:"needs"`
	Permissions any    `yaml:"permissions"`
	// Env carries the job-level environment. Values are strings and numbers,
	// so it is read as `any` per key and asserted only where one is
	// load-bearing — the Claude review's minute budget.
	Env map[string]any `yaml:"env"`
	// TimeoutMinutes is the job's own cap. See step.TimeoutMinutes for why
	// both are `any` rather than int.
	TimeoutMinutes any    `yaml:"timeout-minutes"`
	Steps          []step `yaml:"steps"`
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
	// With carries an action step's inputs. Values are strings, bools and
	// numbers, so it is read as `any` per key and asserted only where an input
	// is load-bearing — a checkout's ref and credential persistence, a review's
	// tool deny-list.
	With map[string]any `yaml:"with"`
	// ContinueOnError accepts a bool or an expression, so it is read as `any`
	// and asserted absent rather than compared: either spelling would turn a
	// failing guard into a green one.
	ContinueOnError any `yaml:"continue-on-error"`
	// TimeoutMinutes accepts an integer literal or a `${{ }}` expression, so it
	// is read as `any` and type-asserted at the call site. Typing it int would
	// make an expression-valued cap anywhere in .github/workflows fail to parse
	// in readWorkflow — taking down every test in this package rather than the
	// one assertion that cares — and a caller that wants a literal wants a bare
	// number to report as the *wrong* value, not as a missing one. Network
	// bootstrap assertions also use it to distinguish a bounded setup failure
	// from the broader job timeout.
	TimeoutMinutes any `yaml:"timeout-minutes"`
}

// TestCLIMacOSKeychainSetupIsLive prevents the matrix from inferring whether
// credential-store coverage matters from source text. QURL_TEST_HARNESS arms
// TestKeyringLiveSmoke on every OS, so macOS must always unlock the ephemeral
// keychain before the suite starts.
func TestCLIMacOSKeychainSetupIsLive(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, cliWorkflow)
	job, ok := workflow.Jobs[cliMatrixJobID]
	if !ok {
		t.Fatalf("%s is missing the %q job", cliWorkflow, cliMatrixJobID)
	}
	var setup *step
	for i := range job.Steps {
		if job.Steps[i].Name == cliMacOSKeychainSetupStep {
			setup = &job.Steps[i]
			break
		}
	}
	if setup == nil {
		t.Fatalf("%s is missing %q", cliMatrixJobID, cliMacOSKeychainSetupStep)
	}
	if setup.If != "runner.os == 'macOS'" {
		t.Errorf("%s if = %q, want the macOS runner guard", cliMacOSKeychainSetupStep, setup.If)
	}
	if setup.Shell != "bash" {
		t.Errorf("%s shell = %q, want bash", cliMacOSKeychainSetupStep, setup.Shell)
	}
	if setup.ContinueOnError != nil {
		t.Errorf("%s sets continue-on-error = %v; failed keychain setup must fail the macOS matrix leg", cliMacOSKeychainSetupStep, setup.ContinueOnError)
	}
	for _, fragment := range []string{"uuidgen", "security create-keychain", "security default-keychain", "security unlock-keychain"} {
		if !strings.Contains(setup.Run, fragment) {
			t.Errorf("%s is missing load-bearing fragment %q", cliMacOSKeychainSetupStep, fragment)
		}
	}
	if strings.Contains(setup.Run, "grep -riq keyring") {
		t.Errorf("%s infers whether live coverage matters from source text", cliMacOSKeychainSetupStep)
	}
}

// TestCLILinuxKeyringSetupIsBoundedAndLive protects the Linux half of the
// cross-platform credential-store smoke. Two independent ubuntu-latest runs
// exhausted the entire 25-minute matrix budget inside an unbounded `apt-get
// update` while the configured Azure mirror stopped delivering package
// indexes. That skipped the tests altogether and held every CLI change behind
// an infrastructure hang.
//
// The bootstrap may reuse binaries already on the hosted image or install
// them, but it may never skip the live store test. Its package operation is
// noninteractive, password-prompt-free, transport/lock/whole-process bounded,
// retried finitely through an explicit non-Azure fallback, and followed by
// binary verification before the D-Bus test path is armed.
func TestCLILinuxKeyringSetupIsBoundedAndLive(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, cliWorkflow)
	job, ok := workflow.Jobs[cliMatrixJobID]
	if !ok {
		t.Fatalf("%s is missing the %q job", cliWorkflow, cliMatrixJobID)
	}
	if got := job.Env["QURL_TEST_HARNESS"]; got != "1" {
		t.Errorf("%s env QURL_TEST_HARNESS = %q, want 1 so TestKeyringLiveSmoke runs", cliMatrixJobID, got)
	}

	var setup, tests *step
	for i := range job.Steps {
		candidate := &job.Steps[i]
		switch candidate.Name {
		case cliLinuxKeyringSetupStep:
			setup = candidate
		case cliMatrixTestStep:
			tests = candidate
		}
	}
	if setup == nil {
		t.Fatalf("%s is missing %q", cliMatrixJobID, cliLinuxKeyringSetupStep)
	}
	if tests == nil {
		t.Fatalf("%s is missing %q", cliMatrixJobID, cliMatrixTestStep)
	}

	if setup.If != "runner.os == 'Linux'" {
		t.Errorf("%s if = %q, want the Linux runner guard", cliLinuxKeyringSetupStep, setup.If)
	}
	if setup.Shell != "bash" {
		t.Errorf("%s shell = %q, want bash", cliLinuxKeyringSetupStep, setup.Shell)
	}
	timeoutMinutes, ok := setup.TimeoutMinutes.(int)
	if !ok || timeoutMinutes <= 0 || timeoutMinutes > 5 {
		t.Errorf("%s timeout-minutes = %#v, want a literal positive integer no greater than 5", cliLinuxKeyringSetupStep, setup.TimeoutMinutes)
	}
	if setup.ContinueOnError != nil {
		t.Errorf("%s sets continue-on-error = %v; failed keyring setup must fail the Linux matrix leg", cliLinuxKeyringSetupStep, setup.ContinueOnError)
	}

	requiredSetupFragments := []string{
		"TODO(upstream-contract)",
		"for attempt in 1 2",
		"sudo -n env DEBIAN_FRONTEND=noninteractive",
		"sudo -n tee /etc/apt/apt-mirrors.txt",
		`printf '%s\tpriority:%s\n'`,
		"https://archive.ubuntu.com/ubuntu/",
		"https://security.ubuntu.com/ubuntu/",
		"timeout --signal=TERM --kill-after=10s 90s",
		"Acquire::Retries=2",
		"Acquire::http::Timeout=20",
		"Acquire::https::Timeout=20",
		"DPkg::Lock::Timeout=30",
		"install -y --no-install-recommends gnome-keyring dbus",
		"command -v gnome-keyring-daemon",
		"command -v dbus-run-session",
		"QURL_CLI_KEYRING_DBUS=1",
	}
	for _, fragment := range requiredSetupFragments {
		if !strings.Contains(setup.Run, fragment) {
			t.Errorf("%s is missing load-bearing fragment %q", cliLinuxKeyringSetupStep, fragment)
		}
	}
	for _, forbidden := range []string{"apt-get update", "grep -riq keyring", "continue-on-error"} {
		if strings.Contains(setup.Run, forbidden) {
			t.Errorf("%s contains %q; setup must not refresh indexes, infer whether coverage matters, or swallow failures", cliLinuxKeyringSetupStep, forbidden)
		}
	}

	for _, fragment := range []string{"QURL_CLI_KEYRING_DBUS", "dbus-run-session", "gnome-keyring-daemon --unlock", "go test -count=1 ./apps/cli/..."} {
		if !strings.Contains(tests.Run, fragment) {
			t.Errorf("%s is missing %q; Linux must exercise the real keyring rather than skip the suite", cliMatrixTestStep, fragment)
		}
	}
}

// TestCLILinuxKeyringSetupRetryBehavior executes the checked-in workflow
// script with a fake sudo boundary. The structural test above pins the apt
// flags; this one proves the control flow they sit inside: a transient failure
// selects the official mirror fallback before one retry, fallback and repeated
// install failures are loud and finite, and a pre-provisioned runner still
// arms the live test without invoking apt.
func TestCLILinuxKeyringSetupRetryBehavior(t *testing.T) {
	workflow := readWorkflow(t, cliWorkflow)
	job := workflow.Jobs[cliMatrixJobID]
	var script string
	for i := range job.Steps {
		candidate := &job.Steps[i]
		if candidate.Name == cliLinuxKeyringSetupStep {
			script = candidate.Run
			break
		}
	}
	if strings.TrimSpace(script) == "" {
		t.Fatalf("%s has no executable script", cliLinuxKeyringSetupStep)
	}

	tests := []struct {
		name             string
		mode             string
		preinstalled     bool
		wantSuccess      bool
		wantInstallCalls int
		wantMirrorCalls  int
		wantErrorMessage string
	}{
		{name: "healthy primary mirror installs without fallback", mode: "succeed-first", wantSuccess: true, wantInstallCalls: 1},
		{name: "slow primary mirror falls back then arms live test", mode: "fail-once", wantSuccess: true, wantInstallCalls: 2, wantMirrorCalls: 1},
		{name: "fallback selection failure fails loudly", mode: "fallback-fail", wantInstallCalls: 1, wantMirrorCalls: 1, wantErrorMessage: "::error::Unable to select the bounded Ubuntu package mirror fallback"},
		{name: "repeated install failure fails loudly after bound", mode: "always-fail", wantInstallCalls: 2, wantMirrorCalls: 1, wantErrorMessage: "::error::Linux keyring package install failed after 2 bounded attempts"},
		{name: "preinstalled binaries avoid apt and still arm live test", mode: "unexpected", preinstalled: true, wantSuccess: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.Mkdir(binDir, 0o700); err != nil {
				t.Fatalf("create fake bin: %v", err)
			}
			writeExecutable := func(name, body string) {
				t.Helper()
				path := filepath.Join(binDir, name)
				// #nosec G306 -- this test fixture must be executable and lives beneath t.TempDir.
				if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
					t.Fatalf("write fake %s: %v", name, err)
				}
			}
			writeExecutable("dbus-run-session", "#!/usr/bin/env bash\nexit 0\n")
			writeExecutable("sleep", "#!/usr/bin/env bash\nexit 0\n")
			if tc.preinstalled {
				writeExecutable("gnome-keyring-daemon", "#!/usr/bin/env bash\nexit 0\n")
			}

			sudoLog := filepath.Join(dir, "sudo.log")
			mirrorContent := filepath.Join(dir, "mirror-content")
			writeExecutable("sudo", `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_SUDO_LOG"
if [[ "$*" == *"tee /etc/apt/apt-mirrors.txt"* ]]; then
  if [ "$FAKE_SUDO_MODE" = "fallback-fail" ]; then
    exit 43
  fi
  cat > "$FAKE_MIRROR_CONTENT"
  exit 0
fi
case "$FAKE_SUDO_MODE" in
  succeed-first)
    printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/gnome-keyring-daemon"
    chmod 700 "$FAKE_BIN/gnome-keyring-daemon"
    ;;
  fail-once)
    if [ "$(grep -c apt-get "$FAKE_SUDO_LOG")" -lt 2 ]; then
      exit 42
    fi
    if ! grep -q archive.ubuntu.com "$FAKE_MIRROR_CONTENT" ||
      ! grep -q security.ubuntu.com "$FAKE_MIRROR_CONTENT" ||
      grep -q azure.archive.ubuntu.com "$FAKE_MIRROR_CONTENT"; then
      echo "second install ran before selecting only the official mirrors" >&2
      exit 44
    fi
    printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/gnome-keyring-daemon"
    chmod 700 "$FAKE_BIN/gnome-keyring-daemon"
    ;;
  always-fail)
    exit 42
    ;;
  fallback-fail)
    exit 42
    ;;
  unexpected)
    echo "sudo was called even though both keyring commands were preinstalled" >&2
    exit 99
    ;;
  *)
    echo "unknown fake sudo mode" >&2
    exit 98
    ;;
esac
`)

			githubEnv := filepath.Join(dir, "github_env")
			if err := os.WriteFile(githubEnv, nil, 0o600); err != nil {
				t.Fatalf("create GITHUB_ENV: %v", err)
			}
			combined, err := runVerifierScriptWithEnv(t, script, map[string]string{
				"FAKE_BIN":            binDir,
				"FAKE_MIRROR_CONTENT": mirrorContent,
				"FAKE_SUDO_LOG":       sudoLog,
				"FAKE_SUDO_MODE":      tc.mode,
				"GITHUB_ENV":          githubEnv,
				"HOME":                filepath.Join(dir, "home"),
				"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			})
			if tc.wantSuccess && err != nil {
				t.Fatalf("setup failed: %v\noutput:\n%s", err, combined)
			}
			if !tc.wantSuccess && err == nil {
				t.Fatalf("setup succeeded, want bounded failure\noutput:\n%s", combined)
			}

			var sudoCalls []string
			// #nosec G304 -- sudoLog is a fixed filename beneath t.TempDir.
			if data, readErr := os.ReadFile(sudoLog); readErr == nil {
				sudoCalls = strings.Split(strings.TrimSpace(string(data)), "\n")
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("read fake sudo log: %v", readErr)
			}
			installCalls, mirrorCalls := 0, 0
			for _, call := range sudoCalls {
				if strings.Contains(call, "apt-get") {
					installCalls++
				}
				if strings.Contains(call, "tee /etc/apt/apt-mirrors.txt") {
					mirrorCalls++
				}
			}
			if installCalls != tc.wantInstallCalls {
				t.Errorf("apt install calls = %d, want %d\noutput:\n%s", installCalls, tc.wantInstallCalls, combined)
			}
			if mirrorCalls != tc.wantMirrorCalls {
				t.Errorf("mirror fallback calls = %d, want %d\noutput:\n%s", mirrorCalls, tc.wantMirrorCalls, combined)
			}
			if tc.wantMirrorCalls > 0 && tc.mode != "fallback-fail" {
				// #nosec G304 -- mirrorContent is a fixed filename beneath t.TempDir.
				data, readErr := os.ReadFile(mirrorContent)
				if readErr != nil {
					t.Fatalf("read fallback mirror content: %v", readErr)
				}
				content := string(data)
				for _, required := range []string{
					"https://archive.ubuntu.com/ubuntu/\tpriority:1",
					"https://security.ubuntu.com/ubuntu/\tpriority:2",
				} {
					if !strings.Contains(content, required) {
						t.Errorf("fallback mirror content is missing %q: %q", required, content)
					}
				}
				if strings.Contains(content, "azure.archive.ubuntu.com") {
					t.Errorf("fallback mirror content still selects Azure: %q", content)
				}
			}

			// #nosec G304 -- githubEnv is a fixed filename beneath t.TempDir.
			envData, readErr := os.ReadFile(githubEnv)
			if readErr != nil {
				t.Fatalf("read GITHUB_ENV: %v", readErr)
			}
			armed := strings.Contains(string(envData), "QURL_CLI_KEYRING_DBUS=1")
			if armed != tc.wantSuccess {
				t.Errorf("live keyring test armed = %t, want %t; GITHUB_ENV = %q", armed, tc.wantSuccess, envData)
			}
			if tc.wantErrorMessage != "" && !strings.Contains(combined, tc.wantErrorMessage) {
				t.Errorf("bounded failure omitted %q\noutput:\n%s", tc.wantErrorMessage, combined)
			}
		})
	}
}

// TestWorkflowContractReportsOnEveryPullRequest pins the premise that makes
// these repo-wide tests useful. A paths filter or conditional job would put the
// check back behind the same green-when-broken hole this package exists to
// close: a workflow edit outside the filter could violate the contract without
// causing this check to report at all.
//
// Whether this workflow also runs on merge_group is not asserted here.
// TestMergeGroupTriggersAgreeAcrossRequiredContexts owns that, because the
// answer has to be the same for every required context rather than for this
// one workflow.
func TestWorkflowContractReportsOnEveryPullRequest(t *testing.T) {
	workflow := readWorkflow(t, workflowContractWorkflow)
	triggers := parseWorkflowTriggers(t, workflowContractWorkflow, workflow.On)

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

	for i := range contract.Steps {
		current := &contract.Steps[i]
		if current.Name != workflowContractTestName {
			continue
		}
		if run := strings.TrimSpace(current.Run); run != workflowContractTestRun {
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
			got := parseWorkflowTriggers(t, "example.yml", test.value)
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

func parseWorkflowTriggers(t *testing.T, workflow string, value any) map[string]any {
	t.Helper()

	switch typed := value.(type) {
	case string:
		return map[string]any{typed: nil}
	case []any:
		triggers := make(map[string]any, len(typed))
		for _, raw := range typed {
			trigger, ok := raw.(string)
			if !ok {
				t.Fatalf("%s on sequence contains non-string value %T", workflow, raw)
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
	case nil:
		// A bare `on:` with no value unmarshals to nil. Named separately from
		// the default below because it is the one malformed shape a human
		// actually writes, and "unexpected type <nil>" describes it poorly.
		t.Fatalf("%s has an empty `on:`, so nothing can ever run it", workflow)
		return nil
	default:
		t.Fatalf("%s on has unexpected type %T", workflow, value)
		return nil
	}
}

// pullRequestTriggers are the two events that run a workflow against a pull
// request and honor a base-branch filter. Both are in scope: what matters is
// not which event fires but that a gate the PR is judged on actually reports,
// and either one filtered to [main] goes missing on a stacked PR.
var pullRequestTriggers = []string{"pull_request", "pull_request_target"}

// pullRequestFilterKey names one of the `on.pull_request.*` keys these tables
// record. The three are siblings rather than variations: each decides, on its
// own axis, whether the workflow starts at all for a given pull request — and a
// workflow that never starts reports nothing, which on the PR page is
// indistinguishable from a check that has not finished yet.
type pullRequestFilterKey struct {
	name string
	// ignoreName is the inverted spelling GitHub accepts in place of this key,
	// or "" for a key that has none. `types` is the only one of the three with
	// no inverted form, so it is the only one nothing has to be refused for.
	ignoreName string
}

var (
	branchesFilterKey = pullRequestFilterKey{name: "branches", ignoreName: "branches-ignore"}
	typesFilterKey    = pullRequestFilterKey{name: "types"}
	pathsFilterKey    = pullRequestFilterKey{name: "paths", ignoreName: "paths-ignore"}
)

// pullRequestTriggerSpec records the intended `on.pull_request` filters for a
// workflow that runs on pull requests but owns no required aggregate, and so
// has no requiredWorkflowSpecs entry.
type pullRequestTriggerSpec struct {
	path string
	// branches, types and paths are the intended filters. A nil value means the
	// workflow must declare no key of that name at all — for `branches` the
	// same reach as ["**"], and for `types` the same reach as
	// defaultPullRequestTypes — arrived at by omission rather than by a filter.
	// Omission and an equivalent explicit filter are kept distinct so this
	// table pins the spelling each workflow actually uses.
	branches []string
	types    []string
	paths    []string
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
var otherPullRequestWorkflows = []pullRequestTriggerSpec{
	{
		path:     "codeql.yml",
		branches: []string{"main"},
		why: "Deliberately narrow. CodeQL produces no required context, so a stacked PR " +
			"that never runs it is not reading green over a gate it skipped — the honest-signal " +
			"argument that widened the app workflows does not apply. The code is still analyzed " +
			"before it reaches main: deleting a merged base branch retargets the PRs stacked on it " +
			"onto main, and the analysis runs on the next push rather than on the retarget itself " +
			"(TestBranchFilteredWorkflowsExcludeEditedActivityType) — and strict status checks " +
			"require that push before the merge anyway. Against that second look sits a " +
			"two-language analysis matrix (30-minute timeout) on every stacked PR, and every PR run " +
			"re-anchors pre-existing alerts onto that PR, where they block merge until a human " +
			"resolves each conversation.",
	},
	{
		path:     "dependency-review.yml",
		branches: []string{"main"},
		why: "Deliberately narrow, on the same reasoning as codeql.yml: no required context, " +
			"and a stacked PR's dependency delta is reviewed again inside the combined diff once " +
			"the merged base is deleted and it retargets to main. Cheaper to widen than CodeQL, " +
			"so this is the entry to revisit first — the cost is not runtime but noise, since " +
			"`comment-summary-in-pr: always` would post a summary onto every stacked PR.",
	},
	{
		path: "secrets-scan.yml",
		why:  "Already unfiltered, so it runs on every PR whatever its base. Recorded so that narrowing it to main fails here.",
	},
	{
		path:                    "scripts.yml",
		producesRequiredContext: true,
		why: "Already unfiltered, and produces a required context. It gates the repo-wide scripts, including " +
			"the extension lockstep and i18n parity checks, which a stacked PR can break exactly as a " +
			"main-targeting one can — and blocks the merge rather than annotating it. Its single job " +
			"sets no `if:`, so it cannot report `skipped`; the step-level guards there decide which steps " +
			"run, never whether the job reports.",
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
		path:  "pr-title.yml",
		types: []string{"opened", "edited", "synchronize", "reopened"},
		why: "Already unfiltered by branch — it validates the PR title itself, which is worth checking on a " +
			"stacked PR too. Its `types` adds `edited` to the default three, because the title is what it reads " +
			"and retitling fires nothing else; adding to the default set widens reach rather than narrowing it.",
	},
	{
		path:  "dependabot-pr-title.yml",
		types: []string{"opened", "edited"},
		why: "Already unfiltered by branch; same reasoning as pr-title.yml. Its `types` does narrow — no " +
			"`synchronize`, so a push to a Dependabot branch does not revalidate — which is survivable only " +
			"because it produces no required context, the premise " +
			"TestNarrowTypeAndPathFiltersProduceNoRequiredContext holds it to.",
	},
	{
		path:  "validate-issue-templates.yml",
		paths: []string{".github/ISSUE_TEMPLATE/**", ".github/workflows/validate-issue-templates.yml"},
		why: "Already unfiltered by branch, and narrowed by trigger-level `paths` rather than by base branch. " +
			"That is survivable only while it produces no required context: a required check behind a " +
			"trigger-level `paths` never registers for a PR the filter misses, which is the failure the nine " +
			"aggregates avoid by narrowing inside the `changes` job instead.",
	},
	{
		path:                    "claude-code-review.yml",
		types:                   []string{"opened", "synchronize", "reopened", "ready_for_review"},
		producesRequiredContext: true,
		why: "Already unfiltered, and on `pull_request_target` rather than `pull_request` because it " +
			"holds ANTHROPIC_API_KEY and so must load its definition from the default branch. Its " +
			"`claude-review` context became required in #1185, which is what pulled that trigger into " +
			"scope here: narrowing it would take a merge-gating check off every stacked PR, which would " +
			"then read green having skipped it — see TestAppWorkflowsRunOnStackedPRs. Its `types` reaches " +
			"the pending sibling on PRs targeting main, so the list is pinned exactly: it carries the " +
			"full default three plus `ready_for_review`, and dropping any of the three would deregister " +
			"`claude-review`. Pinning it exactly also holds the other end, which the premise test cannot — " +
			"`converted_to_draft` must stay absent, or converting a reviewed PR to draft retriggers the job " +
			"and lets the exemption pass replace a completed review on the same head SHA.",
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
// Absent is not pending, and the two are easy to swap. Pending — "Expected —
// Waiting for status to be reported", the 2026-08-14 shape — needs main's
// protection to be judging the PR at all, so it is what a required context
// nothing reports does to a PR targeting main, which then blocks until an admin
// override lands it. Nothing protects the base of a stacked PR, so there is no
// required context there to wait on. Both failures are silent, and while the PR
// is stacked it is this one that merges. Deleting the merged base retargets the
// PR onto main and puts it under main's protection, which is exactly when the
// other shape appears — the second half, pinned below by
// TestBranchFilteredWorkflowsExcludeEditedActivityType.
//
// CONTRIBUTING.md's required-contexts section is the wording to match, and the
// entries and messages elsewhere in this file defer to this paragraph rather
// than restating the mechanism (#1194).
//
// The fix landed one workflow at a time — slack.yml (#981), cli.yml (#1109),
// discord.yml (#1179) — and each time a one-line revert would have undone it
// with every check still green. This is what notices. It reads the intended
// value off each spec rather than asserting "**" across the board, so that a
// workflow which later earns a narrower filter records that decision here
// instead of being quietly blessed by a blanket assertion.
//
// That makes this test alone insufficient, since a commit narrowing the
// workflow can edit the spec beside it in the same breath and satisfy the
// comparison. TestNarrowPullRequestWorkflowsProduceNoRequiredContext is the
// half that cannot be edited into agreement: it weighs the recorded filter
// against the contexts CONTRIBUTING.md documents as required.
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
			assertPullRequestFilter(t, spec.path, branchesFilterKey, spec.pullRequestBranches)
		})
	}
}

// editedActivityType is the `pull_request` activity type GitHub sends when a
// PR's base branch changes, which is what retargeting a stacked PR onto main
// does. It is not among the three types a workflow gets when it declares no
// `types:` of its own — opened, synchronize, reopened — so a workflow taking
// the defaults never sees one.
//
// TODO(upstream-contract): this relies on GitHub continuing to emit `edited`
// when deleting a base branch retargets its open PRs. The test below pins only
// this repository's subscription half; it cannot detect drift in GitHub's event
// contract.
const editedActivityType = "edited"

// TestBranchFilteredWorkflowsExcludeEditedActivityType pins the tree fact that
// makes the second half of the stacked-PR stall real.
//
// TestAppWorkflowsRunOnStackedPRs above covers the first half: a workflow
// filtered to `branches: [main]` is never registered on a PR stacked on a
// feature branch, so its checks are absent rather than skipped and the PR reads
// green having run none of them. Merging and then deleting that base looks like
// the backstop, and is not. The deletion retargets the stacked PR onto main,
// where the required contexts do apply — but the retarget arrives as activity
// type `edited`, which the defaults exclude, so it re-runs nothing. The check
// that never registered finally has a merge box to hold, and the PR sits at
// "Expected — Waiting for status to be reported" until the next push.
//
// That second phase holds only while no branch-filtered workflow asks for
// `edited`. One that did would re-run on the retarget and report, and the stall
// would stop happening — leaving the account above describing a repo that no
// longer exists. It is a claim about the tree, so it belongs in a test rather
// than in prose: "produces a required context" sat in a comment nobody reread
// until #1185 falsified it the same day it was written, which is what
// TestPullRequestWorkflowsRecordWhetherTheyGateMerges now exists to prevent.
//
// Two workflows declare `edited` today — pr-title.yml and
// dependabot-pr-title.yml — and neither carries a `branches:` filter, so
// neither is in scope. The second guard below pins that exact set: if the scan
// stops reading `types:` or this account changes, the test fails rather than
// leaving stale prose behind. issue-priority.yml declares it on an `issues:`
// trigger, which is a different event's activity type entirely.
//
// Scope is any declared filter, not just a narrow one. Narrowness is recorded
// in the tables above, which a commit can edit in the same breath as the
// workflow it describes; a `branches:` key is read straight off the YAML, where
// it cannot be. A `["**"]` workflow gaining `edited` strands nothing — it
// already ran on the stacked PR — but reading every filter keeps the rule one a
// reader can apply without first deciding which table an entry belongs to.
func TestBranchFilteredWorkflowsExcludeEditedActivityType(t *testing.T) {
	filteredTriggers := 0
	editedWorkflows := map[string]bool{}
	for _, name := range workflowFiles(t) {
		triggers := parseWorkflowTriggers(t, name, readWorkflow(t, name).On)
		for _, trigger := range pullRequestTriggers {
			config, ok := triggers[trigger]
			if !ok {
				continue
			}
			branches, filtered := pullRequestFilter(t, name, trigger, config, branchesFilterKey)
			if filtered {
				filteredTriggers++
			}

			// Read `types` on every trigger rather than only the filtered ones
			// this can fire on. The workflows asking for `edited` are exactly the
			// unfiltered ones, so recording them across the whole scan is what gives
			// this read an observation of its own — see the second guard below.
			types, hasTypes := pullRequestFilter(t, name, trigger, config, typesFilterKey)
			asksForEdited := hasTypes && slices.Contains(types, editedActivityType)
			if asksForEdited {
				editedWorkflows[name] = true
			}
			if !filtered || !asksForEdited {
				continue
			}
			t.Errorf("%s %s declares branches %v and types %v; %q makes it re-run when a "+
				"stacked PR is retargeted onto main, which the doc comment on this test says "+
				"no filtered workflow does. Drop %q, or rewrite that comment and the stall it "+
				"describes", name, trigger, branches, types,
				editedActivityType, editedActivityType)
		}
	}

	// Couple each read to a count, on the same reasoning as
	// TestEveryPullRequestWorkflowRecordsItsBranchFilter's: with nothing found
	// there is nothing to contradict, and this would pass vacuously whether the
	// tree had genuinely widened or the scan had stopped matching.
	//
	// A bare nonzero total is what no longer settles that. pullRequestFilter
	// takes its key as an argument, so a read aimed at the wrong one still
	// returns something: pointing the branches read at pathsFilterKey finds
	// validate-issue-templates.yml — the one pull-request workflow declaring
	// `paths:` — which holds the total at 1 while the scan has stopped reading
	// branches at all, and since that workflow declares no `types:` the loop
	// above then reports nothing.
	//
	// The floor is requiredWorkflowSpecs, every entry of which declares a
	// branches filter: TestAppWorkflowsRunOnStackedPRs fails an unrecorded one
	// by name, and assertPullRequestFilter holds the workflow to what the entry
	// records. It stays a floor rather than an equality because a workflow
	// filtering both its triggers counts twice, and because the two `[main]`
	// entries in otherPullRequestWorkflows land in this total as well.
	if want := len(requiredWorkflowSpecs); filteredTriggers < want {
		t.Errorf("found %d branch-filtered pull-request triggers, want at least %d — one per "+
			"requiredWorkflowSpecs entry; the scan is reading something other than %q",
			filteredTriggers, want, branchesFilterKey.name)
	}

	// The `types` read gets no floor from a table: no branch-filtered workflow
	// declares `types:` today, so within the subset this test fires on there is
	// nothing for that read to observe and a miswired key would sit unnoticed
	// behind the count above. The exact whole-tree set is the observation
	// instead, and pins both identities in the doc comment rather than merely
	// proving that some workflow somewhere still asks for `edited`.
	wantEditedWorkflows := map[string]bool{"dependabot-pr-title.yml": true, "pr-title.yml": true}
	if !maps.Equal(editedWorkflows, wantEditedWorkflows) {
		t.Errorf("pull-request workflows asking for %q = %v, want %v; either the scan is "+
			"reading something other than %q, or the doc comment and retarget stall it "+
			"describes need rewriting", editedActivityType, editedWorkflows,
			wantEditedWorkflows, typesFilterKey.name)
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
			assertPullRequestFilter(t, spec.path, branchesFilterKey, spec.branches)
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
// identically, so that workflow narrowing to [main] would now take a
// merge-gating check off every stacked PR, which would read green having
// skipped it.
func TestEveryPullRequestWorkflowRecordsItsBranchFilter(t *testing.T) {
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
	for _, name := range workflowFiles(t) {
		triggers := parseWorkflowTriggers(t, name, readWorkflow(t, name).On)
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
					"a narrow filter would let every stacked PR read green having skipped it", spec.path)
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
// required, the reasoning would be void — the stacked PR would be reading green
// over exactly such a gate, absent rather than pending on it, the failure
// TestAppWorkflowsRunOnStackedPRs describes. This turns that premise into
// something CI checks.
//
// It reads both tables, because the premise is about the pairing of a narrow
// filter with a required context and neither table has a monopoly on either
// half. Restricting it to otherPullRequestWorkflows left the nine app
// workflows resting on TestAppWorkflowsRunOnStackedPRs alone, which compares
// each workflow against the intent recorded beside it — and a commit is free
// to edit both. Narrowing slack.yml to `[main]` and editing its spec's
// pullRequestBranches to match passed the whole package clean, with
// `slack / required` still listed as required in CONTRIBUTING.md. Widened,
// that second edit is what trips this test: the narrowing is now judged
// against the documented gate rather than against its own paperwork.
//
// Scope is `branches:` alone. The same premise on the two sibling keys that
// also decide whether a workflow starts is
// TestNarrowTypeAndPathFiltersProduceNoRequiredContext's, which reaches it from
// the workflow tree rather than from these tables.
func TestNarrowPullRequestWorkflowsProduceNoRequiredContext(t *testing.T) {
	narrow := narrowPullRequestWorkflows(requiredWorkflowSpecs, otherPullRequestWorkflows)
	if len(narrow) == 0 {
		t.Skip("no deliberately-narrow entries recorded, so there is no premise to enforce")
	}

	reported := workflowReportedContexts(t)
	for _, context := range documentedRequiredContexts(t) {
		for _, file := range reported.direct[context] {
			if narrow[file] {
				t.Errorf("%s is recorded as deliberately narrow but reports required context %q; "+
					"a stacked PR skips it and reads green over that gate rather than pending on it, so its entry needs revisiting", file, context)
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
					"a stacked PR skips it and reads green over that gate rather than pending on it, so its entry needs revisiting", file, context)
			}
		}
	}
}

// narrowPullRequestWorkflows collects the workflow files whose recorded filter
// keeps them off a PR stacked on a feature branch, reading both tables through
// one predicate.
//
// It takes its tables as parameters rather than closing over the package ones
// so that TestNarrowPullRequestWorkflowsKeyOnPath can feed it synthetic
// entries. Every real spec records "**" today, so the requiredWorkflowSpecs
// half contributes nothing to the live map, and a keying slip there —
// narrow[spec.name] where narrow[spec.path] was meant, which for the shared
// spec is "shared" against "shared-test.yml" — would ship green and simply
// fail to fire on the day it mattered.
func narrowPullRequestWorkflows(required []requiredWorkflowSpec, other []pullRequestTriggerSpec) map[string]bool {
	narrow := map[string]bool{}
	for i := range other {
		if isNarrowBranchFilter(other[i].branches) {
			narrow[other[i].path] = true
		}
	}
	for i := range required {
		if isNarrowBranchFilter(required[i].pullRequestBranches) {
			narrow[required[i].path] = true
		}
	}
	return narrow
}

// isNarrowBranchFilter reports whether a recorded `branches:` filter keeps a
// workflow off a PR stacked on a feature branch. A nil filter is absent from
// the workflow and so reaches every base branch already, and one naming "**"
// with nothing excluded reaches them explicitly; anything else names the bases
// it runs on and skips the rest.
//
// A negated pattern makes "**" stop meaning full reach: GitHub evaluates the
// list in order, so `["**", "!justin/**"]` matches every base and then takes
// back the stacked ones. That is the same failure pullRequestFilter
// refuses branches-ignore for, in a spelling `branches:` can hold, so it is
// read as narrow rather than waved through by the "**" already present.
//
// nil and []string{} part company here, which is why the guard is against nil
// rather than against len. A nil is how pullRequestTriggerSpec spells "declares
// no filter", the reading assertPullRequestFilter already gives it. A
// present-but-empty one names no base at all, so nothing establishes that it
// reaches a stacked PR and the conservative reading is the safe one. Neither
// table spells it today; the split is pinned by TestIsNarrowBranchFilter so it
// stays a decision, and an app spec whose intent went unrecorded is nil and
// fails TestAppWorkflowsRunOnStackedPRs by name instead of here.
func isNarrowBranchFilter(branches []string) bool {
	if branches == nil {
		return false
	}
	for _, branch := range branches {
		if strings.HasPrefix(branch, "!") {
			return true
		}
	}
	return !slices.Contains(branches, "**")
}

// TestIsNarrowBranchFilter pins each reading the helper gives. It is a short
// function, but it is the single point where "does this filter keep the
// workflow off a stacked PR" is decided for a required aggregate and for an
// unrequired workflow alike, so each reading is spelled out rather than left to
// be inferred from the callers.
//
// Not every reading is reachable from both tables: only pullRequestTriggerSpec
// can carry a present-but-empty filter, since TestAppWorkflowsRunOnStackedPRs
// rejects that spelling on a requiredWorkflowSpec as an unrecorded intent
// rather than honoring it.
func TestIsNarrowBranchFilter(t *testing.T) {
	tests := []struct {
		name     string
		branches []string
		want     bool
	}{
		{name: "nil declares no filter and reaches every base", branches: nil},
		{name: "** reaches every base explicitly", branches: []string{"**"}},
		{name: "** alongside a named base still reaches every base", branches: []string{"main", "**"}},
		{name: "main alone skips a stacked PR", branches: []string{"main"}, want: true},
		{name: "named bases without ** skip the rest", branches: []string{"main", "release"}, want: true},
		{name: "present but empty names no base at all", branches: []string{}, want: true},
		{name: "** with a negated pattern takes the stacked bases back", branches: []string{"**", "!justin/**"}, want: true},
		{name: "a negated pattern before ** is narrow too", branches: []string{"!justin/**", "**"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isNarrowBranchFilter(test.branches); got != test.want {
				t.Errorf("isNarrowBranchFilter(%#v) = %t, want %t", test.branches, got, test.want)
			}
		})
	}
}

// TestNarrowPullRequestWorkflowsKeyOnPath pins the set construction itself,
// which the live tables cannot exercise: all nine required specs record "**",
// so that half of narrowPullRequestWorkflows contributes nothing and every
// assertion above holds identically with it deleted. Synthetic entries are the
// only way to prove it runs at all, and that it keys on path — the shared spec
// is named "shared" but lives in shared-test.yml, so a slip to spec.name would
// record a file that does not exist and silently match no reported context.
func TestNarrowPullRequestWorkflowsKeyOnPath(t *testing.T) {
	got := narrowPullRequestWorkflows(
		[]requiredWorkflowSpec{
			{name: "shared", path: "shared-test.yml", pullRequestBranches: []string{"main"}},
			{name: "slack", path: "slack.yml", pullRequestBranches: []string{"**"}},
		},
		[]pullRequestTriggerSpec{
			{path: "codeql.yml", branches: []string{"main"}},
			{path: "secrets-scan.yml"},
		},
	)

	want := map[string]bool{"shared-test.yml": true, "codeql.yml": true}
	if !maps.Equal(got, want) {
		t.Errorf("narrowPullRequestWorkflows() = %v, want %v", got, want)
	}
}

// TestPullRequestWorkflowsRecordTheirTypeAndPathFilters pins the two sibling
// spellings of the failure the branch-filter tests above pin.
//
// `branches:` is not the only key on a pull-request trigger that decides
// whether a workflow starts. `types:` narrows which activity on the pull
// request starts it, and a trigger-level `paths:` narrows which diffs do.
// Either one, on a workflow owning a required context, lands in the identical
// place: the workflow never runs, so GitHub never registers its check, and the
// PR sits on "Expected — Waiting for status to be reported" with nothing red to
// point at. That is the 2026-08-14 shape reached without touching `branches:`
// at all, which is why it is guarded in the same place rather than left to a
// reviewer noticing the difference between three keys that read alike.
//
// The nine aggregates are what this most nearly happened to. They already
// narrow by diff — but with `dorny/paths-filter` inside the `changes` job, so
// the workflow still starts and the `if: always()` aggregate still reports.
// Lifting that narrowing up to the trigger reads like the same intent spelled
// more cheaply and is not: it deregisters `<app> / required` for every pull
// request the filter misses.
func TestPullRequestWorkflowsRecordTheirTypeAndPathFilters(t *testing.T) {
	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			assertPullRequestFilter(t, spec.path, typesFilterKey, spec.pullRequestTypes)
			assertPullRequestFilter(t, spec.path, pathsFilterKey, spec.pullRequestPaths)
		})
	}
	for i := range otherPullRequestWorkflows {
		spec := &otherPullRequestWorkflows[i]
		t.Run(strings.TrimSuffix(spec.path, ".yml"), func(t *testing.T) {
			assertPullRequestFilter(t, spec.path, typesFilterKey, spec.types)
			assertPullRequestFilter(t, spec.path, pathsFilterKey, spec.paths)
		})
	}
}

// defaultPullRequestTypes are the activity types GitHub starts a pull-request
// trigger on when the workflow declares no `types:` key. Both events in
// pullRequestTriggers share this default.
//
// `opened` and `synchronize` are the load-bearing pair: between them they cover
// every head SHA an ordinary pull request presents, and this repo's protection
// is strict, so the head moves again before any merge. A `types:` list dropping
// either leaves some head SHA the workflow never ran on, and a required context
// absent on the head SHA is exactly what branch protection reports as pending
// forever. `reopened` is held to the same standard rather than argued case by
// case: it is part of the baseline GitHub gives for free, so dropping it is a
// decision, and this is where a decision about what starts a workflow belongs.
var defaultPullRequestTypes = []string{"opened", "synchronize", "reopened"}

// narrowPullRequestTriggerReason reports why a pull-request trigger fails to
// start for some ordinary pull request, or "" when it always starts. It reads
// the workflow rather than a recorded filter, which is what makes it a second
// line behind the exact comparisons above; narrowPullRequestWorkflows is the
// table-side equivalent for `branches:`.
//
// Reading the tree means it inherits pullRequestFilter's refusal of the
// inverted spellings, so a `paths-ignore` anywhere in .github/workflows fails
// here rather than being weighed. That is the intended contract — the tables
// have no way to record one — and it costs nothing in reach, since every
// pull-request workflow must carry a table entry regardless.
//
// Any declared `paths:` counts, with no "**" escape of the kind
// isNarrowBranchFilter grants a branch filter. The two are not symmetric: a
// base branch always exists to be matched, so `branches: ["**"]` genuinely
// reaches every pull request, while a path filter is matched against the diff
// and a pull request may carry no files at all — an empty commit, a
// revert-of-a-revert — at which point even `paths: ["**"]` matches nothing and
// the workflow does not start. There is no spelling of a trigger-level `paths:`
// a required context can survive, so none is carved out.
//
// `paths` is reported ahead of `types` for a trigger carrying both. Either
// alone disqualifies the workflow from owning a required context, so there is
// nothing to gain by listing both, and one named reason is what the reader has
// to act on.
func narrowPullRequestTriggerReason(t *testing.T, path, trigger string, config any) string {
	t.Helper()

	if paths, declared := pullRequestFilter(t, path, trigger, config, pathsFilterKey); declared {
		return fmt.Sprintf("%s.paths = %v, so it does not start for a pull request whose diff matches none of them",
			trigger, paths)
	}

	types, declared := pullRequestFilter(t, path, trigger, config, typesFilterKey)
	if !declared {
		return ""
	}
	missing := []string{}
	for _, activity := range defaultPullRequestTypes {
		if !slices.Contains(types, activity) {
			missing = append(missing, activity)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("%s.types = %v, dropping %v from the default set, so it does not start for every head of an ordinary pull request",
		trigger, types, missing)
}

// TestNarrowPullRequestTriggerReason pins each reading the predicate gives,
// against synthetic triggers rather than the live tree.
//
// The tree cannot exercise it. No workflow today pairs a narrow filter with a
// required context — that is the property the suite exists to keep true — so
// every branch that builds a reason goes unrun by the assertions above, and
// `missing` could be assembled wrongly, or the `paths`-before-`types`
// precedence inverted, with the package still green. The mutation sweep in the
// PR reaches those branches, but it runs out of band and CI never sees it.
// This is the same argument TestIsNarrowBranchFilter rests on for the branch
// half, and it belongs here for the same reason.
//
// Reasons are matched by substring, not compared whole: what has to hold is
// that the message names the trigger, the offending value and — for a types
// filter — which defaults went missing, not the sentence built around them.
func TestNarrowPullRequestTriggerReason(t *testing.T) {
	tests := []struct {
		name        string
		config      any
		narrow      bool
		contains    []string
		notContains []string
	}{
		{name: "a bare trigger declares nothing and starts for every pull request", config: nil},
		{name: "an empty mapping is the same reach spelled longhand", config: map[string]any{}},
		{name: "a branches filter alone is another test's business", config: map[string]any{"branches": []any{"main"}}},
		{
			name:   "the default set spelled out explicitly is not narrow",
			config: map[string]any{"types": []any{"opened", "synchronize", "reopened"}},
		},
		{
			name:   "adding to the default set widens rather than narrows",
			config: map[string]any{"types": []any{"opened", "edited", "synchronize", "reopened"}},
		},
		{
			name:     "dropping one default is narrow and names it",
			config:   map[string]any{"types": []any{"opened", "reopened"}},
			narrow:   true,
			contains: []string{"pull_request.types = [opened reopened]", "[synchronize]"},
		},
		{
			name:     "ready_for_review alone drops all three",
			config:   map[string]any{"types": []any{"ready_for_review"}},
			narrow:   true,
			contains: []string{"pull_request.types = [ready_for_review]", "[opened synchronize reopened]"},
		},
		{
			name:     "an empty types filter drops every default",
			config:   map[string]any{"types": []any{}},
			narrow:   true,
			contains: []string{"pull_request.types = []", "[opened synchronize reopened]"},
		},
		{
			name:     "a scalar types filter is read, not skipped",
			config:   map[string]any{"types": "opened"},
			narrow:   true,
			contains: []string{"pull_request.types = [opened]", "[synchronize reopened]"},
		},
		{
			name:     "any paths filter is narrow, ** included",
			config:   map[string]any{"paths": []any{"**"}},
			narrow:   true,
			contains: []string{"pull_request.paths = [**]"},
		},
		{
			name:     "a scalar paths filter is read too",
			config:   map[string]any{"paths": "apps/slack/**"},
			narrow:   true,
			contains: []string{"pull_request.paths = [apps/slack/**]"},
		},
		{
			name:        "paths is reported ahead of types when a trigger carries both",
			config:      map[string]any{"paths": []any{"docs/**"}, "types": []any{"ready_for_review"}},
			narrow:      true,
			contains:    []string{"pull_request.paths = [docs/**]"},
			notContains: []string{".types"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := narrowPullRequestTriggerReason(t, "synthetic.yml", "pull_request", test.config)
			if (got != "") != test.narrow {
				t.Fatalf("narrowPullRequestTriggerReason(%#v) = %q, want narrow = %t", test.config, got, test.narrow)
			}
			for _, want := range test.contains {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not mention %q, so it does not say what to go fix", got, want)
				}
			}
			for _, unwanted := range test.notContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("reason %q mentions %q; one named reason is what the reader acts on", got, unwanted)
				}
			}
		})
	}
}

// TestNarrowTypeAndPathFiltersProduceNoRequiredContext enforces, for `types:`
// and for trigger-level `paths:`, the premise
// TestNarrowPullRequestWorkflowsProduceNoRequiredContext enforces for
// `branches:` — that a workflow which cannot start for some ordinary pull
// request is not one whose silence blocks a merge.
//
// It reads the workflow tree rather than the tables, where that test reads the
// tables. Either source works while assertPullRequestFilter holds the two in
// agreement, and the choice is deliberate on both sides: recording a filter is
// what forces a `why` to be written, and re-deriving one is what survives a
// commit that edits the workflow and its paperwork together. Taking one route
// each leaves the pair covered from both ends.
//
// The workflows narrowing today all produce no required context:
// dependabot-pr-title.yml drops `synchronize` because a Dependabot force-push
// does not change the title it reads, and validate-issue-templates.yml runs
// only for the templates it validates. pr-title.yml and claude-code-review.yml
// declare a `types:` too but are not narrow by it — both carry the full default
// set and add to it, which widens their reach rather than narrowing it.
func TestNarrowTypeAndPathFiltersProduceNoRequiredContext(t *testing.T) {
	narrow := map[string]string{}
	scanned := 0
	for _, name := range workflowFiles(t) {
		triggers := parseWorkflowTriggers(t, name, readWorkflow(t, name).On)
		for _, trigger := range pullRequestTriggers {
			config, ok := triggers[trigger]
			if !ok {
				continue
			}
			scanned++
			// Any narrow trigger marks the file, rather than only a workflow
			// narrow on all of them. A workflow declaring both events would
			// report its checks twice over, so the pair is not a fallback for
			// one another, and holding each to full reach independently is
			// what assertPullRequestFilter already does for `branches`. A file
			// narrow on both keeps whichever reason came last, which is
			// arbitrary but never wrong: each alone is disqualifying, so the
			// one reported is a real reason to go fix the file.
			if reason := narrowPullRequestTriggerReason(t, name, trigger, config); reason != "" {
				narrow[name] = reason
			}
		}
	}
	// An empty narrow set is a legitimate state, so it cannot stand in for a
	// scan that matched nothing. Couple the count instead: a renamed trigger
	// key or an emptied pullRequestTriggers would otherwise leave every
	// assertion below with nothing to contradict.
	if scanned == 0 {
		t.Fatal("no workflow was found running on a pull-request trigger; the scan matched nothing and this check would be vacuous")
	}

	reported := workflowReportedContexts(t)
	for _, context := range documentedRequiredContexts(t) {
		for _, file := range reported.direct[context] {
			if reason, ok := narrow[file]; ok {
				t.Errorf("%s reports required context %q, but %s; "+
					"a required check that never registers leaves the pull request pending forever", file, context, reason)
			}
		}
		// Reusable-workflow calls report as "<caller job> / <inner job>".
		caller, _, ok := strings.Cut(context, contextSeparator)
		if !ok {
			continue
		}
		for _, file := range reported.reusable[caller] {
			if reason, ok := narrow[file]; ok {
				t.Errorf("%s has a caller job reporting required context %q, but %s; "+
					"a required check that never registers leaves the pull request pending forever", file, context, reason)
			}
		}
	}
}

// assertPullRequestFilter compares a workflow's declared filter for one key
// against the intended one. A nil want means the workflow must declare no key
// of that name at all.
func assertPullRequestFilter(t *testing.T, path string, key pullRequestFilterKey, want []string) {
	t.Helper()

	triggers := parseWorkflowTriggers(t, path, readWorkflow(t, path).On)
	checked := 0
	for _, trigger := range pullRequestTriggers {
		config, ok := triggers[trigger]
		if !ok {
			continue
		}
		checked++

		got, declared := pullRequestFilter(t, path, trigger, config, key)
		switch {
		case want == nil && declared:
			t.Errorf("%s %s declares %s %v, want no filter at all", path, trigger, key.name, got)
		case want == nil:
		case !declared:
			t.Errorf("%s %s declares no %s filter, want %v", path, trigger, key.name, want)
		case !slices.Equal(got, want):
			t.Errorf("%s %s.%s = %v, want %v", path, trigger, key.name, got, want)
		}
	}
	if checked == 0 {
		t.Fatalf("%s must run on one of %v", path, pullRequestTriggers)
	}
}

// pullRequestFilter reads one filter key off a parsed pull-request trigger,
// reporting whether it is declared at all. It accepts both YAML spellings of a
// single filter — a bare scalar and a sequence — so `main` and `[main]` are not
// treated as different decisions.
//
// The comparison this feeds is still order-sensitive, which matters only once a
// workflow earns a multi-element filter: ["main", "release/**"] and
// ["release/**", "main"] have identical reach but are not interchangeable here.
// That is deliberate — the table records the spelling a reader will find in the
// YAML — but it is stricter than reach alone, so record the order as written.
func pullRequestFilter(t *testing.T, path, trigger string, pullRequest any, key pullRequestFilterKey) (values []string, declared bool) {
	t.Helper()

	if pullRequest == nil {
		return nil, false
	}
	config, ok := pullRequest.(map[string]any)
	if !ok {
		t.Fatalf("%s %s trigger has unexpected type %T", path, trigger, pullRequest)
	}

	// The inverted spellings are the ones these tables cannot express, and they
	// fail open rather than loudly: a workflow using one declares no key of the
	// positive name, which reads below as full reach — while `branches-ignore:
	// ["justin/**"]` would take the workflow off exactly the stacked PRs this
	// suite exists to keep it on, and `paths-ignore` does the same to a
	// required context on the diff axis. Refuse them here instead, so adding
	// one forces the decision into the table. Checked before the positive
	// spelling and independently of it: GitHub rejects the two together, but
	// this should not be the thing that assumes so.
	//
	// This refusal is reached from narrowPullRequestTriggerReason's scan of the
	// tree as well as from the exact comparisons, so it fires for a workflow
	// carrying no table entry too. That is not a wider net than the tabled one:
	// TestEveryPullRequestWorkflowRecordsItsBranchFilter already requires an
	// entry for every workflow with a pull-request trigger, so the two sets are
	// the same. It does mean the message has to name the remedy for either
	// caller, which is why it offers both.
	if key.ignoreName != "" {
		if _, ok := config[key.ignoreName]; ok {
			t.Fatalf("%s %s declares %s, which these tables cannot record; extend pullRequestTriggerSpec "+
				"to express it, or narrow inside the job as the nine aggregates do — a workflow reporting "+
				"a required context has no survivable spelling of it", path, trigger, key.ignoreName)
		}
	}

	raw, ok := config[key.name]
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
			entry, ok := value.(string)
			if !ok {
				t.Fatalf("%s %s.%s contains non-string value %T", path, trigger, key.name, value)
			}
			values = append(values, entry)
		}
		return values, true
	default:
		t.Fatalf("%s %s.%s has unexpected type %T", path, trigger, key.name, raw)
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
	registered := make(map[string]bool, len(requiredWorkflowSpecs))
	for i := range requiredWorkflowSpecs {
		registered[requiredWorkflowSpecs[i].path] = true
	}

	seen := 0
	for _, name := range workflowFiles(t) {
		if _, ok := readWorkflow(t, name).Jobs[requiredJobID]; !ok {
			continue
		}
		seen++
		if !registered[name] {
			t.Errorf("%s defines a required aggregate job but has no requiredWorkflowSpecs entry", name)
		}
	}

	// workflowFiles already fatals on an empty scan, so this is purely the
	// count coupling: a workflow that grows a job
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

			required := requiredAggregateJob(t, spec, workflow)
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

			requiredNeeds := stringSet(parseWorkflowNeeds(t, requiredJobID, required.Needs))
			if !requiredNeeds[changesJobID] {
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
				if need == changesJobID {
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

// TestExtensionWorkflowsStayInLockstep pins chrome-extension.yml and
// edge-extension.yml as one file with the browser's name swapped.
//
// apps/edge-extension is a platform fork of apps/chrome-extension, and their
// workflows are the same copy-and-swap: a timeout raised, a step dropped, or an
// action pinned forward on one side only is invisible in review, because
// nothing ever puts the two files side by side. What ships is one browser's
// extension going out through a weaker gate than the other's.
//
// TestAppWorkflowsRunOnStackedPRs above covers one key of these same two files,
// pinning each one's pull_request branch filter against the intent recorded in
// its spec. That is a per-workflow assertion about a value; this is a whole-file
// assertion about a pair, so a one-sided edit to any other line — which no
// recorded intent exists for — fails here instead of nowhere.
//
// The Chrome<->Edge lockstep section of CLAUDE.md carries the policy: why these
// two files are guarded here rather than by scripts/check-extension-lockstep.sh,
// which covers the two app trees.
func TestExtensionWorkflowsStayInLockstep(t *testing.T) {
	chromePath, chrome := maskedExtensionWorkflow(t, "chrome-extension", "Chrome")
	edgePath, edge := maskedExtensionWorkflow(t, "edge-extension", "Edge")

	// Reported with the first diverging line and its text, not the counts alone:
	// "184 lines and 182" says a step was added or dropped without saying where,
	// which is the one thing the reader needs in order to go look.
	if len(chrome) != len(edge) {
		n := firstDivergentLine(chrome, edge)
		t.Fatalf("%s has %d lines and %s has %d, first diverging at line %d: a step, key, or comment exists in only one copy\n\t%s: %s\n\t%s: %s",
			chromePath, len(chrome), edgePath, len(edge), n,
			chromePath, lineAt(chrome, n), edgePath, lineAt(edge, n))
	}
	for i := range chrome {
		if chrome[i] == edge[i] {
			continue
		}
		t.Errorf("line %d has diverged (shown with the sanctioned tokens masked):\n\t%s: %s\n\t%s: %s",
			i+1, chromePath, chrome[i], edgePath, edge[i])
	}
}

// maskedExtensionWorkflow reads one extension workflow and returns its path
// alongside its lines, with every sanctioned delta rewritten to a shared
// placeholder so the two copies can be compared exactly.
//
// The app slug, change output and verifier env var are read out of that app's
// requiredWorkflowSpecs entry rather than restated here, so renaming one there
// cannot leave a stale duplicate quietly widening what this ignores. Only the
// browser's prose name is spelled out: the specs carry it too, but embedded in
// composite strings (verifierStepName, unchangedOutput) that would have to be
// taken apart to recover it.
//
// The four rules cannot interfere with one another because each matches a
// spelling the others do not: the hyphenated slug, the underscored output, the
// SCREAMING_CASE env var, and — case-sensitively, under \b anchors — the
// capitalized prose word. Store names (Chrome Web Store, Microsoft Edge
// Add-ons) are deliberately not masked, unlike check-extension-lockstep.sh:
// these workflows carry none today, and a step that adds one is publishing to a
// different store, which is a real divergence worth stopping on rather than
// normalizing away. The same goes for any future browser-specific publish step
// or lowercase store URL: this test failing is the intended signal, and the fix
// is a new mask documented here and in CLAUDE.md, never deleting the assertion.
//
// Each copy is masked for its own slug and browser name only, not for both.
// That is stricter than check-extension-lockstep.sh, which masks both on both
// sides: a copy naming the wrong browser reads as a match there and is reported
// here. The cost is that neither file can name the other — a "keep in lockstep
// with edge-extension.yml" comment diverges under its own slug mask, as does any
// prose naming both browsers. A sibling-agnostic pointer does work, and is what
// both files carry at the top. Relaxing this to symmetric masking would buy
// those cross-references back at the price of the wrong-browser catch, which
// would then need a separate assertion, the way check-i18n-parity.sh covers the
// same blind spot in the script.
func maskedExtensionWorkflow(t *testing.T, specName, browser string) (path string, lines []string) {
	t.Helper()

	const browserMask = "<browser>"
	// Literal matches rather than lookaheads, because RE2 has none. The article
	// rule runs second, against the placeholder the browser rule leaves behind:
	// "a Chrome extension" and "an Edge extension" are the same sentence, and
	// the article is forced by the word just erased. Both cases are matched so a
	// sentence-initial "A Chrome…"/"An Edge…" is covered too — safe, because the
	// replacement is fixed-case, so nothing can hide in the article's own
	// capitalization that is not already visible in the rest of the line.
	browserWord := regexp.MustCompile(`\b` + regexp.QuoteMeta(browser) + `\b`)
	article := regexp.MustCompile(`\b[Aa]n? ` + browserMask)

	spec := requiredWorkflowSpecByName(t, specName)
	source := readWorkflowSource(t, spec.path)
	source = strings.ReplaceAll(source, spec.name, "<app>")
	source = strings.ReplaceAll(source, spec.changeOutput, "<change-output>")
	source = strings.ReplaceAll(source, spec.changedEnv, "<changed-env>")
	source = browserWord.ReplaceAllString(source, browserMask)
	source = article.ReplaceAllString(source, "<article> "+browserMask)
	return spec.path, strings.Split(source, "\n")
}

// lineAt returns the 1-based line n, or a marker when that copy ended first —
// which is the normal case for the shorter side of a length mismatch.
func lineAt(lines []string, n int) string {
	if n-1 >= len(lines) {
		return "(end of file)"
	}
	return lines[n-1]
}

// firstDivergentLine returns the 1-based line where two masked copies first
// differ, or one past the shorter copy when it is a prefix of the longer.
func firstDivergentLine(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i + 1
		}
	}
	return min(len(a), len(b)) + 1
}

func requiredWorkflowSpecByName(t *testing.T, name string) *requiredWorkflowSpec {
	t.Helper()

	for i := range requiredWorkflowSpecs {
		if requiredWorkflowSpecs[i].name == name {
			return &requiredWorkflowSpecs[i]
		}
	}
	t.Fatalf("no requiredWorkflowSpecs entry named %q", name)
	return nil
}

func readWorkflow(t *testing.T, name string) githubWorkflow {
	t.Helper()

	var workflow githubWorkflow
	if err := yaml.Unmarshal(readWorkflowBytes(t, name), &workflow); err != nil {
		t.Fatalf("parse %s workflow: %v", name, err)
	}
	// A job key with no body decodes to a nil entry rather than to a zero job.
	// (`job: {}` does not — that is a non-nil zero struct; only a true null
	// trips this.) Named here so it reports as the malformed workflow it is, at
	// the file that carries it, rather than as a nil dereference in whichever
	// assertion reached it first. Every githubWorkflow in this package comes
	// from here, so this is also what lets a Jobs lookup anywhere below take
	// its *githubJob as non-nil without re-checking.
	//
	// Unasserted, like the other fatals here: a fixture carrying a null job
	// would have to sit in .github/workflows, where every other scan in this
	// package would read it too. Verified by mutation instead — a bare
	// `orphan:` added under a workflow's `jobs:` names that file and id rather
	// than panicking. Re-run that if you touch this.
	for id, job := range workflow.Jobs {
		if job == nil {
			t.Fatalf("%s job %q has an empty body", name, id)
		}
	}
	return workflow
}

// readWorkflowBytes returns a workflow file's raw contents. It returns bytes
// rather than a string because parsing is the overwhelmingly common use — the
// tests in this package read the workflow directory many times over — and only
// the lockstep comparison wants text, so the conversion belongs on that path
// rather than on every parse.
func readWorkflowBytes(t *testing.T, name string) []byte {
	t.Helper()

	// #nosec G304 -- callers pass checked-in workflow file names, either from
	// requiredWorkflowSpecs or from a ReadDir of .github/workflows itself.
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s workflow: %v", name, err)
	}
	return data
}

// readWorkflowSource returns a workflow file's raw text. Callers that only need
// its shape should use readWorkflow; this exists for the lockstep comparison,
// which is about the bytes — comments and formatting included — and would be
// blind to a divergence YAML parsing throws away.
func readWorkflowSource(t *testing.T, name string) string {
	t.Helper()

	return string(readWorkflowBytes(t, name))
}

// workflowFiles lists the workflow files in .github/workflows. It fails rather
// than returning an empty list: a renamed directory or a changed extension
// would otherwise leave every scan built on it with nothing to contradict, and
// so passing vacuously.
func workflowFiles(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatalf("no workflow files found in %s", dir)
	}
	return names
}

// requiredAggregateJob returns the aggregate job of the workflow a spec names.
// Both callers reach for it before reading anything else about that workflow,
// and for both a spec whose workflow has no such job is the same registration
// bug, so the lookup and its diagnosis are written once here.
func requiredAggregateJob(t *testing.T, spec *requiredWorkflowSpec, workflow githubWorkflow) *githubJob {
	t.Helper()

	job, ok := workflow.Jobs[requiredJobID]
	if !ok {
		t.Fatalf("%s workflow is missing its %q aggregate job", spec.name, requiredJobID)
	}
	return job
}

func requiredWorkflowQualityGates(t *testing.T, spec *requiredWorkflowSpec, workflow githubWorkflow) map[string]bool {
	t.Helper()

	qualityGates := map[string]bool{}
	for id, job := range workflow.Jobs {
		needs := parseWorkflowNeeds(t, id, job.Needs)
		if !looksLikeRequiredWorkflowQualityGate(spec, job, needs) {
			continue
		}
		if !slices.Contains(needs, changesJobID) {
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
	return !slices.Contains(needs, requiredJobID)
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

	required := requiredAggregateJob(t, spec, workflow)
	for i := range required.Steps {
		current := &required.Steps[i]
		if current.Name != spec.verifierStepName {
			continue
		}
		if current.Shell != "bash" {
			t.Fatalf("%s shell = %q, want bash", spec.verifierStepName, current.Shell)
		}
		if strings.TrimSpace(current.Run) == "" {
			t.Fatalf("%s step has empty run script", spec.verifierStepName)
		}
		return current.Run
	}
	t.Fatalf("%s required job is missing %s step", spec.name, spec.verifierStepName)
	return ""
}

func runVerifierScriptWithEnv(t *testing.T, script string, env map[string]string) (string, error) {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "workflow-step.sh")
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
