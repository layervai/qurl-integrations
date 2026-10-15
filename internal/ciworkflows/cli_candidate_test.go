package ciworkflows

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	cliCandidateWorkflow = "cli-candidate.yml"
	// The only arguments that keep GoReleaser from publishing: --snapshot
	// creates no tag, release or cask push, and --skip=sign needs no OIDC.
	cliCandidateGoReleaserArgs = "release --snapshot --clean --skip=sign"
	goReleaserActionPrefix     = "goreleaser/goreleaser-action@"
)

// TestCLICandidateCannotReachCustomers pins what keeps a candidate build
// away from brew, install.sh and the releases page: it starts only by hand,
// can read but never write, builds in snapshot mode, and sees no secret but
// the production Hub pin, which is public key material. The job runs
// whatever code is on the dispatched branch, so any other secret here would
// reach unreviewed code.
func TestCLICandidateCannotReachCustomers(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, cliCandidateWorkflow)
	triggers := parseWorkflowTriggers(t, cliCandidateWorkflow, workflow.On)
	if got := slices.Sorted(maps.Keys(triggers)); !slices.Equal(got, []string{"workflow_dispatch"}) {
		t.Errorf("%s triggers = %v, want workflow_dispatch only", cliCandidateWorkflow, got)
	}
	if !readOnly(workflow.Permissions) {
		t.Errorf("%s permissions = %v, want contents: read only", cliCandidateWorkflow, workflow.Permissions)
	}

	goReleaserSteps := 0
	for name, job := range workflow.Jobs {
		if job.Permissions != nil && !readOnly(job.Permissions) {
			t.Errorf("job %s permissions = %v, want none or contents: read only", name, job.Permissions)
		}
		if job.Uses != "" {
			t.Errorf("job %s calls a reusable workflow %q, whose permissions this test cannot see", name, job.Uses)
		}
		for index := range job.Steps {
			step := &job.Steps[index]
			if strings.HasPrefix(step.Uses, checkoutActionPrefix) && step.With["persist-credentials"] != false {
				t.Errorf("job %s checks out without persist-credentials: false", name)
			}
			if strings.HasPrefix(step.Uses, goReleaserActionPrefix) {
				goReleaserSteps++
				if args, _ := step.With["args"].(string); args != cliCandidateGoReleaserArgs {
					t.Errorf("job %s runs GoReleaser with %q, want %q", name, args, cliCandidateGoReleaserArgs)
				}
			}
		}
	}
	if goReleaserSteps != 1 {
		t.Errorf("%s has %d GoReleaser steps, want 1", cliCandidateWorkflow, goReleaserSteps)
	}

	// Comment lines are dropped so the header may name what it rules out.
	uncommented := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAll(readWorkflowBytes(t, cliCandidateWorkflow), nil)
	allowed := []string{"QURL_PROD_NHP_HUB_PUBLIC_KEY_B64", "QURL_PROD_NHP_HUB_PUBLIC_KEY_SHA256"}
	for _, match := range regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`).FindAllSubmatch(uncommented, -1) {
		if !slices.Contains(allowed, string(match[1])) {
			t.Errorf("%s reads secret %s; only the public Hub pin may reach branch code", cliCandidateWorkflow, match[1])
		}
	}
	if token := regexp.MustCompile(`(?i)github\.token|GITHUB_TOKEN|\bsecrets\s*\[|toJSON\(\s*secrets`).Find(uncommented); token != nil {
		t.Errorf("%s uses %q; a candidate gets no token and no secret map", cliCandidateWorkflow, token)
	}
	if key := regexp.MustCompile(`\bcontinue-on-error\s*:`).Find(uncommented); key != nil {
		t.Errorf("%s uses continue-on-error, which can let a failed check publish an artifact", cliCandidateWorkflow)
	}
}

// readOnly reports whether a permissions value grants exactly contents: read.
func readOnly(permissions any) bool {
	grants, ok := permissions.(map[string]any)
	return ok && len(grants) == 1 && grants["contents"] == "read"
}
