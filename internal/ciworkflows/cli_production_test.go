package ciworkflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCLIPublicationRequiresProductionApproval(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", releasePleaseWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Environment string `yaml:"environment"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Jobs["publish-cli-release"].Environment != "cli-production" {
		t.Fatal("CLI publication must wait for the cli-production approval")
	}
	publisher := readWorkflow(t, releasePleaseWorkflow).Jobs["publish-cli-release"]
	guardIndex, promotionIndex := -1, -1
	for index, step := range publisher.Steps {
		if step.Run == "scripts/check-cli-production-protection.sh" {
			guardIndex = index
			if step.If != "" || step.ContinueOnError != nil {
				t.Fatal("production protection check must be unconditional and required")
			}
		}
		if step.Name == "Sign and promote the tested qurl image" {
			promotionIndex = index
		}
	}
	if guardIndex < 0 || promotionIndex <= guardIndex {
		t.Fatal("production protection must be checked before image promotion")
	}
}

func TestCLIProductionProtectionFailsClosed(t *testing.T) {
	checker, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check-cli-production-protection.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const protection = `{
		"name":"cli-production", "can_admins_bypass":false,
		"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},
		"protection_rules":[{"type":"required_reviewers","reviewers":[
			{"type":"User","reviewer":{"id":178750268,"login":"justin-layerv"}}
		]}]
	}`
	const policies = `{"total_count":1,"branch_policies":[{"name":"main","type":"branch"}]}`
	for _, test := range []struct {
		name, protection, policies, repository, ref string
		wantPass                                    bool
	}{
		{name: "protected main", wantPass: true},
		{name: "missing environment", protection: `{}`},
		{name: "malformed environment", protection: `not json`},
		{name: "API failure", protection: "fail"},
		{name: "missing approval", protection: strings.Replace(protection, `"required_reviewers"`, `"wait_timer"`, 1)},
		{name: "wrong approver", protection: strings.Replace(protection, "178750268", "123", 1)},
		{name: "team approval", protection: strings.Replace(protection, `"User"`, `"Team"`, 1)},
		{name: "additional approver", protection: strings.Replace(protection, `"login":"justin-layerv"}}`, `"login":"justin-layerv"}},{"type":"User","reviewer":{"id":123}}`, 1)},
		{name: "admin bypass", protection: strings.Replace(protection, `"can_admins_bypass":false`, `"can_admins_bypass":true`, 1)},
		{name: "unknown bypass", protection: strings.Replace(protection, `"can_admins_bypass":false,`, "", 1)},
		{name: "all protected branches", protection: strings.Replace(protection, `"protected_branches":false`, `"protected_branches":true`, 1)},
		{name: "all branches", policies: `{"total_count":0,"branch_policies":[]}`},
		{name: "tag named main", policies: strings.Replace(policies, `"branch"`, `"tag"`, 1)},
		{name: "truncated policies", policies: strings.Replace(policies, `"total_count":1`, `"total_count":2`, 1)},
		{name: "fork", repository: "other/qurl-integrations"},
		{name: "branch", ref: "refs/heads/feature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			policyJSON, branchJSON := test.protection, test.policies
			if policyJSON == "" {
				policyJSON = protection
			}
			if branchJSON == "" {
				branchJSON = policies
			}
			for name, body := range map[string]string{"protection.json": policyJSON, "branches.json": branchJSON} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			const mock = `#!/bin/sh
case "$2" in
  */deployment-branch-policies*) cat "$FIXTURE/branches.json" ;;
  */environments/cli-production)
    if [ "$(cat "$FIXTURE/protection.json")" = fail ]; then exit 1; fi
    cat "$FIXTURE/protection.json" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(mock), 0o700); err != nil {
				t.Fatal(err)
			}
			repository, ref := test.repository, test.ref
			if repository == "" {
				repository = "layervai/qurl-integrations"
			}
			if ref == "" {
				ref = "refs/heads/main"
			}
			cmd := exec.Command("bash", checker)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_TOKEN=test", "GITHUB_REPOSITORY="+repository, "GITHUB_REF="+ref, "FIXTURE="+dir)
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantPass {
				t.Fatalf("pass=%t, want %t: %v\n%s", err == nil, test.wantPass, err, output)
			}
		})
	}
}
