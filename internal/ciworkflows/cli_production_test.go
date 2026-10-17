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
