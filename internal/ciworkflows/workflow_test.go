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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	cliCustomerArtifactsJobID  = "customer-artifacts"

	workflowContractWorkflow = "workflow-contract.yml"
)

// TestCLICustomerJourneyArtifactsAreExactAndHermetic protects the untrusted
// producer boundary. Pull-request code can build the package, but it receives
// no standing authority and must publish one SHA-bound, fail-closed bundle.
func TestCLICustomerJourneyIsConsolidatedAndTrusted(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, cliWorkflow)
	for _, name := range []string{cliCustomerArtifactsJobID, "journey", "journey-cleanup", requiredJobID, "notify-soak-success", "notify-soak-manual-failure", "signal-cli-release"} {
		if workflow.Jobs[name] == nil {
			t.Fatalf("%s is missing %q", cliWorkflow, name)
		}
	}

	artifact := workflow.Jobs[cliCustomerArtifactsJobID]
	if artifact.If != "needs.changes.outputs.cli == 'true'" {
		t.Errorf("artifact producer if = %q", artifact.If)
	}
	assertJobPermissions(t, cliCustomerArtifactsJobID, artifact.Permissions, map[string]string{"contents": "read"})
	var identity, checkout, ephemeralPin, build, upload *step
	for index := range artifact.Steps {
		current := &artifact.Steps[index]
		switch {
		case current.Name == "Select the journey identity generation":
			identity = current
		case strings.HasPrefix(current.Uses, checkoutActionPrefix):
			checkout = current
		case current.Name == "Select ephemeral artifact trust root":
			ephemeralPin = current
		case current.Name == "Build exact packaged customer artifacts once":
			build = current
		case current.Name == "Upload exact packaged customer artifacts":
			upload = current
		}
	}
	if identity == nil || identity.ID != "identity" ||
		artifact.Outputs["identity_run_attempt"] != "${{ steps.identity.outputs.run_attempt }}" ||
		!strings.Contains(identity.Run, `echo "run_attempt=$GITHUB_RUN_ATTEMPT"`) {
		t.Errorf("artifact producer does not persist the exact journey identity attempt: step=%#v outputs=%#v", identity, artifact.Outputs)
	}
	const sourceSHA = "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"
	if checkout == nil || checkout.With["ref"] != sourceSHA || checkout.With["persist-credentials"] != false {
		t.Errorf("artifact checkout is not exact and credential-free: %#v", checkout)
	}
	if ephemeralPin == nil || ephemeralPin.If != "" ||
		!strings.Contains(ephemeralPin.Run, "openssl genpkey -algorithm X25519") ||
		!strings.Contains(ephemeralPin.Run, "QURL_RELEASE_HUB_PUBLIC_KEY_SHA256") ||
		strings.Contains(ephemeralPin.Run, "QURL_RELEASE_SESSION_RELAY_URL") ||
		strings.Contains(fmt.Sprint(ephemeralPin.Env)+ephemeralPin.Run, "secrets.") {
		t.Errorf("CI artifact Hub trust root is not fresh, public, and secret-free: %#v", ephemeralPin)
	}
	if build == nil || strings.Count(build.Run, "scripts/build-cli-customer-journey-artifacts.sh") != 1 ||
		strings.Contains(build.Run, "manifest") {
		t.Errorf("artifact build is not one receipt-free build: %#v", build)
	}
	if upload == nil || upload.Uses != "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" ||
		upload.With["if-no-files-found"] != "error" || upload.With["include-hidden-files"] != false ||
		!strings.Contains(fmt.Sprint(upload.With["name"]), sourceSHA) {
		t.Errorf("artifact upload is not exact and fail closed: %#v", upload)
	}

	raw := readWorkflowBytes(t, cliWorkflow)
	var cliConcurrency struct {
		Concurrency struct {
			Group string `yaml:"group"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(raw, &cliConcurrency); err != nil {
		t.Fatalf("decode CLI concurrency: %v", err)
	}
	wantConcurrency := "${{ github.workflow }}-${{ github.ref }}-${{ inputs.release_source_sha != '' && format('release-{0}', inputs.release_source_sha) || contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && 'soak' || 'ci' }}"
	if cliConcurrency.Concurrency.Group != wantConcurrency {
		t.Errorf("CLI concurrency group = %q, want separate main-CI and soak groups %q",
			cliConcurrency.Concurrency.Group, wantConcurrency)
	}
	for _, forbidden := range []string{
		"QURL_PROD_NHP_SESSION_RELAY_URL",
		"QURL_RELEASE_SESSION_RELAY_URL",
		"QURL_REQUIRE_RELEASE_SESSION_RELAY",
		"QURL_CONNECTOR_SESSION_RELAY_URL",
		"TestReleaseSessionRelayEnvironment",
		"sessionrelay.defaultURL",
		"SESSION_RELAY_URL",
	} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("%s retains forbidden Connector session-relay contract %q", cliWorkflow, forbidden)
		}
	}
	var contract struct {
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Environment string `yaml:"environment"`
			RunsOn      string `yaml:"runs-on"`
			Strategy    struct {
				FailFast *bool `yaml:"fail-fast"`
				Matrix   any   `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	journeyContract := contract.Jobs["journey"]
	if journeyContract.If != "(github.ref == 'refs/heads/main' || inputs.release_source_sha != '') && contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && needs.changes.outputs.cli == 'true' && needs.customer-artifacts.result == 'success'" ||
		journeyContract.Environment != "cli-customer-journey" || journeyContract.RunsOn != "${{ matrix.os }}" ||
		fmt.Sprint(journeyContract.Strategy.Matrix) != "${{ fromJSON(needs.changes.outputs.journey_matrix) }}" ||
		journeyContract.Strategy.FailFast == nil || *journeyContract.Strategy.FailFast {
		t.Errorf("journey is not a protected, parallel trusted-event matrix: %#v", journeyContract)
	}
	type journeyLane struct {
		Lane           string `json:"lane"`
		LaneID         int    `json:"lane_id"`
		OS             string `json:"os"`
		TestName       string `json:"test_name"`
		TimeoutMinutes int    `json:"timeout_minutes"`
		Soak           bool   `json:"soak"`
	}
	type journeyMatrix struct {
		Include []journeyLane `json:"include"`
	}
	changes := workflow.Jobs[changesJobID]
	var selector *step
	for index := range changes.Steps {
		if changes.Steps[index].Name == "Select protected customer journeys" {
			selector = &changes.Steps[index]
			break
		}
	}
	if selector == nil {
		t.Fatal("change detector has no protected customer-journey selector")
	}
	var baseLine, soakLine string
	for line := range strings.SplitSeq(selector.Run, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "matrix='{") {
			baseLine = line
		}
		if strings.Contains(line, ".include += [") {
			soakLine = line
		}
	}
	if baseLine == "" || soakLine == "" {
		t.Fatalf("journey selector has no exact base and soak matrices: %q", selector.Run)
	}
	var baseMatrix journeyMatrix
	baseJSON := strings.TrimSuffix(strings.TrimPrefix(baseLine, "matrix='"), "'")
	if err := json.Unmarshal([]byte(baseJSON), &baseMatrix); err != nil {
		t.Fatalf("parse base journey matrix: %v", err)
	}
	wantOS := map[string]string{"linux": "ubuntu-latest", "macos": "macos-latest", "windows": "windows-latest"}
	wantTests := map[string]string{
		"linux":   "TestSandboxLinuxDefaultDaemonLifecycle",
		"macos":   "TestSandboxMacOSDefaultDaemonLifecycle",
		"windows": "TestSandboxWindowsDefaultDaemonFullCustomerLifecycle",
	}
	seenIDs := map[int]bool{}
	for _, lane := range baseMatrix.Include {
		if wantOS[lane.Lane] != lane.OS || wantTests[lane.Lane] != lane.TestName ||
			lane.LaneID < 1 || seenIDs[lane.LaneID] || lane.TimeoutMinutes != 35 || lane.Soak {
			t.Errorf("journey lane is not isolated: %#v", lane)
		}
		seenIDs[lane.LaneID] = true
		delete(wantOS, lane.Lane)
	}
	if len(wantOS) != 0 || len(seenIDs) != 3 {
		t.Errorf("journey matrix is incomplete: missing=%v ids=%v", wantOS, seenIDs)
	}
	const soakPrefix = `matrix="$(jq -c '.include += [`
	const soakSuffix = `]' <<<"$matrix")"`
	soakJSON := strings.TrimSuffix(strings.TrimPrefix(soakLine, soakPrefix), soakSuffix)
	var soak journeyLane
	if err := json.Unmarshal([]byte(soakJSON), &soak); err != nil {
		t.Fatalf("parse scheduled soak lane: %v", err)
	}
	if soak != (journeyLane{
		Lane: "linux", LaneID: 4, OS: "ubuntu-latest", TestName: "TestSandboxLocalPublishSoak",
		TimeoutMinutes: 110, Soak: true,
	}) {
		t.Errorf("scheduled soak lane is not exact and isolated: %#v", soak)
	}
	baseLaneSpecs := make([]string, 0, len(baseMatrix.Include))
	for _, lane := range baseMatrix.Include {
		baseLaneSpecs = append(baseLaneSpecs, fmt.Sprintf("%s:%d:full", lane.Lane, lane.LaneID))
	}
	soakLaneSpec := fmt.Sprintf("%s:%d:soak", soak.Lane, soak.LaneID)
	primaryLaneSpecs := "lane_specs=(" + strings.Join(append(slices.Clone(baseLaneSpecs), soakLaneSpec), " ") + ")"
	if strings.Count(string(raw), primaryLaneSpecs) != 1 {
		t.Errorf("primary cleanup lane topology does not match the journey matrix: want %q", primaryLaneSpecs)
	}
	fallbackTopologySource := string(readWorkflowBytes(t, "qurl-cli-customer-cleanup.yml"))
	fallbackBaseLaneSpecs := "lane_specs=(" + strings.Join(baseLaneSpecs, " ") + ")"
	fallbackSoakLaneSpec := "lane_specs+=(" + soakLaneSpec + ")"
	if strings.Count(fallbackTopologySource, fallbackBaseLaneSpecs) != 1 ||
		strings.Count(fallbackTopologySource, fallbackSoakLaneSpec) != 1 {
		t.Errorf("fallback cleanup lane topology does not match the journey matrix: want %q and %q", fallbackBaseLaneSpecs, fallbackSoakLaneSpec)
	}
	watchdogRaw := string(readWorkflowBytes(t, "qurl-cli-soak-watchdog.yml"))
	watchdogCountMatch := regexp.MustCompile(`"\$expected_after" \d+ (\d+)`).FindStringSubmatch(watchdogRaw)
	watchdogCount := 0
	var watchdogCountErr error
	if len(watchdogCountMatch) == 2 {
		watchdogCount, watchdogCountErr = strconv.Atoi(watchdogCountMatch[1])
	}
	if len(watchdogCountMatch) != 2 || watchdogCountErr != nil || watchdogCount != len(baseMatrix.Include)+1 {
		t.Errorf("watchdog journey count = %v, want scheduled matrix size %d: %v", watchdogCountMatch, len(baseMatrix.Include)+1, watchdogCountErr)
	}
	releaseRaw := string(readWorkflowBytes(t, releasePleaseWorkflow))
	releaseCountMatch := regexp.MustCompile(`"\$CLI_RUN_ID" "\$CLI_RUN_ATTEMPT" (\d+)\)`).FindStringSubmatch(releaseRaw)
	releaseCount := 0
	var releaseCountErr error
	if len(releaseCountMatch) == 2 {
		releaseCount, releaseCountErr = strconv.Atoi(releaseCountMatch[1])
	}
	if len(releaseCountMatch) != 2 || releaseCountErr != nil || releaseCount != len(baseMatrix.Include)+1 {
		t.Errorf("release gate journey count = %v, want scheduled matrix size %d: %v", releaseCountMatch, len(baseMatrix.Include)+1, releaseCountErr)
	}

	journey := workflow.Jobs["journey"]
	if !slices.Contains(parseWorkflowNeeds(t, "journey", journey.Needs), cliCustomerArtifactsJobID) {
		t.Error("journey can run without the one artifact build")
	}
	journeySecretSources := map[string]string{
		"QURL_ENDPOINT":                   "${{ secrets.QURL_JOURNEY_ENDPOINT }}",
		"QURL_JOURNEY_QV2_ISSUER_KEY":     "${{ secrets.QURL_JOURNEY_QV2_ISSUER_KEY }}",
		"QURL_JOURNEY_QV2_RELAY_URL":      "${{ secrets.QURL_JOURNEY_QV2_RELAY_URL }}",
		"QURL_JOURNEY_HUB_HOST":           "${{ secrets.QURL_JOURNEY_HUB_HOST }}",
		"QURL_JOURNEY_HUB_PORT":           "${{ secrets.QURL_JOURNEY_HUB_PORT }}",
		"QURL_JOURNEY_HUB_PUBLIC_KEY_B64": "${{ secrets.QURL_JOURNEY_HUB_PUBLIC_KEY_B64 }}",
		"AUTH_TOKEN_ENDPOINT":             "${{ secrets.QURL_JOURNEY_AUTH_TOKEN_ENDPOINT }}",
	}
	for name, source := range journeySecretSources {
		if fmt.Sprint(journey.Env[name]) != source {
			t.Errorf("journey topology source %s = %q, want protected secret %q", name, journey.Env[name], source)
		}
	}
	mint, run, download := 0, 0, 0
	var posixMint, posixFence, windowsMint, windowsSelect, windowsRun, windowsKeyRemoval, windowsInstallCleanup *step
	for index := range journey.Steps {
		current := &journey.Steps[index]
		switch current.Name {
		case "Fence the POSIX background service":
			posixFence = current
		case "Select this runner's packaged artifact":
			if current.Shell == "pwsh" {
				windowsSelect = current
			}
		case "Mint this lane's disposable keys":
			mint++
			switch current.Shell {
			case "pwsh":
				windowsMint = current
			case "bash":
				posixMint = current
			}
			if fmt.Sprint(current.Env["AUTH_CLIENT_ID"]) != "${{ secrets.QURL_JOURNEY_AUTH_CLIENT_ID }}" ||
				fmt.Sprint(current.Env["AUTH_CLIENT_SECRET"]) != "${{ secrets.QURL_JOURNEY_AUTH_CLIENT_SECRET }}" {
				t.Errorf("journey does not use the one protected M2M authority: %#v", current.Env)
			}
		case "Run the packaged customer journey":
			run++
			if current.Shell == "pwsh" {
				windowsRun = current
			}
			encoded, err := yaml.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"QURL_JOURNEY_AUTH_CLIENT", "cleanup-jwt", "AUTH_CLIENT_SECRET"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("candidate step receives standing authority %q", forbidden)
				}
			}
		case "Remove the Windows customer installation":
			windowsInstallCleanup = current
		case "Remove the local disposable keys":
			windowsKeyRemoval = current
		}
		if strings.HasPrefix(current.Uses, "actions/download-artifact@") {
			download++
			if current.With["run-id"] != nil || current.With["repository"] != nil ||
				current.With["digest-mismatch"] != "error" ||
				fmt.Sprint(current.With["name"]) != "qurl-customer-journey-${{ github.sha }}-${{ needs.customer-artifacts.outputs.identity_run_attempt }}" {
				t.Errorf("journey artifact crosses a workflow or identity-attempt boundary, or skips digest validation: %#v", current)
			}
		}
		if strings.HasPrefix(current.Uses, "actions/upload-artifact@") {
			t.Errorf("credential-bearing journey uploads an artifact: %#v", current)
		}
	}
	if mint != 2 || run != 2 || download != 1 {
		t.Errorf("journey steps = mint %d, run %d, download %d", mint, run, download)
	}
	if posixMint == nil || windowsMint == nil {
		t.Fatalf("journey does not have exact POSIX and Windows mint steps: posix=%#v windows=%#v", posixMint, windowsMint)
	}
	if posixFence == nil ||
		!strings.Contains(posixFence.Run, "deadline=$((SECONDS + 15))") ||
		!strings.Contains(posixFence.Run, `while launchctl print "$service"`) ||
		!strings.Contains(posixFence.Run, "((SECONDS < deadline))") ||
		!strings.Contains(posixFence.Run, "qURL launchd service did not unload") ||
		!strings.Contains(posixFence.Run, "sleep 0.2") {
		t.Errorf("POSIX service fence does not wait for bounded launchd unload completion: %#v", posixFence)
	}
	for _, requiredText := range []string{
		"qurl-cli-ci-credentials.py create-pair",
		`--primary-output-dir "$credential_dir/primary"`,
		`--failure-output-dir "$credential_dir/failure"`,
		`[[ ! "$key" =~ ^lv_(live|test)_`,
		`! "$failure_key" =~ ^lv_(live|test)_`,
		`"$key" == "$failure_key"`,
		"disposable keys are malformed or not isolated",
		"QURL_CLI_SANDBOX_FAILURE_API_KEY_FILE=$key_dir/failure-api-key",
	} {
		if !strings.Contains(posixMint.Run, requiredText) {
			t.Errorf("POSIX mint step does not isolate both one-time keys: missing %q", requiredText)
		}
	}
	for _, requiredText := range []string{
		"qurl-cli-ci-credentials.py create-pair",
		"--primary-output-dir $primaryDir",
		"--failure-output-dir $failureDir",
		"$key -notmatch '^lv_(?:live|test)_'",
		"$failureKey -notmatch '^lv_(?:live|test)_'",
		"$key -eq $failureKey",
		"disposable keys are malformed or not isolated",
		"QURL_CLI_SANDBOX_FAILURE_API_KEY=$failureKey",
	} {
		if !strings.Contains(windowsMint.Run, requiredText) {
			t.Errorf("Windows mint step does not isolate both one-time keys: missing %q", requiredText)
		}
	}
	if windowsSelect == nil {
		t.Fatal("Windows journey selector is missing")
	}
	if !strings.Contains(windowsSelect.Run, `"-test.list=$listPattern"`) ||
		strings.Contains(windowsSelect.Run, "& $harness -test.list") {
		t.Errorf("Windows journey selector does not pass the dotted Go test flag as one native argument: %#v", windowsSelect)
	}
	if !strings.Contains(windowsSelect.Run, "$env:LOCALAPPDATA") ||
		!strings.Contains(windowsSelect.Run, "icacls.exe $installDir /inheritance:r /grant:r") ||
		!strings.Contains(windowsSelect.Run, "$qurl = Join-Path $installDir 'qurl.exe'") ||
		!strings.Contains(windowsSelect.Run, "Copy-Item -LiteralPath $artifactQurl -Destination $qurl") ||
		!strings.Contains(windowsSelect.Run, "$artifactHash -ne $installedHash") ||
		!strings.Contains(windowsSelect.Run, "$installedVersion -ne $expectedVersion") ||
		!strings.Contains(windowsSelect.Run, "QURL_CLI_SANDBOX_INSTALL_DIR=$installDir") ||
		!strings.Contains(windowsSelect.Run, "QURL_CLI_SANDBOX_BINARY=$qurl") ||
		!strings.Contains(windowsSelect.Run, "QURL_CLI_SANDBOX_QURL_BINARY=$qurl") {
		t.Errorf("Windows journey does not install and bind the exact artifact in a protected user path: %#v", windowsSelect)
	}
	recorded := strings.Index(windowsSelect.Run, "QURL_CLI_SANDBOX_INSTALL_DIR=$installDir")
	protected := strings.Index(windowsSelect.Run, "icacls.exe $installDir")
	if recorded < 0 || protected < 0 || recorded > protected {
		t.Error("Windows journey does not record its run-owned install directory before fallible protection and verification")
	}
	if windowsInstallCleanup == nil ||
		windowsInstallCleanup.If != "always() && runner.os == 'Windows' && steps.fence-windows-service.outcome == 'success'" ||
		!strings.Contains(windowsInstallCleanup.Run, "if ([string]::IsNullOrWhiteSpace($env:QURL_CLI_SANDBOX_INSTALL_DIR)) { return }") ||
		!strings.Contains(windowsInstallCleanup.Run, "QURL_CLI_SANDBOX_INSTALL_DIR -ne $expected") ||
		!strings.Contains(windowsInstallCleanup.Run, "Remove-Item -LiteralPath $expected -Recurse -Force") ||
		!strings.Contains(windowsInstallCleanup.Run, "Test-Path -LiteralPath $expected) { throw") {
		t.Errorf("Windows journey does not remove the exact test installation after fencing: %#v", windowsInstallCleanup)
	}
	if windowsInstallCleanup != nil {
		guarded := strings.Index(windowsInstallCleanup.Run, "if ([string]::IsNullOrWhiteSpace($env:QURL_CLI_SANDBOX_INSTALL_DIR)) { return }")
		derived := strings.Index(windowsInstallCleanup.Run, "$expected = Join-Path")
		if guarded < 0 || derived < 0 || guarded > derived {
			t.Error("Windows journey derives an install path before it proves one was recorded")
		}
	}
	if windowsRun == nil || !strings.Contains(windowsRun.Run, `@('-test.v=true', '-test.count=1', "-test.run=$testPattern")`) ||
		!strings.Contains(windowsRun.Run, "@testArgs 2>&1") || strings.Contains(windowsRun.Run, " -test.v ") {
		t.Errorf("Windows journey does not pass dotted Go test flags through a native argument array: %#v", windowsRun)
	}
	if !strings.Contains(fmt.Sprint(journey.Env["QURL_SHARING_RUN_ID"]), "matrix.lane_id") {
		t.Error("parallel lanes do not have distinct deterministic run IDs")
	}
	if fmt.Sprint(journey.Env["QURL_SHARING_RUN_ATTEMPT"]) != "${{ needs.customer-artifacts.outputs.identity_run_attempt }}" {
		t.Errorf("journey identity attempt is not the persisted producer value: %v", journey.Env["QURL_SHARING_RUN_ATTEMPT"])
	}
	if windowsKeyRemoval == nil || !strings.Contains(windowsKeyRemoval.Run, "QURL_API_KEY=") ||
		!strings.Contains(windowsKeyRemoval.Run, "QURL_CLI_SANDBOX_FAILURE_API_KEY=") {
		t.Errorf("Windows journey does not clear both disposable keys after fencing: %#v", windowsKeyRemoval)
	}

	cleanup := workflow.Jobs["journey-cleanup"]
	if cleanup.If != "always() && (github.ref == 'refs/heads/main' || inputs.release_source_sha != '') && contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && needs.changes.outputs.cli == 'true' && needs.customer-artifacts.result == 'success'" ||
		contract.Jobs["journey-cleanup"].Environment != "cli-customer-journey-cleanup" {
		t.Errorf("terminal cleanup is not an exact protected trusted-event gate: if=%q", cleanup.If)
	}
	if fmt.Sprint(cleanup.Env["AUTH_TOKEN_ENDPOINT"]) != "${{ secrets.QURL_JOURNEY_AUTH_TOKEN_ENDPOINT }}" {
		t.Errorf("terminal cleanup exposes its token endpoint through a non-secret source: %#v", cleanup.Env)
	}
	if needs := parseWorkflowNeeds(t, "journey-cleanup", cleanup.Needs); !slices.Equal(needs, []string{"changes", cliCustomerArtifactsJobID, "journey"}) {
		t.Errorf("terminal cleanup needs = %v, want the persisted journey identity producer", needs)
	}
	required := workflow.Jobs[requiredJobID]
	for _, needed := range []string{"journey", "journey-cleanup"} {
		if !slices.Contains(parseWorkflowNeeds(t, requiredJobID, required.Needs), needed) {
			t.Errorf("cli / required can pass without %s", needed)
		}
	}
	var verifyRequired *step
	for index := range required.Steps {
		if required.Steps[index].Name == "Verify CLI CI result" {
			verifyRequired = &required.Steps[index]
			break
		}
	}
	if verifyRequired == nil || fmt.Sprint(verifyRequired.Env["LIVE_REQUIRED"]) !=
		"${{ (github.ref == 'refs/heads/main' || inputs.release_source_sha != '') && contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) }}" {
		t.Errorf("required aggregate can demand or waive live jobs outside a trusted main or release run: %#v", verifyRequired)
	}

	notify := workflow.Jobs["notify-soak-success"]
	wantNotifyIf := "!cancelled() && github.ref == 'refs/heads/main' && contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && needs.required.result == 'success' && needs.journey.result == 'success'" //nolint:misspell // GitHub expression function spelling.
	if got := strings.Join(strings.Fields(notify.If), " "); got != wantNotifyIf {
		t.Errorf("soak success notification if = %q, want %q", got, wantNotifyIf)
	}
	if needs := parseWorkflowNeeds(t, "notify-soak-success", notify.Needs); !slices.Equal(needs, []string{requiredJobID, "journey"}) {
		t.Errorf("soak success notification needs = %v, want required and journey", needs)
	}
	if contract.Jobs["notify-soak-success"].Environment != "cli-customer-journey" {
		t.Errorf("soak success notification is not protected by the main-only journey environment")
	}
	assertJobPermissions(t, "notify-soak-success", notify.Permissions, map[string]string{"contents": "read"})
	var notifyCheckout, notifyPost *step
	for index := range notify.Steps {
		current := &notify.Steps[index]
		switch {
		case strings.HasPrefix(current.Uses, checkoutActionPrefix):
			notifyCheckout = current
		case current.Name == "Post verified soak success to Slack":
			notifyPost = current
		}
	}
	if notifyCheckout == nil || notifyCheckout.With["ref"] != "${{ github.sha }}" ||
		notifyCheckout.With["persist-credentials"] != false {
		t.Errorf("soak notifier checkout is not exact and credential-free: %#v", notifyCheckout)
	}
	if notifyPost == nil || strings.TrimSpace(notifyPost.Run) != "scripts/notify-qurl-cli-soak-status.sh" ||
		fmt.Sprint(notifyPost.Env["SLACK_WEBHOOK_URL"]) != "${{ secrets.SLACK_WEBHOOK_URL }}" {
		t.Errorf("soak notifier does not run the checked-in sender with the protected webhook: %#v", notifyPost)
	}
	if notifyPost == nil || fmt.Sprint(notifyPost.Env["SOAK_STATUS"]) != "success" ||
		fmt.Sprint(notifyPost.Env["SOAK_DURATION"]) != "80m" {
		t.Errorf("soak notifier does not bind the status and duration it reports: %#v", notifyPost)
	}

	manualFailure := workflow.Jobs["notify-soak-manual-failure"]
	wantManualFailureIf := "!cancelled() && github.ref == 'refs/heads/main' && github.event_name == 'workflow_dispatch' && (needs.required.result != 'success' || needs.journey.result != 'success' || needs.notify-soak-success.result != 'success')" //nolint:misspell // GitHub expression function spelling.
	if got := strings.Join(strings.Fields(manualFailure.If), " "); got != wantManualFailureIf {
		t.Errorf("manual soak failure notification if = %q, want %q", got, wantManualFailureIf)
	}
	if needs := parseWorkflowNeeds(t, "notify-soak-manual-failure", manualFailure.Needs); !slices.Equal(needs, []string{requiredJobID, "journey", "notify-soak-success"}) {
		t.Errorf("manual soak failure notification needs = %v, want required, journey, and success notifier", needs)
	}
	if contract.Jobs["notify-soak-manual-failure"].Environment != "cli-customer-journey" {
		t.Error("manual soak failure notification is not protected by the main-only journey environment")
	}
	assertJobPermissions(t, "notify-soak-manual-failure", manualFailure.Permissions, map[string]string{"contents": "read"})
	var manualFailureCheckout, manualFailurePost *step
	for index := range manualFailure.Steps {
		current := &manualFailure.Steps[index]
		switch {
		case strings.HasPrefix(current.Uses, checkoutActionPrefix):
			manualFailureCheckout = current
		case current.Name == "Post failed manual soak to Slack":
			manualFailurePost = current
		}
	}
	if manualFailureCheckout == nil || manualFailureCheckout.With["ref"] != "${{ github.sha }}" ||
		manualFailureCheckout.With["persist-credentials"] != false {
		t.Errorf("manual soak failure checkout is not exact and credential-free: %#v", manualFailureCheckout)
	}
	if manualFailurePost == nil || strings.TrimSpace(manualFailurePost.Run) != "scripts/notify-qurl-cli-soak-status.sh" ||
		fmt.Sprint(manualFailurePost.Env["SLACK_WEBHOOK_URL"]) != "${{ secrets.SLACK_WEBHOOK_URL }}" ||
		fmt.Sprint(manualFailurePost.Env["SOAK_STATUS"]) != "failure" ||
		fmt.Sprint(manualFailurePost.Env["TRIGGER"]) != "workflow_dispatch" ||
		fmt.Sprint(manualFailurePost.Env["SOAK_DURATION"]) != "80m" {
		t.Errorf("manual soak failure notifier is not exact: %#v", manualFailurePost)
	}

	fallback := readWorkflow(t, "qurl-cli-customer-cleanup.yml")
	if fallback.Jobs["resolve"] == nil || fallback.Jobs["cleanup"] == nil || len(fallback.Jobs) != 2 {
		t.Errorf("cancellation cleanup is not one small resolve/cleanup workflow: %v", maps.Keys(fallback.Jobs))
	}
	wantFallbackIf := "github.event_name == 'workflow_dispatch' || github.event.workflow_run.head_branch == 'main' || (github.event.workflow_run.event == 'workflow_dispatch' && startsWith(github.event.workflow_run.head_branch, 'v') && startsWith(github.event.workflow_run.display_title, 'CLI release gate '))"
	if got := strings.Join(strings.Fields(fallback.Jobs["resolve"].If), " "); got != wantFallbackIf {
		t.Errorf("cancellation cleanup resolve if = %q, want only manual, main, or release-tag runs", got)
	}
	fallbackSource := string(readWorkflowBytes(t, "qurl-cli-customer-cleanup.yml"))
	for _, forbidden := range []string{"actions/download-artifact", "actions/upload-artifact", "qurl-integrations-infra", "ops-routines"} {
		if strings.Contains(fallbackSource, forbidden) {
			t.Errorf("cancellation cleanup retains unnecessary coupling %q", forbidden)
		}
	}
	for _, source := range []string{string(raw), fallbackSource} {
		if strings.Contains(source, "vars.QURL_JOURNEY_") {
			t.Error("customer journey workflow exposes protected topology through an unmasked Actions variable")
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", ".github", "workflows", "qurl-cli-customer-journey.yml")); !os.IsNotExist(err) {
		t.Errorf("separate journey execution workflow still exists: %v", err)
	}
	for _, retired := range []string{
		"scripts/verify-cli-customer-journey-artifacts.py",
		"scripts/test-verify-cli-customer-journey-artifacts.py",
	} {
		if _, err := os.Stat(filepath.Join("..", "..", retired)); !os.IsNotExist(err) {
			t.Errorf("retired receipt verifier still exists at %s: %v", retired, err)
		}
	}

	builder := readRepoFile(t, "scripts/build-cli-customer-journey-artifacts.sh")
	for _, requiredText := range []string{
		"QURL_RELEASE_HUB_PUBLIC_KEY_SHA256",
		"release Hub public key does not match its SHA-256",
		`"vcs.modified": "false"`,
		`version --verify-release-native-trust`,
	} {
		if !strings.Contains(builder, requiredText) {
			t.Errorf("artifact builder is missing %q", requiredText)
		}
	}
	for _, forbidden := range []string{
		"manifest.json",
		"qurl_go_version",
		"connector_version",
		"QURL_RELEASE_SESSION_RELAY_URL",
		"QURL_REQUIRE_RELEASE_SESSION_RELAY",
		"TestReleaseSessionRelayEnvironment",
		"sessionrelay.defaultURL",
	} {
		if strings.Contains(builder, forbidden) {
			t.Errorf("artifact builder retains duplicate receipt/version source %q", forbidden)
		}
	}
	assertExecutableRepoScript(t, "scripts/build-cli-customer-journey-artifacts.sh")
}

func TestCLISoakWatchdogOwnsScheduledFreshness(t *testing.T) {
	t.Parallel()

	const watchdogWorkflow = "qurl-cli-soak-watchdog.yml"
	workflow := readWorkflow(t, watchdogWorkflow)
	if len(workflow.Jobs) != 2 || workflow.Jobs["freshness"] == nil || workflow.Jobs["notify-stale"] == nil {
		t.Fatalf("CLI soak watchdog jobs = %v, want freshness and notify-stale", slices.Sorted(maps.Keys(workflow.Jobs)))
	}
	raw := string(readWorkflowBytes(t, watchdogWorkflow))
	if strings.Contains(raw, "workflow_dispatch:") {
		t.Errorf("CLI soak watchdog is not one schedule-only daily check")
	}
	cliRaw := string(readWorkflowBytes(t, cliWorkflow))
	cronPattern := regexp.MustCompile(`(?m)^[ \t]+- cron: "(\d+) (\d+) \* \* \*"[ \t]*$`)
	cliCronMatches := cronPattern.FindAllStringSubmatch(cliRaw, -1)
	watchdogCronMatches := cronPattern.FindAllStringSubmatch(raw, -1)
	boundaryMatch := regexp.MustCompile(`expected_after=\$\(\(day_start \+ (\d+)\)\)`).FindStringSubmatch(raw)
	graceMatch := regexp.MustCompile(`"\$expected_after" (\d+) \d+`).FindStringSubmatch(raw)
	if len(cliCronMatches) != 1 || len(cliCronMatches[0]) != 3 || len(watchdogCronMatches) != 1 || len(watchdogCronMatches[0]) != 3 ||
		len(boundaryMatch) != 2 || len(graceMatch) != 2 {
		t.Fatalf("cannot bind CLI soak cron, watchdog cron, boundary, and grace: cli=%v watchdog=%v boundary=%v grace=%v",
			cliCronMatches, watchdogCronMatches, boundaryMatch, graceMatch)
	}
	cronMatch := cliCronMatches[0]
	minute, minuteErr := strconv.Atoi(cronMatch[1])
	hour, hourErr := strconv.Atoi(cronMatch[2])
	watchdogMinute, watchdogMinuteErr := strconv.Atoi(watchdogCronMatches[0][1])
	watchdogHour, watchdogHourErr := strconv.Atoi(watchdogCronMatches[0][2])
	boundary, boundaryErr := strconv.Atoi(boundaryMatch[1])
	activeGrace, activeGraceErr := strconv.Atoi(graceMatch[1])
	if minuteErr != nil || hourErr != nil || watchdogMinuteErr != nil || watchdogHourErr != nil ||
		boundaryErr != nil || activeGraceErr != nil || boundary != hour*3600+minute*60 {
		t.Errorf("watchdog cohort boundary = %d, want CLI cron %02d:%02d UTC (%d seconds)", boundary, hour, minute, hour*3600+minute*60)
	}
	cliStart := hour*3600 + minute*60
	watchdogStart := watchdogHour*3600 + watchdogMinute*60
	if watchdogStart-cliStart <= activeGrace {
		t.Errorf("watchdog cron %02d:%02d UTC must run after CLI cron %02d:%02d UTC plus %d-second active grace",
			watchdogHour, watchdogMinute, hour, minute, activeGrace)
	}

	freshness := workflow.Jobs["freshness"]
	if freshness.If != "github.ref == 'refs/heads/main'" {
		t.Errorf("watchdog freshness if = %q, want exact main ref", freshness.If)
	}
	assertJobPermissions(t, "freshness", freshness.Permissions, map[string]string{"actions": "read", "contents": "read"})
	var freshnessCheckout, freshnessCheck *step
	for index := range freshness.Steps {
		current := &freshness.Steps[index]
		switch {
		case strings.HasPrefix(current.Uses, checkoutActionPrefix):
			freshnessCheckout = current
		case current.Name == "Require today's successful scheduled soak":
			freshnessCheck = current
		}
	}
	if freshnessCheckout == nil || freshnessCheckout.With["ref"] != "${{ github.sha }}" ||
		freshnessCheckout.With["persist-credentials"] != false {
		t.Errorf("watchdog checkout is not exact and credential-free: %#v", freshnessCheckout)
	}
	if freshnessCheck == nil || freshnessCheck.ID != "evaluate" ||
		freshness.Outputs["stale"] != "${{ steps.evaluate.outputs.stale }}" {
		t.Errorf("watchdog freshness result is not exposed as a typed stale outcome: step=%#v outputs=%#v", freshnessCheck, freshness.Outputs)
	}
	for _, required := range []string{
		"actions/workflows/cli.yml/runs", "-f event=schedule", "-f per_page=100",
		"day_start=$((now_epoch - now_epoch % 86400))", "expected_after=$((day_start + 22620))",
		"if ((now_epoch < expected_after)); then", "expected_after=$((expected_after - 86400))",
		"actions/runs/${run_id}/attempts/${run_attempt}/jobs?per_page=100", "--paginate --slurp",
		`{total_count: .[0].total_count, jobs: [.[].jobs[]]}`,
		`scripts/qurl-cli-soak-watchdog.sh`, `"$RUNNER_TEMP/runs.json" "$jobs_dir" "$expected_after" 14400 4`,
	} {
		if freshnessCheck == nil || !strings.Contains(freshnessCheck.Run, required) {
			t.Errorf("watchdog freshness check is missing %q: %#v", required, freshnessCheck)
		}
	}
	watchdogScriptBytes, err := os.ReadFile(filepath.Join("..", "..", "scripts", "qurl-cli-soak-watchdog.sh"))
	if err != nil {
		t.Fatalf("read CLI soak watchdog script: %v", err)
	}
	watchdogScript := string(watchdogScriptBytes)
	cliContract := readWorkflow(t, cliWorkflow)
	for _, jobID := range []string{requiredJobID, "journey-cleanup"} {
		jobName := cliContract.Jobs[jobID].Name
		if jobName == "" || !strings.Contains(watchdogScript, fmt.Sprintf(".name == %q", jobName)) {
			t.Errorf("watchdog does not bind %s job name %q", jobID, jobName)
		}
	}
	journeyName := cliContract.Jobs["journey"].Name
	if journeyName == "" || !strings.Contains(watchdogScript, fmt.Sprintf("startswith(%q)", journeyName+" (")) {
		t.Errorf("watchdog does not bind journey job-name prefix %q", journeyName)
	}

	notify := workflow.Jobs["notify-stale"]
	if notify.If != "!cancelled() && github.ref == 'refs/heads/main' && needs.freshness.result == 'success' && needs.freshness.outputs.stale == 'true'" { //nolint:misspell // GitHub expression function spelling.
		t.Errorf("stale notification if = %q", notify.If)
	}
	if needs := parseWorkflowNeeds(t, "notify-stale", notify.Needs); !slices.Equal(needs, []string{"freshness"}) {
		t.Errorf("stale notification needs = %v, want freshness", needs)
	}
	assertJobPermissions(t, "notify-stale", notify.Permissions, map[string]string{"contents": "read"})
	var contract struct {
		Jobs map[string]struct {
			Environment string `yaml:"environment"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Jobs["notify-stale"].Environment != "cli-customer-journey" {
		t.Errorf("stale notification is not protected by the main-only journey environment")
	}
	var notifyPost *step
	for index := range notify.Steps {
		if notify.Steps[index].Name == "Post stale soak alert to Slack" {
			notifyPost = &notify.Steps[index]
			break
		}
	}
	if notifyPost == nil || strings.TrimSpace(notifyPost.Run) != "scripts/notify-qurl-cli-soak-status.sh" ||
		fmt.Sprint(notifyPost.Env["SOAK_STATUS"]) != "stale" ||
		fmt.Sprint(notifyPost.Env["SOAK_DURATION"]) != "80m" ||
		fmt.Sprint(notifyPost.Env["SLACK_WEBHOOK_URL"]) != "${{ secrets.SLACK_WEBHOOK_URL }}" {
		t.Errorf("stale notification does not bind the checked-in sender and protected webhook: %#v", notifyPost)
	}

	for _, script := range []string{
		"scripts/notify-qurl-cli-soak-status.sh",
		"scripts/qurl-cli-soak-watchdog.sh",
		"scripts/test-notify-qurl-cli-soak-status.sh",
		"scripts/test-qurl-cli-soak-watchdog.sh",
	} {
		assertExecutableRepoScript(t, script)
	}
}

func TestCLITerminalCleanupBatchesEveryLaneBeforeFailing(t *testing.T) {
	t.Parallel()

	workflows := []struct {
		name            string
		jobID           string
		stepName        string
		sourceRuns      string
		wantLanes       string
		wantInvocations string
		wantSoakLanes   string
		wantSoakCalls   string
	}{
		{
			name:            cliWorkflow,
			jobID:           "journey-cleanup",
			stepName:        "Revoke run resources and credentials",
			sourceRuns:      "700:2",
			wantLanes:       "linux\nmacos\nwindows\nlinux\n",
			wantInvocations: "7001:2:linux:full\n7002:2:macos:full\n7003:2:windows:full\n7004:2:linux:soak\n",
			wantSoakLanes:   "linux\nmacos\nwindows\nlinux\n",
			wantSoakCalls:   "7001:2:linux:full\n7002:2:macos:full\n7003:2:windows:full\n7004:2:linux:soak\n",
		},
		{
			name:            "qurl-cli-customer-cleanup.yml",
			jobID:           "cleanup",
			stepName:        "Revoke exact-run resources and credentials",
			sourceRuns:      "700:2,701:3",
			wantLanes:       "linux\nmacos\nwindows\nlinux\nmacos\nwindows\n",
			wantInvocations: "7001:2:linux:full\n7002:2:macos:full\n7003:2:windows:full\n7011:3:linux:full\n7012:3:macos:full\n7013:3:windows:full\n",
			wantSoakLanes:   "linux\nmacos\nwindows\nlinux\nlinux\nmacos\nwindows\nlinux\n",
			wantSoakCalls:   "7001:2:linux:full\n7002:2:macos:full\n7003:2:windows:full\n7004:2:linux:soak\n7011:3:linux:full\n7012:3:macos:full\n7013:3:windows:full\n7014:3:linux:soak\n",
		},
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const fakePython = `#!/usr/bin/env bash
set -euo pipefail
command_name=
run_specs=()
while (( $# > 0 )); do
  case "$1" in
    reconcile-batch) command_name=$1; shift ;;
    --run-spec) run_specs+=("$2"); shift 2 ;;
    *) shift ;;
  esac
done
[[ "$command_name" == reconcile-batch && ${#run_specs[@]} -gt 0 ]]
printf 'batch\n' >>"$BATCH_CAPTURE"
failed=0
for run_spec in "${run_specs[@]}"; do
  IFS=: read -r run_id run_attempt lane runtime profile <<<"$run_spec"
  [[ -n "$run_id" && -n "$run_attempt" && -n "$lane" && -n "$runtime" && -n "$profile" ]]
  printf '%s\n' "$lane" >>"$LANE_CAPTURE"
  printf '%s:%s:%s:%s\n' "$run_id" "$run_attempt" "$lane" "$profile" >>"$INVOCATION_CAPTURE"
  if [[ -n "${FAIL_LANE:-}" && "$lane" == "$FAIL_LANE" ]]; then
    failed=1
  fi
done
exit "$failed"
`

	for _, subject := range workflows {
		t.Run(subject.name, func(t *testing.T) {
			t.Parallel()

			workflow := readWorkflow(t, subject.name)
			job := workflow.Jobs[subject.jobID]
			if job == nil {
				t.Fatalf("%s is missing job %q", subject.name, subject.jobID)
			}
			var cleanup *step
			for index := range job.Steps {
				if job.Steps[index].Name == subject.stepName {
					cleanup = &job.Steps[index]
					break
				}
			}
			if cleanup == nil {
				t.Fatalf("%s is missing step %q", subject.name, subject.stepName)
			}

			tests := []struct {
				name        string
				failLane    string
				includeSoak bool
				wantFail    bool
			}{
				{name: "all lanes succeed"},
				{name: "first lane fails", failLane: "linux", wantFail: true},
				{name: "soak lane succeeds", includeSoak: true},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()

					fakeBin := t.TempDir()
					if err := os.WriteFile(filepath.Join(fakeBin, "python3"), []byte(fakePython), 0o700); err != nil { //nolint:gosec // Test-owned executable in t.TempDir.
						t.Fatal(err)
					}
					runnerTemp := t.TempDir()
					capture := filepath.Join(runnerTemp, "lanes")
					invocationCapture := filepath.Join(runnerTemp, "invocations")
					batchCapture := filepath.Join(runnerTemp, "batches")
					command := exec.CommandContext(t.Context(), "bash", "--noprofile", "--norc", "-c", cleanup.Run) //nolint:gosec // Executes the checked-in workflow step with a test-owned python3.
					command.Dir = repoRoot
					eventName := "push"
					includeSoak := "false"
					wantLanes := subject.wantLanes
					wantInvocations := subject.wantInvocations
					if test.includeSoak {
						eventName = "schedule"
						includeSoak = "true"
						wantLanes = subject.wantSoakLanes
						wantInvocations = subject.wantSoakCalls
					}
					command.Env = []string{
						"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
						"RUNNER_TEMP=" + runnerTemp,
						"LANE_CAPTURE=" + capture,
						"INVOCATION_CAPTURE=" + invocationCapture,
						"BATCH_CAPTURE=" + batchCapture,
						"FAIL_LANE=" + test.failLane,
						"AUTH_CLIENT_ID=test-client",
						"AUTH_CLIENT_SECRET=secret-value-must-not-print",
						"AUTH_TOKEN_ENDPOINT=https://auth.example",
						"QURL_ENDPOINT=https://sandbox.example",
						"GITHUB_RUN_ID=700",
						"GITHUB_RUN_ATTEMPT=2",
						"JOURNEY_RUN_ATTEMPT=2",
						"GITHUB_EVENT_NAME=" + eventName,
						"INCLUDE_SOAK=" + includeSoak,
						"SOURCE_RUN_ID=700",
						"SOURCE_RUN_ATTEMPT=2",
						"SOURCE_RUNS=" + subject.sourceRuns,
					}
					output, err := command.CombinedOutput()
					if gotFail := err != nil; gotFail != test.wantFail {
						t.Fatalf("cleanup error = %v, want failure %t: %s", err, test.wantFail, output)
					}
					lanes, readErr := os.ReadFile(capture) //nolint:gosec // Test-owned path under t.TempDir.
					if readErr != nil {
						t.Fatal(readErr)
					}
					if got, want := string(lanes), wantLanes; got != want {
						t.Errorf("attempted lanes = %q, want %q", got, want)
					}
					invocations, readErr := os.ReadFile(invocationCapture) //nolint:gosec // Test-owned path under t.TempDir.
					if readErr != nil {
						t.Fatal(readErr)
					}
					if got, want := string(invocations), wantInvocations; got != want {
						t.Errorf("cleanup invocations = %q, want %q", got, want)
					}
					batches, readErr := os.ReadFile(batchCapture) //nolint:gosec // Test-owned path under t.TempDir.
					if readErr != nil {
						t.Fatal(readErr)
					}
					if got, want := string(batches), "batch\n"; got != want {
						t.Errorf("cleanup command count = %q, want one batch", got)
					}
					text := string(output)
					if strings.Contains(text, "secret-value-must-not-print") {
						t.Error("cleanup output contains protected authority")
					}
					if !test.wantFail && strings.Contains(text, "::error::") {
						t.Errorf("successful cleanup reported an error: %s", output)
					}
				})
			}
		})
	}
}

func TestCLICancellationCleanupMatchesRenderedMatrixJobsAtExactSource(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, "qurl-cli-customer-cleanup.yml")
	resolve := workflow.Jobs["resolve"]
	cleanup := workflow.Jobs["cleanup"]
	if resolve == nil || cleanup == nil {
		t.Fatal("cancellation cleanup workflow is incomplete")
	}

	var resolverStep *step
	for index := range resolve.Steps {
		if resolve.Steps[index].Name == "Require an exact trusted journey run" {
			resolverStep = &resolve.Steps[index]
			break
		}
	}
	if resolverStep == nil || fmt.Sprint(resolverStep.Env["SOURCE_RUN_ATTEMPT"]) != "${{ github.event.workflow_run.run_attempt }}" {
		t.Fatalf("cleanup resolver is not bound to the source run attempt: %#v", resolverStep)
	}
	resolverRun := resolverStep.Run
	const jqPrefix = "journey_ran=$(jq -r '\n"
	const jqSuffix = "\n  ' <<<\"$identity_jobs\")"
	start := strings.Index(resolverRun, jqPrefix)
	if start < 0 {
		t.Fatal("cleanup resolver does not contain its required-job jq predicate")
	}
	start += len(jqPrefix)
	end := strings.Index(resolverRun[start:], jqSuffix)
	if end < 0 {
		t.Fatal("cleanup resolver jq predicate has an unexpected shape")
	}
	predicate := resolverRun[start : start+end]

	tests := []struct {
		name    string
		jobName string
		want    string
	}{
		{
			name:    "rendered matrix lane",
			jobName: "cli / customer journey (linux, 1, ubuntu-latest, TestSandboxLinuxDefaultDaemonLifecycle)",
			want:    "true",
		},
		{name: "unsuffixed job", jobName: "cli / customer journey", want: "true"},
		{name: "retired slash suffix", jobName: "cli / customer journey / linux", want: "false"},
		{name: "adjacent cleanup job", jobName: "cli / customer journey cleanup", want: "false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture, err := json.Marshal(map[string]any{
				"total_count": 1,
				"jobs":        []map[string]string{{"name": test.jobName, "conclusion": "success"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "jq", "-r", predicate) //nolint:gosec // Fixed executable and workflow-owned predicate.
			command.Stdin = strings.NewReader(string(fixture))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("execute cleanup resolver predicate: %v: %s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != test.want {
				t.Errorf("cleanup predicate for %q = %q, want %q", test.jobName, got, test.want)
			}
		})
	}

	binDir := t.TempDir()
	mockGH := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$GH_CAPTURE"
if [[ "$*" == *"/jobs?filter=all&per_page=100"* ]]; then
  artifact_attempt=${MOCK_ARTIFACT_ATTEMPT:-$EXPECTED_ATTEMPT}
  jq -n --arg name "$MOCK_JOB_NAME" --arg conclusion "$MOCK_JOB_CONCLUSION" \
    --argjson artifact_attempt "$artifact_attempt" '
    [{jobs:[
      {name:"cli / customer journey artifacts",conclusion:"success",run_attempt:$artifact_attempt},
      {name:$name,conclusion:$conclusion,run_attempt:$artifact_attempt}
    ]} | .total_count = (.jobs | length)]'
elif [[ "$*" == *"/attempts/"*"/jobs?per_page=100" ]]; then
  if [[ -n "${MOCK_CLEANUP_CONCLUSION:-}" ]]; then
    jq -n --arg name "$MOCK_JOB_NAME" --arg conclusion "$MOCK_JOB_CONCLUSION" \
      --arg cleanup_conclusion "$MOCK_CLEANUP_CONCLUSION" '
      {jobs:[{name:$name,conclusion:$conclusion},
         {name:"cli / customer journey cleanup",conclusion:$cleanup_conclusion}]} |
      .total_count = (.jobs | length)'
  elif [[ "$MOCK_JOB_CONCLUSION" == __NULL__ ]]; then
    jq -n --arg name "$MOCK_JOB_NAME" '
      {jobs:[{name:$name,conclusion:null}]} | .total_count = (.jobs | length)'
  else
    jq -n --arg name "$MOCK_JOB_NAME" --arg conclusion "$MOCK_JOB_CONCLUSION" '
      {jobs:[{name:$name,conclusion:$conclusion}]} | .total_count = (.jobs | length)'
  fi
else
  jq -n \
    --arg status "$MOCK_RUN_STATUS" \
    --arg event "$MOCK_RUN_EVENT" \
    --arg branch "$MOCK_RUN_BRANCH" \
    --arg repository "$MOCK_RUN_REPOSITORY" \
    --arg name "$MOCK_RUN_NAME" \
    --arg path "$MOCK_RUN_PATH" \
    --arg run_attempt "$MOCK_RUN_ATTEMPT" \
    --arg sha "$MOCK_RUN_SHA" \
    --arg display_title "$MOCK_RUN_DISPLAY_TITLE" \
    '{status:$status,event:$event,head_branch:$branch,head_sha:$sha,
      display_title:$display_title,head_repository:{full_name:$repository},name:$name,path:$path,
      run_attempt:($run_attempt | tonumber)}'
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(mockGH), 0o700); err != nil { //nolint:gosec // Test-owned executable in t.TempDir.
		t.Fatal(err)
	}
	runResolver := func(overrides map[string]string) (workflowOutput, commandOutput, ghArguments string, runErr error) {
		t.Helper()
		runDir := t.TempDir()
		capturePath := filepath.Join(runDir, "gh-arguments")
		outputPath := filepath.Join(runDir, "workflow-output")
		command := exec.CommandContext(t.Context(), "bash", "-c", resolverRun) //nolint:gosec // Executes the repository-owned fixed workflow step.
		env := map[string]string{
			"PATH":                    binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GH_CAPTURE":              capturePath,
			"EXPECTED_ATTEMPT":        "2",
			"GITHUB_OUTPUT":           outputPath,
			"GITHUB_REPOSITORY":       "layervai/qurl-integrations",
			"GITHUB_EVENT_NAME":       "workflow_run",
			"GITHUB_REF":              "refs/heads/main",
			"REQUESTED_SOURCE_RUNS":   "",
			"SOURCE_BRANCH":           "main",
			"SOURCE_EVENT":            "push",
			"SOURCE_DISPLAY_TITLE":    "CLI push",
			"SOURCE_REPOSITORY":       "layervai/qurl-integrations",
			"SOURCE_RUN_ID":           "700",
			"SOURCE_RUN_ATTEMPT":      "2",
			"SOURCE_SHA":              "0123456789abcdef0123456789abcdef01234567",
			"SOURCE_WORKFLOW_NAME":    "cli: Build and Test",
			"SOURCE_WORKFLOW_PATH":    ".github/workflows/cli.yml",
			"WORKFLOW_REPOSITORY":     "layervai/qurl-integrations",
			"MOCK_JOB_NAME":           "cli / customer journey (linux, 1, ubuntu-latest, TestSandboxLinuxDefaultDaemonLifecycle, 35, false)",
			"MOCK_JOB_CONCLUSION":     "success",
			"MOCK_CLEANUP_CONCLUSION": "",
			"MOCK_RUN_STATUS":         "completed",
			"MOCK_RUN_EVENT":          "push",
			"MOCK_RUN_BRANCH":         "main",
			"MOCK_RUN_REPOSITORY":     "layervai/qurl-integrations",
			"MOCK_RUN_NAME":           "cli: Build and Test",
			"MOCK_RUN_PATH":           ".github/workflows/cli.yml",
			"MOCK_RUN_ATTEMPT":        "2",
			"MOCK_RUN_SHA":            "0123456789abcdef0123456789abcdef01234567",
			"MOCK_RUN_DISPLAY_TITLE":  "CLI push",
		}
		for key, value := range overrides {
			env[key] = value
		}
		command.Env = environmentWithOverrides(os.Environ(), env)
		output, err := command.CombinedOutput()
		workflowBytes, workflowErr := os.ReadFile(outputPath) //nolint:gosec // Test-owned path under t.TempDir.
		if workflowErr != nil && !os.IsNotExist(workflowErr) {
			t.Fatal(workflowErr)
		}
		captureBytes, captureErr := os.ReadFile(capturePath) //nolint:gosec // Test-owned path under t.TempDir.
		if captureErr != nil && !os.IsNotExist(captureErr) {
			t.Fatal(captureErr)
		}
		return string(workflowBytes), string(output), string(captureBytes), err
	}
	workflowOutput, output, ghArguments, err := runResolver(nil)
	if err != nil {
		t.Fatalf("execute attempt-bound cleanup resolver: %v: %s", err, output)
	}
	if strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=false" {
		t.Fatalf("attempt-bound cleanup result = %q, want required=true and exact source_runs", workflowOutput)
	}
	if !strings.Contains(ghArguments, "/actions/runs/700/attempts/2/jobs?per_page=100") ||
		!strings.Contains(ghArguments, "/actions/runs/700/jobs?filter=all&per_page=100") {
		t.Errorf("cleanup queried a different run attempt: %s", ghArguments)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"SOURCE_EVENT": "schedule"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=true" {
		t.Fatalf("scheduled cleanup source omitted soak lane: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	releaseSHA := "0123456789abcdef0123456789abcdef01234567"
	workflowOutput, output, _, err = runResolver(map[string]string{
		"SOURCE_BRANCH":        "v1.2.3",
		"SOURCE_EVENT":         "workflow_dispatch",
		"SOURCE_DISPLAY_TITLE": "CLI release gate " + releaseSHA,
		"SOURCE_SHA":           releaseSHA,
	})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=true" {
		t.Fatalf("release-tag cleanup source was rejected: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"MOCK_JOB_NAME": "cli / lint"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=false" {
		t.Fatalf("automatic non-journey source did not skip cleanly: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"MOCK_JOB_CONCLUSION": "skipped"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=false" {
		t.Fatalf("automatic skipped journey did not skip cleanly: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"MOCK_JOB_CONCLUSION": "__NULL__"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=false" {
		t.Fatalf("automatic unsettled journey did not bias toward cleanup: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, ghArguments, err = runResolver(map[string]string{"MOCK_ARTIFACT_ATTEMPT": "1"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:1\ninclude_soak=false" {
		t.Fatalf("rerun cleanup did not resolve the persisted identity attempt: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	if strings.Contains(ghArguments, "/actions/runs/700/attempts/1/jobs?per_page=100") {
		t.Errorf("rerun cleanup inferred identity by walking prior attempts: %s", ghArguments)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"MOCK_CLEANUP_CONCLUSION": "success"})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=false" {
		t.Fatalf("automatic source with successful primary cleanup did not skip fallback: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{"SOURCE_RUN_ATTEMPT": "31"})
	if err == nil || strings.TrimSpace(workflowOutput) != "" || !strings.Contains(output, "source run attempt is outside the 1-30 recovery bound") {
		t.Fatalf("automatic over-bound attempt did not fail loudly: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}

	for _, test := range []struct {
		name        string
		overrides   map[string]string
		wantMessage string
	}{
		{name: "wrong automatic branch", overrides: map[string]string{"SOURCE_BRANCH": "feature"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "wrong automatic event", overrides: map[string]string{"SOURCE_EVENT": "pull_request"}, wantMessage: "source event is not an approved CLI trigger"},
		{name: "spoofed release title", overrides: map[string]string{"SOURCE_BRANCH": "v1.2.3", "SOURCE_EVENT": "workflow_dispatch", "SOURCE_DISPLAY_TITLE": "Operator CLI soak"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "release title on branch", overrides: map[string]string{"SOURCE_BRANCH": "feature", "SOURCE_EVENT": "workflow_dispatch", "SOURCE_DISPLAY_TITLE": "CLI release gate 0123456789abcdef0123456789abcdef01234567"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "wrong automatic source repository", overrides: map[string]string{"SOURCE_REPOSITORY": "other/repo"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "wrong automatic workflow repository", overrides: map[string]string{"WORKFLOW_REPOSITORY": "other/repo"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "wrong automatic workflow name", overrides: map[string]string{"SOURCE_WORKFLOW_NAME": "other"}, wantMessage: "exact trusted same-repository CLI workflow"},
		{name: "wrong automatic workflow path", overrides: map[string]string{"SOURCE_WORKFLOW_PATH": ".github/workflows/other.yml"}, wantMessage: "exact trusted same-repository CLI workflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, output, _, err := runResolver(test.overrides)
			if err == nil || !strings.Contains(output, test.wantMessage) {
				t.Fatalf("resolver accepted an invalid automatic source: err=%v output=%s", err, output)
			}
		})
	}

	manualSuccess := map[string]string{
		"GITHUB_EVENT_NAME":     "workflow_dispatch",
		"REQUESTED_SOURCE_RUNS": "700:2",
	}
	workflowOutput, output, ghArguments, err = runResolver(manualSuccess)
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=false" {
		t.Fatalf("exact manual cleanup source was rejected: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{
		"GITHUB_EVENT_NAME":     "workflow_dispatch",
		"REQUESTED_SOURCE_RUNS": "700:2",
		"MOCK_RUN_EVENT":        "schedule",
	})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=true" {
		t.Fatalf("manual scheduled cleanup source omitted soak lane: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	workflowOutput, output, _, err = runResolver(map[string]string{
		"GITHUB_EVENT_NAME":      "workflow_dispatch",
		"REQUESTED_SOURCE_RUNS":  "700:2",
		"MOCK_RUN_EVENT":         "workflow_dispatch",
		"MOCK_RUN_BRANCH":        "v1.2.3",
		"MOCK_RUN_DISPLAY_TITLE": "CLI release gate 0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2\ninclude_soak=true" {
		t.Fatalf("manual release-tag cleanup source was rejected: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}
	for _, want := range []string{
		"/actions/runs/700\n",
		"/actions/runs/700/attempts/2/jobs?per_page=100",
	} {
		if !strings.Contains(ghArguments, want) {
			t.Errorf("manual cleanup did not query %q: %s", want, ghArguments)
		}
	}
	exactCap := map[string]string{
		"GITHUB_EVENT_NAME":     "workflow_dispatch",
		"REQUESTED_SOURCE_RUNS": "700:2,701:2,702:2",
	}
	workflowOutput, output, _, err = runResolver(exactCap)
	if err != nil || strings.TrimSpace(workflowOutput) != "required=true\nsource_runs=700:2,701:2,702:2\ninclude_soak=false" {
		t.Fatalf("three-source manual cleanup was rejected: err=%v output=%s workflow_output=%q", err, output, workflowOutput)
	}

	for _, test := range []struct {
		name        string
		overrides   map[string]string
		wantMessage string
	}{
		{
			name:        "wrong dispatch ref",
			overrides:   map[string]string{"GITHUB_REF": "refs/heads/feature"},
			wantMessage: "manual cleanup must run from main",
		},
		{
			name:        "malformed input",
			overrides:   map[string]string{"REQUESTED_SOURCE_RUNS": "700:0"},
			wantMessage: "source runs are malformed",
		},
		{
			name:        "run id leaves no lane suffix room",
			overrides:   map[string]string{"REQUESTED_SOURCE_RUNS": "12345678901234567890:1"},
			wantMessage: "source runs are malformed",
		},
		{
			name:        "duplicate input",
			overrides:   map[string]string{"REQUESTED_SOURCE_RUNS": "700:2,700:2"},
			wantMessage: "contain a duplicate",
		},
		{
			name:        "more than three inputs",
			overrides:   map[string]string{"REQUESTED_SOURCE_RUNS": "1:1,2:1,3:1,4:1"},
			wantMessage: "exceed the 3-run limit",
		},
		{
			name:        "wrong run repository",
			overrides:   map[string]string{"MOCK_RUN_REPOSITORY": "other/repo"},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name:        "wrong run event",
			overrides:   map[string]string{"MOCK_RUN_EVENT": "pull_request"},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name:        "unfinished run",
			overrides:   map[string]string{"MOCK_RUN_STATUS": "in_progress"},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name:        "wrong run branch",
			overrides:   map[string]string{"MOCK_RUN_BRANCH": "feature"},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name: "schedule with release-looking ref",
			overrides: map[string]string{
				"MOCK_RUN_EVENT":         "schedule",
				"MOCK_RUN_BRANCH":        "v1.2.3",
				"MOCK_RUN_DISPLAY_TITLE": "CLI release gate 0123456789abcdef0123456789abcdef01234567",
			},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name:        "unrelated main workflow",
			overrides:   map[string]string{"MOCK_RUN_PATH": ".github/workflows/other.yml", "MOCK_RUN_NAME": "cli: Build and Test"},
			wantMessage: "exact trusted same-repository CLI workflow",
		},
		{
			name:        "source without journey",
			overrides:   map[string]string{"MOCK_JOB_NAME": "cli / lint"},
			wantMessage: "source did not run the customer journey",
		},
		{
			name:        "skipped journey",
			overrides:   map[string]string{"MOCK_JOB_CONCLUSION": "skipped"},
			wantMessage: "source did not run the customer journey",
		},
	} {
		t.Run("manual "+test.name, func(t *testing.T) {
			overrides := map[string]string{}
			for key, value := range manualSuccess {
				overrides[key] = value
			}
			for key, value := range test.overrides {
				overrides[key] = value
			}
			_, output, _, err := runResolver(overrides)
			if err == nil || !strings.Contains(output, test.wantMessage) {
				t.Fatalf("resolver accepted invalid manual source: err=%v output=%s", err, output)
			}
		})
	}

	var checkout *step
	for index := range cleanup.Steps {
		if strings.HasPrefix(cleanup.Steps[index].Uses, checkoutActionPrefix) {
			checkout = &cleanup.Steps[index]
			break
		}
	}
	if checkout == nil || checkout.With["ref"] != "${{ github.event_name == 'workflow_run' && github.event.workflow_run.head_sha || github.sha }}" ||
		checkout.With["persist-credentials"] != false {
		t.Errorf("cancellation cleanup checkout is not exact and credential-free: %#v", checkout)
	}
	var cleanupStep *step
	for index := range cleanup.Steps {
		if cleanup.Steps[index].Name == "Revoke exact-run resources and credentials" {
			cleanupStep = &cleanup.Steps[index]
			break
		}
	}
	if cleanupStep == nil || fmt.Sprint(cleanupStep.Env["SOURCE_RUNS"]) != "${{ needs.resolve.outputs.source_runs }}" ||
		strings.Contains(cleanupStep.Run, "${{ needs.resolve.outputs.source_runs }}") {
		t.Errorf("cleanup source runs are not passed through a sanitized step environment value: %#v", cleanupStep)
	}
	if timeout, ok := cleanup.TimeoutMinutes.(int); !ok || timeout != 45 {
		t.Errorf("cancellation cleanup timeout = %#v, want 45 minutes for the bounded 12-reconciliation workload", cleanup.TimeoutMinutes)
	}
}

type requiredWorkflowSpec struct {
	name                 string
	path                 string
	requiredJobID        string
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
		requiredJobID:        "chrome-required",
		checkNamePrefix:      "chrome-extension / ",
		changeOutput:         "chrome_extension",
		changedEnv:           "CHROME_EXTENSION_CHANGED",
		qualityGateCondition: "needs.changes.outputs.chrome_extension == 'true'",
		detectChangesName:    "browser-extensions / detect changes",
		requiredName:         "chrome-extension / required",
		verifierStepName:     "Verify Chrome extension CI result",
		unchangedOutput:      "No Chrome extension-impacting changes detected",
		pullRequestBranches:  []string{"**"},
	},
	{
		name:                 "edge-extension",
		path:                 "chrome-extension.yml",
		requiredJobID:        "edge-required",
		checkNamePrefix:      "edge-extension / ",
		changeOutput:         "edge_extension",
		changedEnv:           "EDGE_EXTENSION_CHANGED",
		qualityGateCondition: "needs.changes.outputs.edge_extension == 'true'",
		detectChangesName:    "browser-extensions / detect changes",
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
	Env     map[string]any    `yaml:"env"`
	Outputs map[string]string `yaml:"outputs"`
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
	Env  map[string]any `yaml:"env"`
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
	if strings.Contains(fmt.Sprint(verifyStep.Env), "CLI_RELEASE_SOURCE_SHA") {
		t.Errorf("%q infers a dropped release source from workflow state; recovery must require the explicit original source SHA",
			cliReleaseVerifierStepName)
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

	// Keep workflow-dispatch authority out of the third-party release action.
	// The separate signal job owns the narrow Actions write permission.
	assertJobPermissions(t, releasePleaseJobID, job.Permissions, map[string]string{
		"contents":      "write",
		"pull-requests": "write",
	})
}

// TestCLIReleaseUsesAnExactEventDrivenGate keeps publication behind the exact
// packaged journey without holding a polling runner. The release creator
// starts the exact main CLI workflow, which signals the SHA-bound continuation
// only after the customer journey passes.
func TestCLIReleaseUsesAnExactEventDrivenGate(t *testing.T) {
	t.Parallel()

	raw := readWorkflowBytes(t, releasePleaseWorkflow)
	var releaseConcurrency struct {
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress *bool  `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(raw, &releaseConcurrency); err != nil {
		t.Fatalf("decode release concurrency: %v", err)
	}
	wantConcurrency := "${{ github.event_name == 'push' && 'release-please-maintenance' || format('release-please-continuation-{0}-{1}', inputs.cli_tag, inputs.source_sha || 'operator') }}"
	if releaseConcurrency.Concurrency.Group != wantConcurrency || releaseConcurrency.Concurrency.CancelInProgress == nil ||
		*releaseConcurrency.Concurrency.CancelInProgress {
		t.Errorf("release concurrency = %#v, want separate non-canceling maintenance and exact continuation groups",
			releaseConcurrency.Concurrency)
	}

	workflow := readWorkflow(t, releasePleaseWorkflow)
	gate, ok := workflow.Jobs["cli-release-gate"]
	if !ok {
		t.Fatal("release-please.yml is missing the exact CLI release gate")
	}
	if gate.Name != "Verify the exact CLI release gate" {
		t.Errorf("cli-release-gate.name = %q", gate.Name)
	}
	if timeout, ok := gate.TimeoutMinutes.(int); !ok || timeout != 5 {
		t.Errorf("cli-release-gate timeout = %#v, want 5", gate.TimeoutMinutes)
	}
	assertJobPermissions(t, "cli-release-gate", gate.Permissions, map[string]string{
		"actions":  "read",
		"contents": "write",
	})

	steps := map[string]*step{}
	for index := range gate.Steps {
		current := &gate.Steps[index]
		steps[current.Name] = current
	}
	for _, name := range []string{
		"Require the canonical release branch",
		"Resolve the exact release source",
		"Check the exact packaged customer-journey gate once",
	} {
		if steps[name] == nil {
			t.Fatalf("cli-release-gate is missing %q", name)
		}
	}
	verify := steps["Check the exact packaged customer-journey gate once"]
	if !strings.Contains(verify.Run, "scripts/check-exact-cli-release-gate.sh") ||
		!strings.Contains(verify.Run+fmt.Sprint(verify.Env), "CLI_RUN_ID") ||
		!strings.Contains(verify.Run+fmt.Sprint(verify.Env), "CLI_RUN_ATTEMPT") ||
		strings.Contains(verify.Run, "sleep ") || strings.Contains(verify.Run, "while ") {
		t.Error("cli-release-gate is not one bounded exact check")
	}
	exactGate, err := os.ReadFile(filepath.Join("..", "..", "scripts", "check-exact-cli-release-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(exactGate, []byte(`.display_title == ("CLI release gate " + $sha)`)) {
		t.Error("exact release gate accepts an operator soak at the same source SHA")
	}
	source := steps["Resolve the exact release source"]
	for _, required := range []string{"HANDOFF_SOURCE_SHA", `"${CLI_TAG}^{commit}"`,
		"CLI release tag does not match the handed-off source SHA",
		`gh release view "$CLI_TAG"`, "--json tagName,targetCommitish,isDraft",
		`(.isDraft | type) == "boolean"`, `draft=$(jq -r '.isDraft'`,
		`"$release_tag" == "$CLI_TAG"`, `"$draft" == true && "$release_target" != "$source_sha"`,
		"Draft CLI release target mismatch"} {
		if !strings.Contains(source.Run+fmt.Sprint(source.Env), required) {
			t.Errorf("exact source resolver is missing %q", required)
		}
	}
	if strings.Contains(source.Run, "/releases/tags/") {
		t.Error("exact source resolver uses the REST by-tag route, which cannot see a draft release")
	}
	if needs := parseWorkflowNeeds(t, "cli-release-gate", gate.Needs); !slices.Equal(needs, []string{releasePleaseJobID, "signal-cli-release"}) {
		t.Errorf("cli-release-gate needs = %v, want release-please and signal-cli-release", needs)
	}
	wantGateIf := "always() && !cancelled() && github.event_name == 'workflow_dispatch'" //nolint:misspell // GitHub expression function spelling.
	if got := strings.Join(strings.Fields(gate.If), " "); got != wantGateIf {
		t.Errorf("cli-release-gate.if = %q, want %q", got, wantGateIf)
	}

	releasePlease := workflow.Jobs[releasePleaseJobID]
	if releasePlease == nil {
		t.Fatal("release-please.yml is missing release-please")
	}
	if needs := parseWorkflowNeeds(t, releasePleaseJobID, releasePlease.Needs); len(needs) != 0 {
		t.Errorf("release-please PR maintenance is coupled to %v", needs)
	}
	signal := workflow.Jobs["signal-cli-release"]
	if signal == nil {
		t.Fatal("release workflow is missing its narrow exact-source signal job")
	}
	assertJobPermissions(t, "signal-cli-release", signal.Permissions, map[string]string{
		"actions": "write", "contents": "read",
	})
	releaseSignal := len(signal.Steps) == 1 &&
		strings.Contains(signal.Steps[0].Run, "gh workflow run cli.yml") &&
		strings.Contains(signal.Steps[0].Run, `release_source_sha=$SOURCE_SHA`) &&
		strings.Contains(signal.Steps[0].Run, `"$tag_sha" == "$SOURCE_SHA"`) &&
		strings.Contains(signal.Steps[0].Run, `--ref "$CLI_TAG"`) &&
		strings.Contains(signal.Steps[0].Run, "cli-customer-journey cli-customer-journey-cleanup") &&
		strings.Contains(signal.Steps[0].Run, `{name:"main",type:"branch"}`) &&
		strings.Contains(signal.Steps[0].Run, `{name:"v*",type:"tag"}`) &&
		!strings.Contains(signal.Steps[0].Run, "main_sha=")
	if !releaseSignal {
		t.Error("release creator does not start the exact source customer journey")
	}

	releaseCLI := workflow.Jobs["release-cli"]
	if releaseCLI == nil {
		t.Fatal("release-please.yml is missing release-cli")
	}
	if needs := parseWorkflowNeeds(t, "release-cli", releaseCLI.Needs); !slices.Equal(needs, []string{"cli-release-gate"}) {
		t.Errorf("release-cli.needs = %v, want only cli-release-gate", needs)
	}
	wantReleaseIf := "!cancelled() && needs.cli-release-gate.result == 'success' && needs.cli-release-gate.outputs.required == 'true'" //nolint:misspell // GitHub expression function spelling.
	if got := strings.Join(strings.Fields(releaseCLI.If), " "); got != wantReleaseIf {
		t.Errorf("release-cli.if = %q, want %q", got, wantReleaseIf)
	}
	matchedSource := false
	for index := range releaseCLI.Steps {
		current := &releaseCLI.Steps[index]
		if current.Name == "Verify the gated source matches the release tag" {
			matchedSource = strings.Contains(current.Run,
				"release tag does not match the exact source that passed the CLI customer journey")
			if current.ContinueOnError != nil {
				t.Error("release source-binding step allows failure")
			}
		}
	}
	if !matchedSource {
		t.Error("release-cli does not bind its tag to the exact gated source")
	}

	cli := readWorkflow(t, cliWorkflow)
	cliRaw := string(readWorkflowBytes(t, cliWorkflow))
	for _, required := range []string{
		"release_source_sha:",
		"Validate the exact release source",
		`"$GITHUB_REF_TYPE" == tag`,
		`"$GITHUB_SHA" == "$RELEASE_SOURCE_SHA"`,
		"before any job can request an Auth0 token",
	} {
		if !strings.Contains(cliRaw, required) {
			t.Errorf("CLI workflow does not bind a release handoff before Auth0 use: missing %q", required)
		}
	}
	for _, required := range []string{"cli_run_id:", "cli_run_attempt:"} {
		if !strings.Contains(string(readWorkflowBytes(t, releasePleaseWorkflow)), required) {
			t.Errorf("release workflow cannot accept exact CLI handoff field %q", required)
		}
	}
	result := cli.Jobs["signal-cli-release"]
	wantSignalIf := "github.event_name == 'workflow_dispatch' && inputs.release_source_sha != '' && needs.changes.outputs.cli == 'true' && needs.required.result == 'success'"
	if got := strings.Join(strings.Fields(result.If), " "); got != wantSignalIf {
		t.Errorf("CLI release signal if = %q, want an exact manual customer journey", got)
	}
	assertJobPermissions(t, "signal-cli-release", result.Permissions, map[string]string{
		"actions":  "write",
		"contents": "write",
	})
	journeySignal := false
	for index := range result.Steps {
		current := &result.Steps[index]
		if current.Name == "Continue an exact draft CLI release" {
			journeySignal = strings.Contains(current.Run, "gh workflow run release-please.yml") &&
				strings.Contains(current.Run, "source_sha=$GITHUB_SHA") &&
				strings.Contains(current.Run, "cli_run_id=$GITHUB_RUN_ID") &&
				strings.Contains(current.Run, "cli_run_attempt=$GITHUB_RUN_ATTEMPT") &&
				strings.Contains(current.Run, `"${cli_tag}^{commit}"`) &&
				strings.Contains(current.Run, `gh release view "$cli_tag"`) &&
				strings.Contains(current.Run, "--json tagName,targetCommitish,isDraft") &&
				strings.Contains(current.Run, `(.isDraft | type) == "boolean"`) &&
				strings.Contains(current.Run, `release_draft=$(jq -r '.isDraft'`) &&
				strings.Contains(current.Run, `"$release_tag" == "$cli_tag"`) &&
				strings.Contains(current.Run, `"$release_target" == "$GITHUB_SHA"`) &&
				strings.Contains(current.Run, `"$release_draft" == true`) &&
				!strings.Contains(current.Run, "/releases/tags/")
		}
	}
	if !journeySignal {
		t.Error("main CLI journey does not signal the exact source continuation")
	}

	assertExecutableRepoScript(t, "scripts/check-exact-cli-release-gate.sh")
}

// TestCLIReleaseDraftAwareStepsExecuteBothStates runs the checked-in shell,
// not a duplicate implementation. A valid JSON false must remain a successful
// lookup so an already-public release can take its intended no-op path.
func TestCLIReleaseDraftAwareStepsExecuteBothStates(t *testing.T) {
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var manifest map[string]string
	manifestBytes, err := os.ReadFile(filepath.Join("..", "..", ".release-please-manifest.json"))
	if err != nil {
		t.Fatalf("read release manifest: %v", err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode release manifest: %v", err)
	}
	cliVersion := manifest["apps/cli"]
	if cliVersion == "" {
		t.Fatal("release manifest has no CLI version")
	}
	cliTag := "v" + cliVersion

	stepRun := func(t *testing.T, workflowName, jobID, stepName string) string {
		t.Helper()
		workflow := readWorkflow(t, workflowName)
		job := workflow.Jobs[jobID]
		if job == nil {
			t.Fatalf("%s is missing job %q", workflowName, jobID)
		}
		for _, candidate := range job.Steps {
			if candidate.Name == stepName {
				return candidate.Run
			}
		}
		t.Fatalf("%s job %q is missing step %q", workflowName, jobID, stepName)
		return ""
	}

	type result struct {
		output       string
		githubOutput string
		ghCalls      string
		err          error
	}
	runStep := func(t *testing.T, script string, draft bool, target string, metadataOverrides ...map[string]string) result {
		t.Helper()

		tempDir := t.TempDir()
		binDir := filepath.Join(tempDir, "bin")
		if err := os.Mkdir(binDir, 0o700); err != nil {
			t.Fatalf("create stub bin: %v", err)
		}
		gitStub := `#!/bin/sh
set -eu
if [ "$1" = "rev-parse" ] && [ "$2" = "--verify" ]; then
  printf '%s\n' "$STUB_SHA"
  exit 0
fi
if [ "$1" = "merge-base" ] && [ "$2" = "--is-ancestor" ]; then
  exit 0
fi
exit 2
`
		ghStub := `#!/bin/sh
set -eu
if [ "$1" = "api" ]; then
  case "$2" in
    *deployment-branch-policies*)
      if [ "$STUB_ENV_VALID" = true ]; then
        printf '%s\n' '{"total_count":2,"branch_policies":[{"name":"v*","type":"tag"},{"name":"main","type":"branch"}]}'
      else
        printf '%s\n' '{"total_count":1,"branch_policies":[{"name":"main","type":"branch"}]}'
      fi
      ;;
    *environments/*)
      environment=${2#*environments/}
      printf '{"name":"%s","deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}\n' "$environment"
      ;;
    *commits/*) printf '%s\n' "$STUB_SHA" ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = "release" ] && [ "$2" = "view" ]; then
  printf '{"tagName":"%s","targetCommitish":"%s","isDraft":%s}\n' \
    "$STUB_TAG" "$STUB_TARGET" "$STUB_DRAFT"
  exit 0
fi
if [ "$1" = "workflow" ] && [ "$2" = "run" ]; then
  printf '%s\n' "$*" >>"$STUB_GH_LOG"
  exit 0
fi
exit 2
`
		for name, contents := range map[string]string{"git": gitStub, "gh": ghStub} {
			if err := os.WriteFile(filepath.Join(binDir, name), []byte(contents), 0o700); err != nil { //nolint:gosec // Test-owned command stubs must be executable and live under t.TempDir.
				t.Fatalf("write %s stub: %v", name, err)
			}
		}

		githubOutput := filepath.Join(tempDir, "github-output")
		ghCalls := filepath.Join(tempDir, "gh-calls")
		for _, path := range []string{githubOutput, ghCalls} {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("create %s: %v", filepath.Base(path), err)
			}
		}

		draftValue := "false"
		if draft {
			draftValue = "true"
		}
		overrides := map[string]string{
			"CLI_TAG":             cliTag,
			"GITHUB_OUTPUT":       githubOutput,
			"GITHUB_REPOSITORY":   "layervai/qurl-integrations",
			"GITHUB_SHA":          sourceSHA,
			"SOURCE_SHA":          sourceSHA,
			"HANDOFF_SOURCE_SHA":  sourceSHA,
			"HANDOFF_RUN_ID":      "700",
			"HANDOFF_RUN_ATTEMPT": "2",
			"GITHUB_RUN_ID":       "700",
			"GITHUB_RUN_ATTEMPT":  "2",
			"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"STUB_DRAFT":          draftValue,
			"STUB_ENV_VALID":      "true",
			"STUB_GH_LOG":         ghCalls,
			"STUB_SHA":            sourceSHA,
			"STUB_TAG":            cliTag,
			"STUB_TARGET":         target,
		}
		if len(metadataOverrides) > 1 {
			t.Fatal("runStep accepts at most one metadata override map")
		}
		if len(metadataOverrides) == 1 {
			for key, value := range metadataOverrides[0] {
				overrides[key] = value
			}
		}
		command := exec.CommandContext(t.Context(), "bash", "-euo", "pipefail", "-c", script) //nolint:gosec // Executes checked-in workflow shell with fixed test inputs.
		command.Dir = filepath.Join("..", "..")
		command.Env = environmentWithOverrides(os.Environ(), overrides)
		output, err := command.CombinedOutput()
		read := func(path string) string {
			contents, err := os.ReadFile(path) //nolint:gosec // Callers pass only test-owned paths created under t.TempDir above.
			if err != nil {
				t.Fatalf("read %s: %v", filepath.Base(path), err)
			}
			return string(contents)
		}
		return result{output: string(output), githubOutput: read(githubOutput), ghCalls: read(ghCalls), err: err}
	}

	releaseSignalRun := stepRun(t, releasePleaseWorkflow, "signal-cli-release", "Dispatch the exact CLI customer journey")
	t.Run("release_signal_accepts_exact_environment_policy", func(t *testing.T) {
		got := runStep(t, releaseSignalRun, true, sourceSHA)
		if got.err != nil {
			t.Fatalf("release signal rejected exact environment policy: %v\n%s", got.err, got.output)
		}
		for _, required := range []string{"workflow run cli.yml", "--ref " + cliTag, "release_source_sha=" + sourceSHA} {
			if !strings.Contains(got.ghCalls, required) {
				t.Errorf("release signal dispatch = %q, want %q", got.ghCalls, required)
			}
		}
	})
	t.Run("release_signal_rejects_broad_or_missing_environment_policy", func(t *testing.T) {
		got := runStep(t, releaseSignalRun, true, sourceSHA, map[string]string{"STUB_ENV_VALID": "false"})
		if got.err == nil || !strings.Contains(got.output, "must allow only main and v* release tags") {
			t.Fatalf("release signal accepted incomplete environment policy: err=%v output=%s", got.err, got.output)
		}
		if got.ghCalls != "" {
			t.Errorf("invalid environment policy dispatched a workflow: %q", got.ghCalls)
		}
	})

	releaseGateRun := stepRun(t, releasePleaseWorkflow, "cli-release-gate", "Resolve the exact release source")
	for _, draft := range []bool{true, false} {
		t.Run(fmt.Sprintf("release_gate_draft_%t", draft), func(t *testing.T) {
			target := "main"
			if draft {
				target = sourceSHA
			}
			got := runStep(t, releaseGateRun, draft, target)
			if got.err != nil {
				t.Fatalf("execute release gate with draft=%t: %v\n%s", draft, got.err, got.output)
			}
			wantRequired := fmt.Sprintf("required=%t\n", draft)
			if !strings.Contains(got.githubOutput, wantRequired) {
				t.Errorf("gate output = %q, want %q", got.githubOutput, wantRequired)
			}
		})
	}

	t.Run("public_operator_recovery_needs_no_handoff", func(t *testing.T) {
		got := runStep(t, releaseGateRun, false, "main", map[string]string{
			"HANDOFF_SOURCE_SHA":  "",
			"HANDOFF_RUN_ID":      "",
			"HANDOFF_RUN_ATTEMPT": "",
		})
		if got.err != nil {
			t.Fatalf("public operator recovery failed: %v\n%s", got.err, got.output)
		}
		if !strings.Contains(got.githubOutput, "required=false\n") {
			t.Errorf("public operator gate output = %q, want no journey gate", got.githubOutput)
		}
	})

	t.Run("release_gate_draft_requires_exact_run", func(t *testing.T) {
		got := runStep(t, releaseGateRun, true, sourceSHA, map[string]string{
			"HANDOFF_RUN_ID":      "",
			"HANDOFF_RUN_ATTEMPT": "",
		})
		if got.err == nil || !strings.Contains(got.output, "requires one exact tested workflow run") {
			t.Fatalf("draft release accepted no exact CLI run: err=%v output=%s", got.err, got.output)
		}
	})

	cliSignalRun := stepRun(t, cliWorkflow, "signal-cli-release", "Continue an exact draft CLI release")
	for _, draft := range []bool{true, false} {
		t.Run(fmt.Sprintf("cli_signal_draft_%t", draft), func(t *testing.T) {
			target := "main"
			if draft {
				target = sourceSHA
			}
			got := runStep(t, cliSignalRun, draft, target)
			if got.err != nil {
				t.Fatalf("execute CLI signal with draft=%t: %v\n%s", draft, got.err, got.output)
			}
			if draft {
				for _, required := range []string{
					"workflow run release-please.yml",
					"cli_tag=" + cliTag,
					"source_sha=" + sourceSHA,
					"cli_run_id=700",
					"cli_run_attempt=2",
				} {
					if !strings.Contains(got.ghCalls, required) {
						t.Errorf("draft dispatch = %q, want %q", got.ghCalls, required)
					}
				}
				return
			}
			if got.ghCalls != "" {
				t.Errorf("public release dispatched a workflow: %q", got.ghCalls)
			}
			if !strings.Contains(got.output, "The exact CLI release is already public.") {
				t.Errorf("public release output = %q", got.output)
			}
		})
	}

	wrongTarget := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, subject := range []struct {
		name   string
		script string
	}{
		{name: "release_gate", script: releaseGateRun},
		{name: "cli_signal", script: cliSignalRun},
	} {
		t.Run(subject.name+"_rejects_wrong_draft_target", func(t *testing.T) {
			got := runStep(t, subject.script, true, wrongTarget)
			if got.err == nil {
				t.Fatal("workflow step accepted a draft whose target differs from its exact tag commit")
			}
			for _, required := range []string{"::error::Draft CLI release target mismatch", "observed " + wrongTarget, "expected exact", sourceSHA} {
				if !strings.Contains(got.output, required) {
					t.Errorf("draft-target failure = %q, want %q", got.output, required)
				}
			}
		})
		t.Run(subject.name+"_rejects_wrong_release_tag", func(t *testing.T) {
			got := runStep(t, subject.script, true, sourceSHA, map[string]string{"STUB_TAG": "v9.9.9"})
			if got.err == nil {
				t.Fatal("workflow step accepted release metadata for a different tag")
			}
			for _, required := range []string{"::error::CLI release tag mismatch", "v9.9.9", cliTag} {
				if !strings.Contains(got.output, required) {
					t.Errorf("release-tag failure = %q, want %q", got.output, required)
				}
			}
		})
		t.Run(subject.name+"_rejects_nonboolean_draft_state", func(t *testing.T) {
			got := runStep(t, subject.script, true, sourceSHA, map[string]string{"STUB_DRAFT": "null"})
			if got.err == nil {
				t.Fatal("workflow step accepted a nonboolean release draft state")
			}
			if !strings.Contains(got.output, "::error::CLI release metadata has no exact draft state") {
				t.Errorf("release-state failure = %q, want exact draft-state error", got.output)
			}
		})
	}
}

// TestCLIReleaseValidatesPackagesBeforePublication keeps each exact native
// package on the recoverable side of both publication lines. The macOS job
// installs the staged Homebrew cask, Linux reuses the downloaded release
// archive, and Windows runs its staged archive before publication.
func TestCLIReleaseValidatesPackagesBeforePublication(t *testing.T) {
	t.Parallel()

	var globals struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(readWorkflowBytes(t, releasePleaseWorkflow), &globals); err != nil {
		t.Fatal(err)
	}
	const commandRoster = "start stop restart status inspect daemon"
	if globals.Env["QURL_RELEASE_LIFECYCLE_COMMANDS"] != commandRoster {
		t.Errorf("release lifecycle command roster = %q, want %q", globals.Env["QURL_RELEASE_LIFECYCLE_COMMANDS"], commandRoster)
	}
	if globals.Env["QURL_REQUIRE_RELEASE_HUB_PIN"] != "0" {
		t.Errorf("release Hub-pin source mode = %q, want reviewed dark mode 0", globals.Env["QURL_REQUIRE_RELEASE_HUB_PIN"])
	}

	workflow := readWorkflow(t, releasePleaseWorkflow)
	release := workflow.Jobs["release-cli"]
	validator := workflow.Jobs["validate-homebrew-cask"]
	archiveValidator := workflow.Jobs["validate-windows-release-archive"]
	publisher := workflow.Jobs["publish-cli-release"]
	tapPublisher := workflow.Jobs["publish-homebrew-cask"]
	if release == nil || validator == nil || archiveValidator == nil || publisher == nil || tapPublisher == nil {
		t.Fatalf("release package chain is incomplete: release=%t homebrew=%t archives=%t publisher=%t tap=%t",
			release != nil, validator != nil, archiveValidator != nil, publisher != nil, tapPublisher != nil)
	}
	assertJobPermissions(t, "validate-homebrew-cask", validator.Permissions, map[string]string{"actions": "read", "contents": "read"})
	assertJobPermissions(t, "validate-windows-release-archive", archiveValidator.Permissions, map[string]string{"actions": "read", "contents": "read"})
	assertJobPermissions(t, "publish-cli-release", publisher.Permissions, map[string]string{
		"contents": "write", "packages": "write", "id-token": "write",
	})
	assertJobPermissions(t, "publish-homebrew-cask", tapPublisher.Permissions, map[string]string{"actions": "read", "contents": "read"})
	stepsFor := func(job *githubJob) map[string]*step {
		steps := map[string]*step{}
		for index := range job.Steps {
			steps[job.Steps[index].Name] = &job.Steps[index]
		}
		return steps
	}
	releaseSteps := stepsFor(release)
	validatorSteps := stepsFor(validator)
	archiveSteps := stepsFor(archiveValidator)
	publisherSteps := stepsFor(publisher)
	tapSteps := stepsFor(tapPublisher)
	validateName := "Validate generated Homebrew cask"
	stageName := "Stage the Homebrew validation bundle"
	if releaseSteps[validateName] == nil || releaseSteps[stageName] == nil {
		t.Fatal("release-cli does not validate and stage the generated cask")
	}
	for _, fragment := range []string{
		"generated=dist/homebrew/Casks/qurl.rb",
		"GoReleaser did not generate the qurl Homebrew cask",
		`line == want`,
		"generated Homebrew cask does not name the exact CLI version",
		`archive_url_prefix='releases/download/v#{version}/qurl_#{version}_'`,
		`for archive in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do`,
		"generated Homebrew cask does not bind exactly four release archives",
		"generated Homebrew cask does not bind the exact ${archive} release archive",
	} {
		if !strings.Contains(releaseSteps[validateName].Run, fragment) {
			t.Errorf("%q does not enforce %q", validateName, fragment)
		}
	}
	if releaseSteps[stageName].Uses != "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" ||
		releaseSteps[stageName].With["if-no-files-found"] != "error" ||
		!strings.Contains(fmt.Sprint(releaseSteps[stageName].With["path"]), "dist/qurl_*_darwin_*.tar.gz") ||
		!strings.Contains(fmt.Sprint(releaseSteps[stageName].With["path"]), "dist/qurl_*_windows_amd64.zip") {
		t.Errorf("staged Homebrew bundle is not exact: %#v", releaseSteps[stageName])
	}

	audit := validatorSteps["Audit and install the exact staged cask"]
	upload := validatorSteps["Upload the audited and installed cask"]
	if audit == nil || upload == nil {
		t.Fatal("macOS Homebrew validator does not audit, install, and emit one cask")
	}
	for _, fragment := range []string{
		`brew style --fix "$cask"`,
		`brew style "$cask"`,
		`brew audit --cask "$token"`,
		`brew --cache --cask "$token"`,
		`install -m 0644 "$archive" "$cache_path"`,
		`brew install --cask "$token"`,
		`reported_version=$("$installed" version | awk 'NR == 1 { print $3 }')`,
		`[[ "$reported_version" == "$release_version" ]]`,
		`for command in $QURL_RELEASE_LIFECYCLE_COMMANDS; do`,
		`"$installed" "$command" --help >/dev/null`,
		`if [[ "$QURL_RELEASE_HUB_PIN_MODE" == pinned ]]; then`,
		`elif [[ "$QURL_RELEASE_HUB_PIN_MODE" == dark ]]; then`,
		`missing required built-in connection settings`,
	} {
		if !strings.Contains(audit.Run, fragment) {
			t.Errorf("Homebrew pre-publication validator does not enforce %q", fragment)
		}
	}
	if upload.Uses != "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" ||
		upload.With["if-no-files-found"] != "error" {
		t.Errorf("validated Homebrew cask is not fail-closed: %#v", upload)
	}
	if got := parseWorkflowNeeds(t, "validate-homebrew-cask", validator.Needs); !slices.Contains(got, "release-cli") {
		t.Errorf("Homebrew validator does not depend on release-cli: %v", got)
	}
	if validator.Env["QURL_RELEASE_HUB_PIN_MODE"] != "${{ needs.release-cli.outputs.hub_pin_mode }}" {
		t.Error("Homebrew validator does not consume the exact validated Hub-pin mode")
	}
	draftVerifier := releaseSteps["Verify the draft CLI trust posture"]
	if draftVerifier == nil ||
		!strings.Contains(draftVerifier.Run, `"qurl_${release_version}_windows_arm64.zip"`) ||
		!strings.Contains(draftVerifier.Run, `for filename in "${expected[@]}"; do`) ||
		!strings.Contains(draftVerifier.Run, `reported_version=$("$native_binary" version | awk 'NR == 1 { print $3 }')`) ||
		!strings.Contains(draftVerifier.Run, `[[ "$reported_version" != "$release_version" ]]`) ||
		!strings.Contains(draftVerifier.Run, `for command in $QURL_RELEASE_LIFECYCLE_COMMANDS; do`) ||
		!strings.Contains(draftVerifier.Run, `"$native_binary" "$command" --help >/dev/null`) {
		t.Error("Linux release archive does not validate the lifecycle command roster")
	}
	download := archiveSteps["Download the staged Windows release archive"]
	windows := archiveSteps["Validate the exact Windows archive command roster"]
	if download == nil || download.Uses != "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c" ||
		download.With["digest-mismatch"] != "error" || windows == nil {
		t.Fatal("Windows release archive validation is not exact and fail closed")
	}
	if !strings.Contains(windows.Run, "qurl_${releaseVersion}_windows_amd64.zip") ||
		!strings.Contains(windows.Run, "$versionOutput = & $binary version") ||
		!strings.Contains(windows.Run, "$versionLine = $versionOutput | Select-Object -First 1") ||
		strings.Contains(windows.Run, "(& $binary version | Select-Object -First 1)") ||
		!strings.Contains(windows.Run, "$versionFields[2] -cne $releaseVersion") ||
		!strings.Contains(windows.Run, "$trustOutput = @(& $binary version --verify-release-native-trust 2>&1)") ||
		!strings.Contains(windows.Run, "missing required built-in connection settings") ||
		!strings.Contains(windows.Run, "$env:QURL_RELEASE_LIFECYCLE_COMMANDS -split ' '") ||
		!strings.Contains(windows.Run, "& $binary $command --help") {
		t.Error("Windows release archive does not validate the lifecycle command roster")
	}
	if got := parseWorkflowNeeds(t, "validate-windows-release-archive", archiveValidator.Needs); !slices.Contains(got, "release-cli") {
		t.Errorf("release archive validator does not depend on release-cli: %v", got)
	}
	if archiveValidator.Env["QURL_RELEASE_HUB_PIN_MODE"] != "${{ needs.release-cli.outputs.hub_pin_mode }}" {
		t.Error("Windows validator does not consume the exact validated Hub-pin mode")
	}

	promoteImage := publisherSteps["Sign and promote the tested qurl image"]
	publisherBranch := publisherSteps["Require the canonical release branch"]
	releasePublish := publisherSteps["Publish the verified CLI release"]
	if publisherBranch == nil || !strings.Contains(publisherBranch.Run, `[ "$GITHUB_REF" = refs/heads/main ]`) {
		t.Error("post-validation publisher does not fail closed outside refs/heads/main")
	}
	if releasePublish == nil || !strings.Contains(releasePublish.Run, "--draft=false --verify-tag") {
		t.Error("GitHub Release publication is not behind the Homebrew validator")
	}
	if releaseSteps["Promote and sign tested qurl image"] != nil ||
		releaseSteps["Sign and promote the tested qurl image"] != nil || promoteImage == nil ||
		!strings.Contains(promoteImage.Run, `docker buildx imagetools create --tag "$tagged" "$candidate"`) ||
		!strings.Contains(promoteImage.Run, `gh release upload "$CLI_TAG"`) {
		t.Error("versioned GHCR publication is not confined to the post-validation publisher")
	}
	if promoteImage == nil ||
		!strings.Contains(promoteImage.Run, `promoted_digest="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$tagged")"`) ||
		!strings.Contains(promoteImage.Run, `[[ "$promoted_digest" =~ ^sha256:[0-9a-f]{64}$ ]]`) ||
		!strings.Contains(promoteImage.Run, `[ "$promoted_digest" = "$IMAGE_DIGEST" ]`) {
		t.Error("versioned GHCR promotion does not request and validate the exact machine-readable index digest")
	}
	if promoteImage != nil {
		for _, line := range strings.Split(promoteImage.Run, "\n") {
			if strings.Contains(line, "imagetools inspect") && strings.Contains(line, "|") {
				t.Errorf("imagetools inspect is piped into another process: %s", strings.TrimSpace(line))
			}
		}
	}
	if promoteImage != nil && strings.Index(promoteImage.Run, `gh release upload "$CLI_TAG"`) >=
		strings.Index(promoteImage.Run, `docker buildx imagetools create --tag "$tagged" "$candidate"`) {
		t.Error("versioned GHCR tag can be promoted before signed image metadata reaches the draft release")
	}
	promoteIndex, publishIndex := -1, -1
	for index := range publisher.Steps {
		switch publisher.Steps[index].Name {
		case "Sign and promote the tested qurl image":
			promoteIndex = index
		case "Publish the verified CLI release":
			publishIndex = index
		}
	}
	if promoteIndex < 0 || publishIndex < 0 || promoteIndex >= publishIndex {
		t.Errorf("CLI release becomes public before its exact image: promote=%d publish=%d", promoteIndex, publishIndex)
	}
	if release.Outputs["qurl_image_digest"] != "${{ steps.qurl_candidate.outputs.digest }}" ||
		release.Outputs["hub_pin_mode"] != "${{ steps.release_hub_pin.outputs.mode }}" ||
		publisher.Env["QURL_IMAGE_DIGEST"] != "${{ needs.release-cli.outputs.qurl_image_digest }}" {
		t.Error("post-validation publisher is not bound to the exact image candidate tested by release-cli")
	}
	if got := parseWorkflowNeeds(t, "publish-cli-release", publisher.Needs); !slices.Contains(got, "validate-homebrew-cask") ||
		!slices.Contains(got, "validate-windows-release-archive") ||
		!slices.Contains(got, "release-cli") ||
		!strings.Contains(publisher.If, "needs.validate-windows-release-archive.result == 'success'") {
		t.Errorf("CLI publication bypasses package validation: %v", got)
	}

	tapPublish := tapSteps["Publish the audited Homebrew cask"]
	if tapPublish == nil || !strings.Contains(tapPublish.Run, `gh api --method PUT "repos/${tap_repo}/contents/${cask_path}"`) ||
		!strings.Contains(tapPublish.Run, "$RUNNER_TEMP/qurl-homebrew-validated/qurl.rb") {
		t.Error("tap publication does not consume the audited cask")
	}
	tapNeeds := parseWorkflowNeeds(t, "publish-homebrew-cask", tapPublisher.Needs)
	for _, required := range []string{"publish-cli-release", "validate-homebrew-cask"} {
		if !slices.Contains(tapNeeds, required) {
			t.Errorf("tap publication can bypass %q: %v", required, tapNeeds)
		}
	}
	for id, candidate := range map[string]*githubJob{
		"release-cli": release, "validate-homebrew-cask": validator,
		"validate-windows-release-archive": archiveValidator, "publish-cli-release": publisher,
	} {
		encoded, err := yaml.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "HOMEBREW_TAP_GITHUB_TOKEN") {
			t.Errorf("%s can access the tap publication token", id)
		}
	}
	if tapPublish.Env["GH_TOKEN"] != "${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}" {
		t.Error("tap publisher does not hold the token only at its final API step")
	}
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
		spec := &requiredWorkflowSpecs[i]
		registered[spec.path+"\x00"+specRequiredJobID(spec)] = true
	}

	seen := 0
	for _, name := range workflowFiles(t) {
		for id, job := range readWorkflow(t, name).Jobs {
			if id != requiredJobID && !strings.HasSuffix(job.Name, " / required") {
				continue
			}
			seen++
			if !registered[name+"\x00"+id] {
				t.Errorf("%s job %q defines a required aggregate but has no requiredWorkflowSpecs entry", name, id)
			}
		}
	}

	// workflowFiles already fatals on an empty scan, so this is purely the
	// count coupling: a workflow that grows a job
	// keyed `required` must land its spec entry in the same change, or the
	// whole suite goes red rather than quietly under-enforcing the new
	// aggregate.
	if seen != len(requiredWorkflowSpecs) {
		t.Errorf("found %d required aggregates, want %d", seen, len(requiredWorkflowSpecs))
	}
}

func TestEdgePackageRequiresSharedSourceChecks(t *testing.T) {
	workflow := readWorkflow(t, "chrome-extension.yml")
	job := workflow.Jobs["package-edge"]
	if job == nil {
		t.Fatal("chrome-extension.yml is missing package-edge")
	}
	needs := stringSet(parseWorkflowNeeds(t, "package-edge", job.Needs))
	if !needs[changesJobID] || !needs["build-chrome"] {
		t.Fatalf("package-edge needs = %#v, want changes and build-chrome", needs)
	}

	var guard *step
	checkoutIndex, guardIndex := -1, -1
	for i := range job.Steps {
		if strings.HasPrefix(job.Steps[i].Uses, checkoutActionPrefix) {
			checkoutIndex = i
		}
		if job.Steps[i].Name == "Require shared source checks" {
			guard = &job.Steps[i]
			guardIndex = i
		}
	}
	if guard == nil || guard.If != "needs.changes.outputs.chrome_extension == 'true'" {
		t.Fatalf("package-edge shared-source guard = %#v", guard)
	}
	if checkoutIndex < 0 || guardIndex <= checkoutIndex {
		t.Fatalf("package-edge checkout index = %d, guard index = %d", checkoutIndex, guardIndex)
	}
	if _, err := runVerifierScriptWithEnv(t, guard.Run, map[string]string{"CHROME_BUILD_RESULT": "success"}); err != nil {
		t.Fatalf("shared-source guard rejected success: %v", err)
	}
	if output, err := runVerifierScriptWithEnv(t, guard.Run, map[string]string{"CHROME_BUILD_RESULT": "failure"}); err == nil || !strings.Contains(output, "concluded failure") {
		t.Fatalf("shared-source guard accepted failure: err=%v output=%s", err, output)
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

			requiredNeeds := stringSet(parseWorkflowNeeds(t, specRequiredJobID(spec), required.Needs))
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

func TestBrowserRequiredVerifierScriptsStayInLockstep(t *testing.T) {
	workflow := readWorkflow(t, "chrome-extension.yml")
	chrome := requiredVerifierScript(t, requiredWorkflowSpecByName(t, "chrome-extension"), workflow)
	edge := requiredVerifierScript(t, requiredWorkflowSpecByName(t, "edge-extension"), workflow)

	chrome = strings.NewReplacer(
		"CHROME_EXTENSION_CHANGED", "BROWSER_EXTENSION_CHANGED",
		"chrome_extension", "browser_extension",
		"No Chrome extension-impacting changes detected", "No browser-impacting changes detected",
		"chrome-extension / build and test", "browser-extension / gate",
		"build-chrome", "browser-gate",
	).Replace(chrome)
	edge = strings.NewReplacer(
		"EDGE_EXTENSION_CHANGED", "BROWSER_EXTENSION_CHANGED",
		"edge_extension", "browser_extension",
		"No Edge extension-impacting changes detected", "No browser-impacting changes detected",
		"edge-extension / package", "browser-extension / gate",
		"package-edge", "browser-gate",
	).Replace(edge)

	if chrome != edge {
		t.Fatal("Chrome and Edge required-job verifier scripts drifted")
	}
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

// readWorkflowBytes returns a workflow file's raw contents.
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

	jobID := specRequiredJobID(spec)
	job, ok := workflow.Jobs[jobID]
	if !ok {
		t.Fatalf("%s workflow is missing its %q aggregate job", spec.name, jobID)
	}
	return job
}

func specRequiredJobID(spec *requiredWorkflowSpec) string {
	if spec.requiredJobID != "" {
		return spec.requiredJobID
	}
	return requiredJobID
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
	return !slices.Contains(needs, specRequiredJobID(spec))
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

func environmentWithOverrides(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; ok && replaced {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
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
