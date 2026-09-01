package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type cliWorkflowContract struct {
	Env  map[string]any            `yaml:"env"`
	Jobs map[string]cliWorkflowJob `yaml:"jobs"`
}

type cliWorkflowJob struct {
	If              any               `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Env             map[string]any    `yaml:"env"`
	Outputs         map[string]any    `yaml:"outputs"`
	Steps           []cliWorkflowStep `yaml:"steps"`
	TimeoutMinutes  int               `yaml:"timeout-minutes"`
}

type cliWorkflowStep struct {
	Name            string         `yaml:"name"`
	If              any            `yaml:"if"`
	ContinueOnError any            `yaml:"continue-on-error"`
	Env             map[string]any `yaml:"env"`
	With            map[string]any `yaml:"with"`
	Uses            string         `yaml:"uses"`
	Run             string         `yaml:"run"`
}

func validateRequiredCLIWorkflowGate(job *cliWorkflowJob, step *cliWorkflowStep, expectedJobIf string) error {
	if job.If != expectedJobIf {
		return fmt.Errorf("job if = %#v, want %q", job.If, expectedJobIf)
	}
	if job.ContinueOnError != nil {
		return fmt.Errorf("job continue-on-error must be absent, got %#v", job.ContinueOnError)
	}
	if step.If != nil {
		return fmt.Errorf("step if must be absent, got %#v", step.If)
	}
	if step.ContinueOnError != nil {
		return fmt.Errorf("step continue-on-error must be absent, got %#v", step.ContinueOnError)
	}
	return nil
}

func TestRequiredCLIWorkflowGateRejectsBypassMutations(t *testing.T) {
	t.Parallel()
	const expectedJobIf = "needs.changes.outputs.cli == 'true'"
	for _, test := range []struct {
		name, jobIf, jobExtra, stepExtra string
		wantErr                          bool
	}{
		{name: "valid", jobIf: "    if: needs.changes.outputs.cli == 'true'\n"},
		{name: "job if removed", wantErr: true},
		{name: "job if narrowed", jobIf: "    if: github.event_name == 'push'\n", wantErr: true},
		{name: "job continues on error", jobIf: "    if: needs.changes.outputs.cli == 'true'\n", jobExtra: "    continue-on-error: true\n", wantErr: true},
		{name: "job explicit false", jobIf: "    if: needs.changes.outputs.cli == 'true'\n", jobExtra: "    continue-on-error: false\n", wantErr: true},
		{name: "step conditional", jobIf: "    if: needs.changes.outputs.cli == 'true'\n", stepExtra: "        if: github.event_name == 'push'\n", wantErr: true},
		{name: "step continues on error", jobIf: "    if: needs.changes.outputs.cli == 'true'\n", stepExtra: "        continue-on-error: true\n", wantErr: true},
		{name: "step explicit false", jobIf: "    if: needs.changes.outputs.cli == 'true'\n", stepExtra: "        continue-on-error: false\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := "jobs:\n  test:\n" + test.jobIf + test.jobExtra +
				"    steps:\n      - name: required gate\n" + test.stepExtra
			var workflow cliWorkflowContract
			if err := yaml.Unmarshal([]byte(source), &workflow); err != nil {
				t.Fatalf("parse workflow mutation: %v", err)
			}
			job := workflow.Jobs["test"]
			if len(job.Steps) != 1 {
				t.Fatalf("workflow mutation has %d steps, want one", len(job.Steps))
			}
			err := validateRequiredCLIWorkflowGate(&job, &job.Steps[0], expectedJobIf)
			if (err != nil) != test.wantErr {
				t.Fatalf("workflow mutation error = %v, want error=%t", err, test.wantErr)
			}
		})
	}
}

func TestCLICustomerJourneyTimeoutBudget(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(cliRepoRoot, ".github", "workflows", "cli.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow cliWorkflowContract
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse public CLI workflow: %v", err)
	}
	for jobName, want := range map[string]int{
		"journey":         35,
		"journey-cleanup": 10,
	} {
		job, ok := workflow.Jobs[jobName]
		if !ok {
			t.Fatalf("public CLI workflow has no %q job", jobName)
		}
		if job.TimeoutMinutes != want {
			t.Errorf("public CLI workflow %s timeout = %d minutes, want %d", jobName, job.TimeoutMinutes, want)
		}
	}
}

func TestCLIImageContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	for _, want := range []string{
		"FROM scratch",
		"ARG HUB_TRUST_ROOT_B64=",
		"ARG SESSION_RELAY_URL=",
		"-X github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub.defaultServerPublicKeyB64=${HUB_TRUST_ROOT_B64}",
		"-X github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay.defaultURL=${SESSION_RELAY_URL}",
		"COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=build /out/qurl /usr/local/bin/qurl",
		"USER 65532:65532",
		`ENTRYPOINT ["/usr/local/bin/qurl"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("CLI Dockerfile missing %q", want)
		}
	}
	for _, forbidden := range []string{"qurl-connector", " apk ", " apt-get ", "ENTRYPOINT [\"/bin/", "\nVOLUME ", "\nCMD "} {
		if strings.Contains(dockerfile, forbidden) {
			t.Fatalf("CLI Dockerfile contains forbidden companion/runtime surface %q", forbidden)
		}
	}
	finalStage := dockerfile[strings.LastIndex(dockerfile, "FROM scratch"):]
	if got := strings.Count(finalStage, "\nCOPY "); got != 2 {
		t.Fatalf("scratch image has %d COPY instructions, want exactly CA data plus /usr/local/bin/qurl", got)
	}
}

func TestActiveSourcesDoNotReferenceLegacyConnectorArtifacts(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Clean(cliRepoRoot)
	forbidden := [][]byte{
		[]byte("ghcr.io/layervai/qurl-connector"),
		[]byte("/usr/local/bin/qurl-connector"),
	}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "test", "testdata", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".golden") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is constrained to regular source files below the repository root.
		if err != nil {
			return err
		}
		for _, needle := range forbidden {
			if bytes.Contains(data, needle) {
				t.Errorf("active file %s references retired customer artifact %q", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCLIRequiredPRTestGatesAreExactAndFailClosed(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(cliRepoRoot, ".github", "workflows", "cli.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow cliWorkflowContract
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse public CLI workflow: %v", err)
	}
	const expectedCLIJobIf = "needs.changes.outputs.cli == 'true'"
	findStep := func(jobName, name string) cliWorkflowStep {
		t.Helper()
		job, ok := workflow.Jobs[jobName]
		if !ok {
			t.Fatalf("public CLI workflow has no %q job", jobName)
		}
		var matches []cliWorkflowStep
		for _, step := range job.Steps {
			if step.Name == name {
				matches = append(matches, step)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("public CLI workflow %s job has %d %q steps, want one", jobName, len(matches), name)
		}
		return matches[0]
	}
	assertRequiredGate := func(jobName string, step cliWorkflowStep) {
		t.Helper()
		job := workflow.Jobs[jobName]
		if err := validateRequiredCLIWorkflowGate(&job, &step, expectedCLIJobIf); err != nil {
			t.Errorf("public CLI workflow %s / %s is bypassable: %v", jobName, step.Name, err)
		}
	}

	lifecycleTests := []string{
		"TestDaemonServesTwoResourcesAndStopsOneIndependently",
		"TestLinuxStartRotatesEpochAfterLocalTerminalDisable",
		"TestLocalPublishCompensatesSetupFailureBeforeDaemonOwnership",
		"TestPublishDaemonLifecycleServesRealHTTPAndStopsCleanly",
		"TestPublishNewMachineTakeoverRotatesEpochOnce",
		"TestRestartReconcilesAmbiguousAppliedPostWithoutReplay",
		"TestRestartSetupFailureAlwaysCompensatesAdvancedEpoch",
		"TestShareLifecycleCommandsConvergeCloudRegistryAndDaemon",
		"TestStartFailsImmediatelyWhenServingEpochAdvances",
	}
	lifecycleRegex := "^(" + strings.Join(lifecycleTests, "|") + ")$"
	lifecycleNames := strings.Join(lifecycleTests, "\n")
	lifecycleListCommand := `go test -race -list "$LIFECYCLE_TEST_REGEX" ./apps/cli/cmd`
	lifecycleRunCommand := `go test -race -count=1 -json -run "$LIFECYCLE_TEST_REGEX" ./apps/cli/cmd`
	lifecycleStep := findStep("test", "Run CRID lifecycle unit tests")
	assertRequiredGate("test", lifecycleStep)
	if len(lifecycleStep.Env) != 2 || lifecycleStep.Env["LIFECYCLE_TEST_REGEX"] != lifecycleRegex || lifecycleStep.Env["LIFECYCLE_TEST_NAMES"] != lifecycleNames {
		t.Errorf("public CLI workflow lifecycle env = %#v, want exact regex and sorted test names", lifecycleStep.Env)
	}
	for _, required := range []string{
		lifecycleListCommand,
		`if [[ "$actual_tests" != "$LIFECYCLE_TEST_NAMES" ]]; then`,
		lifecycleRunCommand,
		`jq -j 'select(.Output != null) | .Output'`,
		`lifecycle_status=0`,
		`select(.Action == "pass" and ((.Test // "") | test($regex)))`,
		`select(.Action == "skip" and ((.Test // "") | test($regex)))`,
		`if [[ -n "$skipped_tests" ]]; then`,
		`if [[ "$passed_tests" != "$LIFECYCLE_TEST_NAMES" ]]; then`,
	} {
		if strings.Count(lifecycleStep.Run, required) != 1 {
			t.Errorf("public CLI workflow does not pin the exact CRID lifecycle test set with %q", required)
		}
	}
	if strings.Count(lifecycleStep.Run, "LC_ALL=C sort") != 3 {
		t.Error("public CLI workflow does not sort declaration, PASS, and SKIP results under the C locale")
	}
	if strings.Index(lifecycleStep.Run, lifecycleListCommand) >= strings.Index(lifecycleStep.Run, lifecycleRunCommand) {
		t.Error("public CLI workflow does not verify CRID lifecycle test declarations before execution")
	}

	validatorTests := []string{
		"TestCanonicalSandboxFailureRootResolvesAlias",
		"TestReadSandboxSecretFileFailsClosed",
		"TestRunSandboxLocalCLIForwardsOnlyHardenedImageBinding",
		"TestRunSandboxLocalCLIUsesExactBinaryAndState",
		"TestSandboxDeletedCommandDiagnosticWithholdsChildOutput",
		"TestSandboxFailureChildEnvironmentUsesItsOwnOneTimeKey",
		"TestSandboxFailureDiagnosticsAreAllowListedAndRedacted",
		"TestSandboxForegroundLifecycleStateContract",
		"TestSandboxFullCustomerLifecyclePhaseContract",
		"TestSandboxGrantedRouteAccessFailureCategories",
		"TestSandboxGrantedRouteFenceValidator",
		"TestSandboxGrantedRouteLifetime",
		"TestSandboxGrantedRouteProbeAllowsCrossOriginWithoutSendingBearer",
		"TestSandboxGrantedRouteProbeAuthorizesEverySameOriginRequest",
		"TestSandboxGrantedRouteProbeRejectsMissingAuthorization",
		"TestSandboxGrantedRouteReadiness",
		"TestSandboxHarnessPassesInlineAPIKeyToExactBinary",
		"TestSandboxLocalStateReasonDoesNotForwardHostileLogText",
		"TestSandboxLocalStateReasonIsClosedAndUsesLatestCause",
		"TestSandboxNamespaceIsCanonicalAndSeparated",
		"TestSandboxProcessRecoveryCleanupAfterPreReadyFailure",
		"TestSandboxPublishProcessReportsEarlyExit",
		"TestSandboxPublishReadinessWaitsForCompleteCRIDLine",
		"TestSandboxResolveCommandDiagnosticWithholdsChildOutput",
		"TestSandboxResourceCleanupIsSafeBeforePublish",
		"TestSandboxRunIdentityBindsOnlyImmutableHardenedImage",
		"TestSandboxSiblingCleanupPreservesDeviceAfterResourceFailure",
		"TestSandboxStoppedRouteRefusalMatchesQuietGet",
		"TestValidateSandboxCLIBinary",
		"TestValidateSandboxDeviceIdentity",
		"TestValidateSandboxRouteFence",
		"TestValidateSandboxSharingTransitionRequiresAdvancedEpoch",
	}
	validatorPattern := "^(" + strings.Join(validatorTests, "|") + ")$"
	validatorNames := strings.Join(validatorTests, "\n")
	validatorStep := findStep("test", "Run credential-free journey validator tests")
	assertRequiredGate("test", validatorStep)
	if len(validatorStep.Env) != 2 || validatorStep.Env["VALIDATOR_TEST_REGEX"] != validatorPattern || validatorStep.Env["VALIDATOR_TEST_NAMES"] != validatorNames {
		t.Errorf("public CLI workflow validator env = %#v, want exact regex and sorted test names", validatorStep.Env)
	}
	validatorSkippedCheck := `if [[ -n "$skipped" ]]; then`
	validatorPassedCheck := `if [[ "$passed" != "$VALIDATOR_TEST_NAMES" ]]; then`
	validatorStatusCheck := `if (( validator_status != 0 )); then`
	for _, required := range []string{"go test -race -tags=clisandbox -list", "go test -race -tags=clisandbox -count=1 -json", `jq -j 'select(.Output != null) | .Output'`, `select(.Action == "pass"`, `select(.Action == "skip"`, validatorSkippedCheck, validatorPassedCheck, validatorStatusCheck} {
		if strings.Count(validatorStep.Run, required) != 1 {
			t.Errorf("public CLI workflow validator gate does not fail closed with %q", required)
		}
	}
	if strings.Index(validatorStep.Run, validatorSkippedCheck) >= strings.Index(validatorStep.Run, validatorStatusCheck) ||
		strings.Index(validatorStep.Run, validatorPassedCheck) >= strings.Index(validatorStep.Run, validatorStatusCheck) {
		t.Error("public CLI workflow validator gate checks process status before PASS/SKIP diagnostics")
	}

	windowsValidatorStep := findStep("matrix", "Run Windows credential-free journey validator tests")
	windowsValidatorJob := workflow.Jobs["matrix"]
	if windowsValidatorJob.If != expectedCLIJobIf || windowsValidatorJob.ContinueOnError != nil ||
		windowsValidatorStep.If != "runner.os == 'Windows'" || windowsValidatorStep.ContinueOnError != nil {
		t.Errorf("Windows credential-free validator is bypassable: job=%#v step=%#v", windowsValidatorJob, windowsValidatorStep)
	}
	if fmt.Sprint(windowsValidatorStep.Env["WINDOWS_VALIDATOR_TEST_REGEX"]) != "^TestReadWindowsSandboxLocalStateReasonKeepsClassifiedPrimaryLog$" ||
		fmt.Sprint(windowsValidatorStep.Env["WINDOWS_VALIDATOR_TEST_NAME"]) != "TestReadWindowsSandboxLocalStateReasonKeepsClassifiedPrimaryLog" {
		t.Errorf("Windows credential-free validator env = %#v, want one exact test", windowsValidatorStep.Env)
	}
	for _, required := range []string{
		"go test -tags=clisandbox -list $env:WINDOWS_VALIDATOR_TEST_REGEX",
		"go test -tags=clisandbox -count=1 -json -run $env:WINDOWS_VALIDATOR_TEST_REGEX",
		"$skipped.Count -ne 0 -or $passed.Count -ne 1",
	} {
		if strings.Count(windowsValidatorStep.Run, required) != 1 {
			t.Errorf("Windows credential-free validator does not fail closed with %q", required)
		}
	}

	warmDaemonStep := findStep("test", "Run exact warm-daemon process contract")
	assertRequiredGate("test", warmDaemonStep)
	if len(warmDaemonStep.Env) != 2 || warmDaemonStep.Env["WARM_DAEMON_TEST_REGEX"] != "^TestExactWarmDaemonProcessContract$" || warmDaemonStep.Env["WARM_DAEMON_TEST_NAME"] != "TestExactWarmDaemonProcessContract" {
		t.Errorf("public CLI workflow warm-daemon env = %#v, want one exact test", warmDaemonStep.Env)
	}
	warmDaemonSkippedCheck := `if [[ -n "$skipped" ]]; then`
	warmDaemonPassedCheck := `if [[ "$passed" != "$WARM_DAEMON_TEST_NAME" ]]; then`
	warmDaemonStatusCheck := `if (( warm_daemon_status != 0 )); then`
	for _, required := range []string{"go test -race -tags='clisandbox clisoak' -list", "go test -race -tags='clisandbox clisoak' -count=1 -json", `jq -j 'select(.Output != null) | .Output'`, `select(.Action == "pass"`, `select(.Action == "skip"`, warmDaemonSkippedCheck, warmDaemonPassedCheck, warmDaemonStatusCheck} {
		if strings.Count(warmDaemonStep.Run, required) != 1 {
			t.Errorf("public CLI workflow warm-daemon gate does not fail closed with %q", required)
		}
	}
	if strings.Index(warmDaemonStep.Run, warmDaemonSkippedCheck) >= strings.Index(warmDaemonStep.Run, warmDaemonStatusCheck) ||
		strings.Index(warmDaemonStep.Run, warmDaemonPassedCheck) >= strings.Index(warmDaemonStep.Run, warmDaemonStatusCheck) {
		t.Error("public CLI workflow warm-daemon gate checks process status before PASS/SKIP diagnostics")
	}
}

func TestReleaseNativeConnectionWorkflowsRequireExactTestResult(t *testing.T) {
	t.Parallel()
	type workflowTarget struct {
		file, job string
		release   bool
	}
	targets := []workflowTarget{
		{file: "release-please.yml", job: "release-cli", release: true},
		{file: "cli-nightly.yml", job: "snapshot"},
	}
	for _, target := range targets {
		data, err := os.ReadFile(filepath.Join(cliRepoRoot, ".github", "workflows", target.file))
		if err != nil {
			t.Fatal(err)
		}
		var workflow cliWorkflowContract
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatalf("parse %s: %v", target.file, err)
		}
		job, ok := workflow.Jobs[target.job]
		if !ok {
			t.Fatalf("%s has no %s job", target.file, target.job)
		}
		var relaySteps []cliWorkflowStep
		relayStepIndex := -1
		for index, candidate := range job.Steps {
			if candidate.Name == "Verify the production NHP session relay" {
				relaySteps = append(relaySteps, candidate)
				relayStepIndex = index
			}
		}
		if len(relaySteps) != 1 {
			t.Fatalf("%s has %d production session-relay steps, want one", target.file, len(relaySteps))
		}
		relayStep := relaySteps[0]
		if relayStep.If != nil || relayStep.ContinueOnError != nil || job.ContinueOnError != nil {
			t.Errorf("%s production session-relay gate is bypassable", target.file)
		}
		const relaySource = "${{ vars.QURL_PROD_NHP_SESSION_RELAY_URL }}"
		if got := fmt.Sprint(relayStep.Env["QURL_REQUIRE_RELEASE_SESSION_RELAY"]); got != "1" {
			t.Errorf("%s production session-relay requirement = %q, want 1", target.file, got)
		}
		if got := fmt.Sprint(relayStep.Env["QURL_RELEASE_SESSION_RELAY_URL"]); got != relaySource {
			t.Errorf("%s production session-relay source = %q, want repository variable", target.file, got)
		}
		for _, required := range []string{
			"go test ./apps/cli/internal/connector/sessionrelay -list",
			"go test ./apps/cli/internal/connector/sessionrelay -run",
			"-count=1 -json",
			`select(.Action == "pass" and .Test == $test)`,
			`select(.Action == "skip" and .Test == $test)`,
			`if [[ -n "$skipped" || "$passed" != "$test_name" ]]; then`,
			`if (( test_status != 0 )); then`,
		} {
			if strings.Count(relayStep.Run, required) != 1 {
				t.Errorf("%s production session-relay gate does not fail closed with %q", target.file, required)
			}
		}
		releaseBuilderName := "Build snapshot release"
		if target.release {
			releaseBuilderName = "Run GoReleaser"
		}
		var releaseBuilders []cliWorkflowStep
		releaseBuilderIndex := -1
		for index, candidate := range job.Steps {
			if candidate.Name == releaseBuilderName {
				releaseBuilders = append(releaseBuilders, candidate)
				releaseBuilderIndex = index
			}
		}
		if len(releaseBuilders) != 1 {
			t.Fatalf("%s has %d %q steps, want one", target.file, len(releaseBuilders), releaseBuilderName)
		}
		if got := fmt.Sprint(releaseBuilders[0].Env["QURL_RELEASE_SESSION_RELAY_URL"]); got != relaySource {
			t.Errorf("%s release builder does not consume the exact verified session-relay source", target.file)
		}
		if relayStepIndex >= releaseBuilderIndex {
			t.Errorf("%s production session-relay gate must run before the release builder (relay=%d builder=%d)", target.file, relayStepIndex, releaseBuilderIndex)
		}
		var matches []cliWorkflowStep
		pinStepIndex := -1
		for index, step := range job.Steps {
			if step.Name == "Verify the production NHP Hub trust pin" {
				matches = append(matches, step)
				pinStepIndex = index
			}
		}
		if len(matches) != 1 {
			t.Fatalf("%s has %d production Hub-pin steps, want one", target.file, len(matches))
		}
		step := matches[0]
		if step.If != nil || step.ContinueOnError != nil || job.ContinueOnError != nil {
			t.Errorf("%s production Hub-pin gate is bypassable", target.file)
		}
		for _, required := range []string{
			"go test ./apps/cli/internal/connector/hub -list",
			"go test ./apps/cli/internal/connector/hub -run",
			"-count=1 -json",
			`select(.Action == "pass" and .Test == $test)`,
			`select(.Action == "skip" and .Test == $test)`,
		} {
			if strings.Count(step.Run, required) != 1 {
				t.Errorf("%s production Hub-pin gate does not fail closed with %q", target.file, required)
			}
		}
		requiredMode, hasRequiredMode := step.Env["QURL_REQUIRE_RELEASE_HUB_PIN"]
		if target.release {
			releaseGate := strings.Join(strings.Fields(fmt.Sprint(job.If)), " ")
			const expectedReleaseGate = "!cancelled() && needs.cli-release-gate.result == 'success' && needs.cli-release-gate.outputs.required == 'true'" //nolint:misspell // GitHub spells this function cancelled().
			if releaseGate != expectedReleaseGate {
				t.Errorf("%s release-cli gate = %q, want %q", target.file, releaseGate, expectedReleaseGate)
			}
			if hasRequiredMode {
				t.Errorf("%s release Hub-pin step shadows the committed workflow mode with %v", target.file, requiredMode)
			}
			if mode := fmt.Sprint(workflow.Env["QURL_REQUIRE_RELEASE_HUB_PIN"]); mode != "0" {
				t.Errorf("%s release Hub-pin source mode = %q, want reviewed dark mode 0", target.file, mode)
			}
			for _, required := range []string{
				`if [[ -z "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" && -z "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256" ]]; then`,
				`if [[ "$QURL_REQUIRE_RELEASE_HUB_PIN" != 0 || "$skipped" != "$test_name" || -n "$passed" ]]; then`,
				`elif [[ -n "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" && -n "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256" ]]; then`,
				`if [[ -n "$skipped" || "$passed" != "$test_name" ]]; then`,
				`printf 'mode=%s\n' "$mode" >>"$GITHUB_OUTPUT"`,
			} {
				if strings.Count(step.Run, required) != 1 {
					t.Errorf("%s release Hub-pin gate does not pin dark/configured behavior with %q", target.file, required)
				}
			}
			pinSource, pinSourcePresent := step.Env["QURL_RELEASE_HUB_PUBLIC_KEY_B64"].(string)
			if !pinSourcePresent || strings.TrimSpace(pinSource) == "" {
				t.Errorf("%s release Hub-pin verifier has no key source", target.file)
			}
			var goreleaserSteps []cliWorkflowStep
			goreleaserStepIndex := -1
			for index, candidate := range job.Steps {
				if candidate.Name == "Run GoReleaser" {
					goreleaserSteps = append(goreleaserSteps, candidate)
					goreleaserStepIndex = index
				}
			}
			if len(goreleaserSteps) != 1 {
				t.Fatalf("%s has %d GoReleaser steps, want one", target.file, len(goreleaserSteps))
			}
			goreleaserPin, goreleaserPinPresent := goreleaserSteps[0].Env["QURL_RELEASE_HUB_PUBLIC_KEY_B64"].(string)
			if !goreleaserPinPresent || goreleaserPin != pinSource {
				t.Errorf("%s GoReleaser step does not consume the exact verified Hub-pin source", target.file)
			}
			if pinStepIndex >= goreleaserStepIndex {
				t.Errorf("%s production Hub-pin gate must run before GoReleaser (pin=%d goreleaser=%d)", target.file, pinStepIndex, goreleaserStepIndex)
			}
			var releaseVerifierSteps []cliWorkflowStep
			releaseVerifierStepIndex := -1
			for index, candidate := range job.Steps {
				if candidate.Name == "Verify the draft CLI trust posture" {
					releaseVerifierSteps = append(releaseVerifierSteps, candidate)
					releaseVerifierStepIndex = index
				}
			}
			if len(releaseVerifierSteps) != 1 {
				t.Fatalf("%s has %d draft CLI Hub-pin verifiers, want one", target.file, len(releaseVerifierSteps))
			}
			releaseVerifier := releaseVerifierSteps[0]
			if releaseVerifier.If != nil || releaseVerifier.ContinueOnError != nil || releaseVerifierStepIndex <= goreleaserStepIndex {
				t.Errorf("%s draft CLI Hub-pin verifier is bypassable or precedes GoReleaser", target.file)
			}
			fingerprintSource, ok := releaseVerifier.Env["QURL_RELEASE_HUB_PUBLIC_KEY_SHA256"].(string)
			if !ok || strings.TrimSpace(fingerprintSource) == "" {
				t.Errorf("%s released CLI Hub-pin verifier has no fingerprint source", target.file)
			}
			artifactPinSource, ok := releaseVerifier.Env["QURL_RELEASE_HUB_PUBLIC_KEY_B64"].(string)
			if !ok || strings.TrimSpace(artifactPinSource) == "" {
				t.Errorf("%s released CLI Hub-pin verifier has no public-key source", target.file)
			}
			artifactRelaySource, ok := releaseVerifier.Env["QURL_RELEASE_SESSION_RELAY_URL"].(string)
			if !ok || artifactRelaySource != relaySource {
				t.Errorf("%s released CLI verifier does not consume the exact reviewed session-relay source", target.file)
			}
			for _, required := range []string{
				`case "$QURL_RELEASE_HUB_PIN_MODE" in`,
				`[[ -z "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" && -z "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256" ]]`,
				`[[ -n "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" && -n "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256" ]]`,
				`gh release download "$CLI_TAG"`,
				`--pattern 'qurl_*_darwin_*.tar.gz'`,
				`--pattern 'qurl_*_linux_*.tar.gz'`,
				`--pattern 'qurl_*_windows_*.zip'`,
				`if (( ${#archives[@]} != ${#expected[@]} ))`,
				`[[ "$QURL_RELEASE_HUB_PIN_MODE" == pinned ]]`,
				`! grep -aFq -- "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" "$binary"`,
				`! grep -aFq -- "$QURL_RELEASE_SESSION_RELAY_URL" "$binary"`,
				`version --verify-release-native-trust`,
				`"$fingerprint" != "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256"`,
				`if "$native_binary" version --verify-release-native-trust >"$trust_stdout" 2>"$trust_stderr"; then`,
				`missing required built-in connection settings`,
			} {
				if !strings.Contains(releaseVerifier.Run, required) {
					t.Errorf("%s draft CLI Hub-pin verifier does not bind exact artifact behavior %q", target.file, required)
				}
			}
			var imageSmokeSteps []cliWorkflowStep
			for _, candidate := range job.Steps {
				if candidate.Name == "Smoke both qurl image platforms" {
					imageSmokeSteps = append(imageSmokeSteps, candidate)
				}
			}
			if len(imageSmokeSteps) != 1 {
				t.Fatalf("%s has %d qurl image trust verifiers, want one", target.file, len(imageSmokeSteps))
			}
			imageSmoke := imageSmokeSteps[0]
			if fmt.Sprint(imageSmoke.Env["QURL_RELEASE_HUB_PIN_MODE"]) != "${{ steps.release_hub_pin.outputs.mode }}" {
				t.Errorf("%s image smoke does not consume the validated Hub-pin mode", target.file)
			}
			if fmt.Sprint(imageSmoke.Env["QURL_RELEASE_SESSION_RELAY_URL"]) != relaySource {
				t.Errorf("%s image smoke does not consume the exact reviewed session-relay source", target.file)
			}
			for _, required := range []string{
				`for platform in linux/amd64 linux/arm64; do`,
				`container_id=$(docker create --platform "$platform" "$platform_candidate")`,
				`docker cp "$container_id:/usr/local/bin/qurl" "$binary"`,
				`! grep -aFq -- "$QURL_RELEASE_SESSION_RELAY_URL" "$binary"`,
				`if [[ "$QURL_RELEASE_HUB_PIN_MODE" == pinned ]]; then`,
				`version --verify-release-native-trust`,
				`missing required built-in connection settings`,
			} {
				if !strings.Contains(imageSmoke.Run, required) {
					t.Errorf("%s image smoke does not pin both trust postures with %q", target.file, required)
				}
			}
			if fmt.Sprint(job.Outputs["hub_pin_mode"]) != "${{ steps.release_hub_pin.outputs.mode }}" {
				t.Errorf("%s does not export the exact validated Hub-pin mode", target.file)
			}
			releaseJobText := fmt.Sprint(job)
			for _, forbidden := range []string{"QURL_SANDBOX", "QURL_CONNECTOR_HUB_", "openssl genpkey -algorithm X25519"} {
				if strings.Contains(releaseJobText, forbidden) {
					t.Errorf("%s release job contains forbidden non-production trust input %q", target.file, forbidden)
				}
			}
			signatureIndex, imageIndex, caskValidationIndex, caskStageIndex := -1, -1, -1, -1
			for index, candidate := range job.Steps {
				switch candidate.Name {
				case "Verify release signature (self-test)":
					signatureIndex = index
				case "Promote and sign tested qurl image", "Sign and promote the tested qurl image":
					imageIndex = index
				case "Validate generated Homebrew cask":
					caskValidationIndex = index
					for _, required := range []string{"dist/homebrew/Casks/qurl.rb", `releases/download/${CLI_TAG}/`, "generated Homebrew cask does not bind all four release archives"} {
						if strings.Count(candidate.Run, required) != 1 {
							t.Errorf("%s Homebrew pre-publication gate does not bind %q", target.file, required)
						}
					}
				case "Stage the Homebrew validation bundle":
					caskStageIndex = index
					if candidate.Uses != "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" {
						t.Errorf("%s stages Homebrew validation with an unpinned action", target.file)
					}
				}
			}
			if imageIndex >= 0 {
				t.Errorf("%s promotes the versioned qurl image before native package validation", target.file)
			}
			if caskValidationIndex <= releaseVerifierStepIndex || caskValidationIndex <= signatureIndex {
				t.Errorf("%s validates the generated cask before local artifact gates (cask=%d archive=%d signature=%d)", target.file, caskValidationIndex, releaseVerifierStepIndex, signatureIndex)
			}
			if caskStageIndex <= caskValidationIndex {
				t.Errorf("%s stages the Homebrew bundle before local validation (stage=%d validate=%d)", target.file, caskStageIndex, caskValidationIndex)
			}
			if _, present := goreleaserSteps[0].Env["HOMEBREW_TAP_GITHUB_TOKEN"]; present {
				t.Errorf("%s exposes the tap token to GoReleaser before verification", target.file)
			}
			validator, hasValidator := workflow.Jobs["validate-homebrew-cask"]
			publisher, hasPublisher := workflow.Jobs["publish-cli-release"]
			tapPublisher, hasTapPublisher := workflow.Jobs["publish-homebrew-cask"]
			if !hasValidator || !hasPublisher || !hasTapPublisher {
				t.Fatalf("%s has an incomplete Homebrew publication chain", target.file)
			}
			for _, required := range []string{`brew style --fix "$cask"`, `brew audit --cask "$token"`, `brew install --cask "$token"`} {
				if len(validator.Steps) == 0 || !strings.Contains(fmt.Sprint(validator.Steps), required) {
					t.Errorf("%s Homebrew validator does not bind %q", target.file, required)
				}
			}
			if !strings.Contains(fmt.Sprint(publisher.Steps), `gh release edit "$CLI_TAG"`) ||
				!strings.Contains(fmt.Sprint(publisher.Steps), "Sign and promote the tested qurl image") ||
				!strings.Contains(fmt.Sprint(tapPublisher.Steps), `gh api --method PUT "repos/${tap_repo}/contents/${cask_path}"`) {
				t.Errorf("%s publication jobs do not preserve release then tap publication", target.file)
			}
			continue
		}
		if hasRequiredMode {
			t.Errorf("%s dark nightly Hub-pin gate forces release mode", target.file)
		}
		for _, required := range []string{
			`if [[ -z "$QURL_RELEASE_HUB_PUBLIC_KEY_B64" && -z "$QURL_RELEASE_HUB_PUBLIC_KEY_SHA256" ]]; then`,
			`if [[ "$skipped" != "$test_name" || -n "$passed" ]]; then`,
			`elif [[ -n "$skipped" || "$passed" != "$test_name" ]]; then`,
		} {
			if strings.Count(step.Run, required) != 1 {
				t.Errorf("%s nightly Hub-pin gate does not pin dark and configured results with %q", target.file, required)
			}
		}
	}
}

func TestReleaseSignsAndVerifiesExactQURLImageDigest(t *testing.T) {
	t.Parallel()
	workflow, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "release-please.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(workflow), "\r\n", "\n")
	for _, want := range []string{
		"timeout-minutes: 40",
		`[ "$GITHUB_REF" = refs/heads/main ]`,
		"platforms: linux/amd64,linux/arm64",
		"HUB_TRUST_ROOT_B64=${{ secrets.QURL_PROD_NHP_HUB_PUBLIC_KEY_B64 }}",
		"SESSION_RELAY_URL=${{ vars.QURL_PROD_NHP_SESSION_RELAY_URL }}",
		"provenance: mode=max",
		"sbom: true",
		`candidate="${IMAGE_NAME}@${IMAGE_DIGEST}"`,
		"Resolve an existing released qurl image",
		"steps.qurl_existing.outputs.digest || steps.qurl_image.outputs.digest",
		`image_name="${QURL_IMAGE%@*}"`,
		`docker buildx imagetools inspect "$QURL_IMAGE" --raw`,
		`.platform.os == $os`,
		`.platform.architecture == $architecture`,
		`if length == 1 then .[0].digest`,
		`platform_candidate="${image_name}@${platform_digest}"`,
		`docker run --rm --platform "$platform" "$platform_candidate" version`,
		`docker run --rm --platform "$platform" "$platform_candidate" version --verify-release-native-trust`,
		`QURL_RELEASE_HUB_PUBLIC_KEY_SHA256: ${{ secrets.QURL_PROD_NHP_HUB_PUBLIC_KEY_SHA256 }}`,
		"scripts/extract-qurl-image-attestations.sh",
		"QURL_EXPECTED_VCS_SOURCE=https://github.com/layervai/qurl-integrations",
		`cosign sign --yes "$candidate"`,
		"https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1",
		"cosign verify-attestation --type https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1",
		`printf '%s\n' "$candidate" > dist/qurl-image.txt`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow missing qurl image trust contract %q", want)
		}
	}
	releaseCLI := strings.Index(text, "\n  release-cli:\n")
	if releaseCLI < 0 {
		t.Fatal("release workflow missing release-cli job")
	}
	releaseJob := text[releaseCLI:]
	branchGate := strings.Index(releaseJob, `name: Require the canonical release branch`)
	checkout := strings.Index(releaseJob, `uses: actions/checkout@`)
	goReleaser := strings.Index(releaseJob, `name: Run GoReleaser`)
	if branchGate < 0 || checkout < 0 || goReleaser < 0 || branchGate > checkout || branchGate > goReleaser {
		t.Errorf("release branch gate must run before checkout and GoReleaser (gate=%d checkout=%d goreleaser=%d)", branchGate, checkout, goReleaser)
	}
	if got := strings.Count(text, "outputs: type=image,name=ghcr.io/layervai/qurl,"); got != 1 {
		t.Errorf("release workflow publishes qurl customer image %d times, want exactly once", got)
	}
	if strings.Contains(text, `docker run --rm --platform "$platform" "$QURL_IMAGE" version`) {
		t.Error("release image smoke runs multiple platforms through one local index digest reference")
	}
	for _, forbidden := range []string{
		"ghcr.io/layervai/qurl-connector",
		"/usr/local/bin/qurl-connector",
		"QURL_EXPECTED_VCS_SOURCE=https://github.com/layervai/qurl-integrations.git",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("release workflow contains retired artifact %q", forbidden)
		}
	}
}

func TestPRImageSmokeRunsEachImmutablePlatformManifest(t *testing.T) {
	t.Parallel()
	workflow, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "cli.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(workflow), "\r\n", "\n")
	for _, want := range []string{
		"platforms: linux/amd64,linux/arm64",
		`candidate="localhost:5000/qurl@${IMAGE_DIGEST}"`,
		`docker buildx imagetools inspect "$candidate" --raw`,
		`.platform.os == $os`,
		`.platform.architecture == $architecture`,
		`if length == 1 then .[0].digest`,
		`platform_candidate="localhost:5000/qurl@${platform_digest}"`,
		`docker run --rm --platform "$platform" "$platform_candidate" version`,
		"QURL_EXPECTED_VCS_SOURCE=https://github.com/layervai/qurl-integrations",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("PR image smoke missing immutable platform contract %q", want)
		}
	}
	if strings.Contains(text, `docker run --rm --platform "$platform" "$candidate" version`) {
		t.Error("PR image smoke runs multiple platforms through one mutable local index reference")
	}
	if strings.Contains(text, "QURL_EXPECTED_VCS_SOURCE=https://github.com/layervai/qurl-integrations.git") {
		t.Error("PR image smoke expects a non-canonical BuildKit source URL")
	}
}

func TestReleaseDocsDescribeIndependentImageTrust(t *testing.T) {
	t.Parallel()
	docs, err := os.ReadFile(filepath.Join("..", "..", "..", "RELEASING.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(docs)), " ")
	for _, want := range []string{
		"`qurl-image.txt` is intentionally not in that manifest",
		"https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1",
		"Do not replace the digest from",
		"CLI releases have two reviewed Hub-trust postures",
		"A production-enabled release must contain the exact production Hub trust root",
		"A dark release must contain no production Hub trust root",
		"Every official release must also contain the exact reviewed production HTTPS session-relay origin",
		"`QURL_PROD_NHP_SESSION_RELAY_URL`",
		"all six native archives and both immutable OCI platform images",
		"changed session-relay value therefore requires a new CLI version",
		"fixed, redacted missing-settings error",
		"release process rejects partial trust data",
		"must never embed development or test trust data",
		"GitHub Release stays draft and the Homebrew tap stays on its prior version",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("RELEASING.md missing image trust guidance %q", want)
		}
	}
}
