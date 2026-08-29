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
	Jobs map[string]cliWorkflowJob `yaml:"jobs"`
}

type cliWorkflowJob struct {
	If              any               `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Steps           []cliWorkflowStep `yaml:"steps"`
}

type cliWorkflowStep struct {
	Name            string         `yaml:"name"`
	If              any            `yaml:"if"`
	ContinueOnError any            `yaml:"continue-on-error"`
	Env             map[string]any `yaml:"env"`
	Run             string         `yaml:"run"`
}

func validateRequiredCLIWorkflowGate(job cliWorkflowJob, step cliWorkflowStep, expectedJobIf string) error {
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
			err := validateRequiredCLIWorkflowGate(job, job.Steps[0], expectedJobIf)
			if (err != nil) != test.wantErr {
				t.Fatalf("workflow mutation error = %v, want error=%t", err, test.wantErr)
			}
		})
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

func TestCustomerSharingLiveLanesArePrivate(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Clean(cliRepoRoot)
	retiredWorkflows, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "cli-connector-resource-*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(retiredWorkflows) != 0 {
		t.Fatalf("retired credential-bearing workflows still exist: %v", retiredWorkflows)
	}

	for _, name := range []string{"cli.yml", "cli-nightly.yml"} {
		path := filepath.Join(repoRoot, ".github", "workflows", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"CLI_SANDBOX_E2E",
			"QURL_SANDBOX_API_KEY",
			"QURL_SANDBOX_CLEANUP_JWT",
			"QURL_CLI_SANDBOX_CONNECTOR",
			"-tags=clisoak",
		} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Errorf("public workflow %s retains private live-lane contract %q", name, forbidden)
			}
		}
	}
	cliWorkflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "cli.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cliWorkflow = bytes.ReplaceAll(cliWorkflow, []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(cliWorkflow, []byte("go test -race -tags=clisandbox -run '^$' -count=1 ./apps/cli/...")) {
		t.Error("public CLI workflow does not compile the private sandbox test surface")
	}
	var workflow cliWorkflowContract
	if err := yaml.Unmarshal(cliWorkflow, &workflow); err != nil {
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
		if err := validateRequiredCLIWorkflowGate(workflow.Jobs[jobName], step, expectedCLIJobIf); err != nil {
			t.Errorf("public CLI workflow %s / %s is bypassable: %v", jobName, step.Name, err)
		}
	}
	sandboxLintStep := findStep("lint", "golangci-lint sandbox tests")
	const sandboxLintCommand = "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --build-tags=clisandbox,clisoak --timeout=5m ./apps/cli/..."
	if sandboxLintStep.Run != sandboxLintCommand {
		t.Errorf("public CLI workflow sandbox lint command = %q, want %q", sandboxLintStep.Run, sandboxLintCommand)
	}
	assertRequiredGate("lint", sandboxLintStep)
	lifecycleTests := []string{
		"TestDaemonServesTwoResourcesAndStopsOneIndependently",
		"TestLocalPublishCompensatesSetupFailureBeforeDaemonOwnership",
		"TestPublishDaemonLifecycleServesRealHTTPAndStopsCleanly",
		"TestPublishNewMachineTakeoverRotatesEpochOnce",
		"TestRestartReconcilesAmbiguousAppliedPostWithoutReplay",
		"TestRestartSetupFailureAlwaysCompensatesAdvancedEpoch",
		"TestShareLifecycleCommandsConvergeCloudRegistryAndDaemon",
		"TestStartFailsImmediatelyWhenServingEpochAdvances",
		"TestStartRotatesEpochAfterLocalTerminalDisable",
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
		"TestReadSandboxSecretFileFailsClosed",
		"TestRunSandboxLocalCLIForwardsOnlyHardenedImageBinding",
		"TestRunSandboxLocalCLIUsesExactBinaryAndState",
		"TestSandboxForegroundLifecycleStateContract",
		"TestSandboxFullCustomerLifecyclePhaseContract",
		"TestSandboxHarnessPassesInlineAPIKeyToExactBinary",
		"TestSandboxNamespaceIsCanonicalAndSeparated",
		"TestSandboxProcessRecoveryCleanupAfterPreReadyFailure",
		"TestSandboxPublishProcessReportsEarlyExit",
		"TestSandboxPublishReadinessWaitsForCompleteCRIDLine",
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
	validatorStep := findStep("test", "Run credential-free sandbox validator tests")
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
	windowsTaskStep := findStep("matrix", "Run pinned Windows Task Scheduler integration")
	windowsPipeStep := findStep("matrix", "Run pinned Windows named-pipe owner integration")
	windowsCompileStep := findStep("matrix", "Compile private sandbox test surface on Windows")
	for _, step := range []cliWorkflowStep{windowsTaskStep, windowsPipeStep, windowsCompileStep} {
		if workflow.Jobs["matrix"].If != expectedCLIJobIf || workflow.Jobs["matrix"].ContinueOnError != nil ||
			step.If != "runner.os == 'Windows'" || step.ContinueOnError != nil {
			t.Errorf("public CLI workflow Windows gate %q is bypassable", step.Name)
		}
	}
	if got := windowsTaskStep.Env["QURL_WINDOWS_USER_JOB_INTEGRATION"]; got != "1" {
		t.Errorf("Windows Task Scheduler integration arming = %#v, want 1", got)
	}
	for _, required := range []string{
		"$testName = 'TestWindowsUserJobIntegration'",
		"$testPattern = '^TestWindowsUserJobIntegration$'",
		"go test -list $testPattern $testPackage",
		"go test -count=1 -json -run $testPattern $testPackage",
		"$_.Action -eq 'skip'",
		"$_.Action -eq 'pass'",
		"$skipped.Count -ne 0 -or $passed.Count -ne 1",
	} {
		if strings.Count(windowsTaskStep.Run, required) != 1 {
			t.Errorf("Windows Task Scheduler integration does not fail closed with %q", required)
		}
	}
	for _, required := range []string{
		"$testName = 'TestWindowsIPCServerReadinessReloadSecondDaemonAndShutdown'",
		"$testPattern = '^TestWindowsIPCServerReadinessReloadSecondDaemonAndShutdown$'",
		"go test -list $testPattern $testPackage",
		"go test -count=1 -json -run $testPattern $testPackage",
		"$_.Action -eq 'skip'",
		"$_.Action -eq 'pass'",
		"$skipped.Count -ne 0 -or $passed.Count -ne 1",
	} {
		if strings.Count(windowsPipeStep.Run, required) != 1 {
			t.Errorf("Windows named-pipe owner integration does not fail closed with %q", required)
		}
	}
	if !strings.Contains(windowsCompileStep.Run, "go test -tags=clisandbox -run '^$' -count=1 ./apps/cli/...") {
		t.Error("Windows matrix does not compile the private sandbox test surface")
	}

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"-tags=clisoak", "CLI_SANDBOX_E2E"} {
		if bytes.Contains(makefile, []byte(forbidden)) {
			t.Errorf("public Makefile retains private live-lane contract %q", forbidden)
		}
	}
	if !bytes.Contains(makefile, []byte("go test -tags=clisandbox -run '^$$' -count=1 ./apps/cli/...")) {
		t.Error("public Makefile does not compile the private sandbox test surface")
	}
}

func TestReleaseHubPinWorkflowsRequireExactTestResult(t *testing.T) {
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
			if !hasRequiredMode || requiredMode != "1" {
				t.Errorf("%s release Hub-pin gate is not required", target.file)
			}
			if strings.Count(step.Run, `if [[ -n "$skipped" || "$passed" != "$test_name" ]]; then`) != 1 {
				t.Errorf("%s release Hub-pin gate does not reject SKIP", target.file)
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
	text := string(docs)
	for _, want := range []string{
		"`qurl-image.txt` is intentionally not in that manifest",
		"https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1",
		"Do not replace the digest from",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("RELEASING.md missing image trust guidance %q", want)
		}
	}
}
