package ciworkflows

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCLIPublicationRequiresExactProductionProof(t *testing.T) {
	workflow := readWorkflow(t, releasePleaseWorkflow)
	for _, name := range []string{"production-proof", "production-cleanup", "publish-cli-release"} {
		if workflow.Jobs[name] == nil {
			t.Fatalf("required production gate is missing: %s", name)
		}
	}
	publisher := workflow.Jobs["publish-cli-release"]
	for _, gate := range []string{"production-proof", "production-cleanup"} {
		if !slices.Contains(parseWorkflowNeeds(t, "publish-cli-release", publisher.Needs), gate) ||
			!strings.Contains(publisher.If, "needs."+gate+".result == 'success'") {
			t.Fatalf("publication must require successful %s, including recovery dispatches", gate)
		}
	}
	verified, checkedOut := false, false
	for _, current := range publisher.Steps {
		if strings.HasPrefix(current.Uses, checkoutActionPrefix) {
			checkedOut = true
		}
		if strings.Contains(current.Run, "verify-production-proof.py\" verify") {
			if !checkedOut {
				t.Fatal("production proof verification requires the checked-out trusted guard")
			}
			if current.If != "" || current.ContinueOnError != nil {
				t.Fatal("production evidence verification cannot be optional")
			}
			verified = true
		}
		if strings.Contains(current.Run, "docker buildx imagetools create") || strings.Contains(current.Run, "--draft=false") {
			if !verified {
				t.Fatal("public distribution precedes production proof verification")
			}
		}
	}
	if !verified {
		t.Fatal("publication does not verify exact archive and evidence bytes")
	}
	proof := workflow.Jobs["production-proof"]
	assertJobPermissions(t, "production-proof", proof.Permissions, map[string]string{"contents": "read"})
	steps := map[string]step{}
	for _, current := range proof.Steps {
		steps[current.Name] = current
	}
	journey := steps["Require the real production journey to pass without skips"]
	for _, required := range []string{"TestProductionReleaseJourney", "-test.count=1", "-test.list", "--- SKIP:", `[[ "$status" == 0 ]]`, "scripts/cli-production-proof.py record"} {
		if !strings.Contains(journey.Run, required) {
			t.Errorf("production journey must enforce %q", required)
		}
	}
	if journey.If != "" || journey.ContinueOnError != nil {
		t.Fatal("production journey is optional")
	}
	download := steps["Download the exact draft release candidate"]
	if !strings.Contains(download.Uses, "actions/download-artifact@") ||
		!strings.Contains(download.With["name"].(string), "needs.cli-release-gate.outputs.source_sha") ||
		!strings.Contains(download.With["name"].(string), "github.run_attempt") {
		t.Fatal("production must use the exact packaged candidate from this attempt")
	}
	staged := false
	for _, current := range workflow.Jobs["release-cli"].Steps {
		if current.Name == "Stage the production candidate bundle" {
			staged = strings.Contains(current.Uses, "actions/upload-artifact@") && current.With["name"] == download.With["name"] && strings.Contains(current.With["path"].(string), "dist/checksums.txt") && strings.Contains(current.With["path"].(string), "dist/qurl_*.tar.gz") && strings.Contains(current.With["path"].(string), "dist/qurl_*.zip")
		}
	}
	if !staged {
		t.Fatal("release build must stage every archive and checksum for production proof")
	}
	cleanup := workflow.Jobs["production-cleanup"]
	if !strings.Contains(cleanup.If, "always()") {
		t.Fatal("failed production runs must still clean up")
	}
	fallback := readWorkflow(t, "cli-production-cleanup.yml")
	if !strings.Contains(fallback.Jobs["cleanup"].If, "head_branch == 'main'") {
		t.Fatal("cancellation cleanup must reject branch workflows")
	}
	for _, current := range fallback.Jobs["cleanup"].Steps {
		if strings.Contains(current.Uses, "download-artifact") {
			t.Fatal("privileged cancellation cleanup must not consume candidate artifacts")
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "apps", "cli", "cmd", "production_release_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "t.Skip") || !strings.Contains(string(raw), `const productionProofEndpoint = "https://api.layerv.ai"`) {
		t.Fatal("production proof must target production and never skip")
	}
}

func TestCLIProductionCredentialsUseNativeWindowsPermissionsAndCleanup(t *testing.T) {
	workflow := readWorkflow(t, releasePleaseWorkflow)
	steps := map[string]step{}
	for _, current := range workflow.Jobs["production-proof"].Steps {
		steps[current.Name] = current
	}
	for _, name := range []string{"Mint disposable production credentials", "Remove local disposable credentials"} {
		if !strings.Contains(steps[name].If, "runner.os != 'Windows'") {
			t.Fatalf("POSIX credential operations must exclude Windows: %s", name)
		}
	}
	mint := steps["Mint disposable Windows production credentials"]
	if mint.If != "runner.os == 'Windows'" || mint.Shell != "pwsh" || mint.ContinueOnError != nil {
		t.Fatal("Windows credentials require a mandatory native PowerShell step")
	}
	for _, required := range []string{"icacls.exe", "/inheritance:r", "/grant:r", "Remove-Item Env:AUTOMATION_API_KEY", "create-pair", "--qurl-endpoint https://api.layerv.ai", "--run-id $env:QURL_SHARING_RUN_ID", "--run-attempt $env:QURL_SHARING_RUN_ATTEMPT", "finally {", "Test-Path -LiteralPath $authority"} {
		if !strings.Contains(mint.Run, required) {
			t.Errorf("Windows production credentials lack %q", required)
		}
	}
	if strings.Index(mint.Run, "icacls.exe") > strings.Index(mint.Run, "WriteAllText") {
		t.Fatal("Windows directory ACL must be restricted before the parent key is written")
	}
	cleanup := steps["Remove local Windows production credentials"]
	if cleanup.If != "always() && runner.os == 'Windows'" || cleanup.Shell != "pwsh" || cleanup.ContinueOnError != nil {
		t.Fatal("Windows production credential cleanup must always run and fail closed")
	}
	for _, required := range []string{"@('production-authority', 'production-keys')", "Join-Path $env:RUNNER_TEMP $name", "Remove-Item -LiteralPath $path -Recurse -Force", "throw 'local production credentials remain"} {
		if !strings.Contains(cleanup.Run, required) {
			t.Errorf("Windows production cleanup lacks %q", required)
		}
	}
}

func TestCLI360ImageRecoveryCannotRebuildOrChangeNativeRelease(t *testing.T) {
	workflow := readWorkflow(t, releasePleaseWorkflow)
	recovery := workflow.Jobs["recover-cli360-image"]
	if recovery == nil || recovery.If != "github.event_name == 'workflow_dispatch' && inputs.promote_built_cli_360_image" {
		t.Fatal("image-only recovery must require its explicit dispatch")
	}
	assertJobPermissions(t, "recover-cli360-image", recovery.Permissions, map[string]string{"actions": "read", "contents": "write", "packages": "write", "id-token": "write"})
	if !strings.Contains(workflow.Jobs["cli-release-gate"].If, "!inputs.promote_built_cli_360_image") {
		t.Fatal("image-only dispatch must not start the normal release build")
	}
	var operations strings.Builder
	for _, current := range recovery.Steps {
		operations.WriteString(current.Run)
		if strings.HasPrefix(current.Uses, checkoutActionPrefix) && current.With["persist-credentials"] != false {
			t.Fatal("image recovery checkout must not retain write credentials")
		}
	}
	text := operations.String()
	for _, forbidden := range []string{"goreleaser", "docker buildx build", "gh release edit", "--draft=false", "--clobber"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("image-only recovery contains a native publication or rebuild: %s", forbidden)
		}
	}
	for _, required := range []string{"check-cli360-image-recovery.py", "cosign verify-blob", "version --verify-release-native-trust", "extract-qurl-image-attestations.sh", "QURL_EXPECTED_VCS_REVISION", "https://github.com/layervai/qurl-integrations/actions/runs/39854001197/attempts/1", "cosign sign --yes", "cosign attest --yes", "cosign verify-attestation", "release-please.yml@refs/heads/main", "docker buildx imagetools create", `== "$IMAGE_DIGEST"`} {
		if !strings.Contains(text, required) {
			t.Errorf("image recovery lacks %q", required)
		}
	}
	if strings.Contains(text, "|| true") || !strings.Contains(text, `cat "$RUNNER_TEMP/image-tag-error" >&2`) {
		t.Fatal("unknown registry failures must not be treated as an absent image tag")
	}
	if strings.Count(text, "gh release upload") != 1 || !strings.Contains(text, "for file in qurl-image.txt qurl-image-buildkit-manifest.json; do") || !strings.Contains(text, `gh release upload "$CLI_TAG" "$RUNNER_TEMP/image-proof/$file"`) || !strings.Contains(text, `cmp "$RUNNER_TEMP/image-proof/$file"`) {
		t.Fatal("only the two exact immutable image proof assets may be added or compared, never replaced")
	}
	if strings.Count(text, "python3 scripts/check-cli360-image-recovery.py") != 2 {
		t.Fatal("owner and exact evidence must be checked before verification and again before promotion")
	}
}
