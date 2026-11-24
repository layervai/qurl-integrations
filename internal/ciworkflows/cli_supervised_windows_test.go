package ciworkflows

import (
	"strings"
	"testing"
)

func TestWindowsSupervisedLoginUsesShippingDependency(t *testing.T) {
	workflow := readWorkflow(t, cliWorkflow)
	var check step
	for _, current := range workflow.Jobs[cliMatrixJobID].Steps {
		if strings.Contains(current.Run, "-modfile=") || current.With["repository"] == "layervai/qurl-connector" {
			t.Fatal("Windows integration must use the shipping Connector dependency")
		}
		if current.Name == "Verify Windows supervised CLI integration" {
			check = current
		}
	}
	if check.If != "runner.os == 'Windows'" || check.Shell != "pwsh" || check.ContinueOnError != nil {
		t.Fatal("supervised integration must be a mandatory native Windows check")
	}
	for _, required := range []string{"go test -count=1 -json", "TestWindowsNodeSupervisedColdAndWarmLogin", "$passed.Count -ne 1", "$skipped.Count -ne 0"} {
		if !strings.Contains(check.Run, required) {
			t.Errorf("supervised integration is missing %q", required)
		}
	}
}
