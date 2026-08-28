package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type cliWorkflowContract struct {
	Jobs map[string]struct {
		Steps []cliWorkflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

type cliWorkflowStep struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
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
	repoRoot := filepath.Clean("../../..")
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
	repoRoot := filepath.Clean("../../..")
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
	if !bytes.Contains(cliWorkflow, []byte("go test -tags=clisandbox -run '^$' -count=1 ./apps/cli/...")) {
		t.Error("public CLI workflow does not compile the private sandbox test surface")
	}
	var workflow cliWorkflowContract
	if err := yaml.Unmarshal(cliWorkflow, &workflow); err != nil {
		t.Fatalf("parse public CLI workflow: %v", err)
	}
	findStep := func(name string) cliWorkflowStep {
		t.Helper()
		var matches []cliWorkflowStep
		for _, step := range workflow.Jobs["test"].Steps {
			if step.Name == name {
				matches = append(matches, step)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("public CLI workflow test job has %d %q steps, want one", len(matches), name)
		}
		return matches[0]
	}
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
	lifecycleListCommand := `go test -list "$LIFECYCLE_TEST_REGEX" ./apps/cli/cmd`
	lifecycleRunCommand := `go test -race -count=1 -run "$LIFECYCLE_TEST_REGEX" ./apps/cli/cmd`
	lifecycleStep := findStep("Run CRID lifecycle unit tests")
	if len(lifecycleStep.Env) != 2 || lifecycleStep.Env["LIFECYCLE_TEST_REGEX"] != lifecycleRegex || lifecycleStep.Env["LIFECYCLE_TEST_NAMES"] != lifecycleNames {
		t.Errorf("public CLI workflow lifecycle env = %#v, want exact regex and sorted test names", lifecycleStep.Env)
	}
	for _, required := range []string{
		lifecycleListCommand,
		"LC_ALL=C sort",
		`if [[ "$actual_tests" != "$LIFECYCLE_TEST_NAMES" ]]; then`,
		lifecycleRunCommand,
	} {
		if strings.Count(lifecycleStep.Run, required) != 1 {
			t.Errorf("public CLI workflow does not pin the exact CRID lifecycle test set with %q", required)
		}
	}
	if strings.Index(lifecycleStep.Run, lifecycleListCommand) >= strings.Index(lifecycleStep.Run, lifecycleRunCommand) {
		t.Error("public CLI workflow does not verify CRID lifecycle test declarations before execution")
	}
	validatorTests := []string{
		"TestRunSandboxLocalCLIUsesExactBinaryAndState",
		"TestSandboxForegroundLifecycleStateContract",
		"TestValidateSandboxRouteFence",
		"TestValidateSandboxSharingTransitionRequiresAdvancedEpoch",
	}
	validatorRegex := "-run '^(" + strings.Join(validatorTests, "|") + ")$'"
	validatorStep := findStep("Run credential-free sandbox validator tests")
	if strings.Count(validatorStep.Run, validatorRegex) != 1 {
		t.Error("public CLI workflow does not execute the exact credential-free sandbox validator set")
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
