package ciworkflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Local hooks and every CI invocation must use the same linter as make.
func TestGolangciLintPinsMatchMakefile(t *testing.T) {
	t.Parallel()
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION := (v\d+\.\d+\.\d+)$`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("Makefile must pin GOLANGCI_LINT_VERSION")
	}
	expected := string(match[1])
	runPin := regexp.MustCompile(`github\.com/golangci/golangci-lint/v2/cmd/golangci-lint@([^\s]+)`)
	actions, invocations := 0, 0
	for _, filename := range workflowFiles(t) {
		workflow := readWorkflow(t, filename)
		for jobID, job := range workflow.Jobs {
			for i := range job.Steps {
				current := &job.Steps[i]
				check := func(pin string) {
					t.Helper()
					pin = strings.Trim(pin, "\"'")
					switch pin {
					case "${{ env.GOLANGCI_LINT_VERSION }}", "${GOLANGCI_LINT_VERSION}", "$GOLANGCI_LINT_VERSION":
						value := job.Env["GOLANGCI_LINT_VERSION"]
						if override, ok := current.Env["GOLANGCI_LINT_VERSION"]; ok {
							value = override
						}
						pin, _ = value.(string)
					}
					if pin != expected {
						t.Errorf("%s job %s step %q pins %q; Makefile pins %s", filename, jobID, current.Name, pin, expected)
					}
				}
				if strings.HasPrefix(current.Uses, "golangci/golangci-lint-action@") {
					actions++
					pin, _ := current.With["version"].(string)
					check(pin)
				}
				for _, pin := range runPin.FindAllStringSubmatch(current.Run, -1) {
					invocations++
					check(pin[1])
				}
			}
		}
	}
	if actions == 0 || invocations == 0 {
		t.Fatal("no linter action or config-verification invocation found")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".pre-commit-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct{ Repos []struct{ Repo, Rev string } }
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, repo := range config.Repos {
		if repo.Repo != "https://github.com/golangci/golangci-lint" {
			continue
		}
		found = true
		if repo.Rev != expected {
			t.Errorf("pre-commit pins %q; Makefile pins %s", repo.Rev, expected)
		}
	}
	if !found {
		t.Fatal("missing golangci-lint pre-commit hook")
	}
}
