package ciworkflows

import (
	"regexp"
	"strings"
	"testing"
)

func TestWindowsConnectorPreviewCannotReplaceTheShippingDependency(t *testing.T) {
	workflow := readWorkflow(t, cliWorkflow)
	steps := map[string]step{}
	for _, current := range workflow.Jobs[cliMatrixJobID].Steps {
		steps[current.Name] = current
	}
	checkout := steps["Check out the nonshipping Windows Connector candidate"]
	if checkout.If != "runner.os == 'Windows'" || !strings.HasPrefix(checkout.Uses, checkoutActionPrefix) ||
		checkout.With["repository"] != "layervai/qurl-connector" || checkout.With["persist-credentials"] != false {
		t.Fatal("Connector preview must use a credential-free Windows-only public checkout")
	}
	ref, _ := checkout.With["ref"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(ref) {
		t.Fatal("Connector preview requires an immutable reviewed source commit")
	}
	check := steps["Verify the nonshipping Windows supervised CLI integration"]
	if check.If != "runner.os == 'Windows'" || check.Shell != "pwsh" || check.ContinueOnError != nil {
		t.Fatal("Connector preview must be a mandatory native Windows check")
	}
	for _, required := range []string{"-modfile=$modfile", "-tags=connectorpreview", "TestWindowsNodeSupervisedColdAndWarmLogin", "$passed.Count -ne 1", "$skipped.Count -ne 0", "git diff --exit-code -- go.mod go.sum"} {
		if !strings.Contains(check.Run, required) {
			t.Errorf("source-only integration is missing %q", required)
		}
	}
}
