package ciworkflows

import (
	"slices"
	"strings"
	"testing"
)

func TestTeamsDockerGateBuildsAndSmokesTheProductionImage(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, "teams.yml")
	job := workflow.Jobs["docker-check"]
	if job == nil {
		t.Fatal("Teams CI must build and smoke the production image")
	}
	if job.If != "needs.changes.outputs.teams == 'true'" {
		t.Errorf("Teams image gate condition = %q", job.If)
	}
	if required := workflow.Jobs[requiredJobID]; required == nil ||
		!slices.Contains(parseWorkflowNeeds(t, requiredJobID, required.Needs), "docker-check") {
		t.Fatal("teams / required must require the image gate")
	}

	var build, smoke *step
	for index := range job.Steps {
		current := &job.Steps[index]
		if strings.HasPrefix(current.Uses, "docker/build-push-action@") {
			build = current
		}
		if strings.Contains(current.Run, "apps/teams/scripts/docker-smoke.sh") {
			smoke = current
		}
	}
	if build == nil {
		t.Fatal("Teams image gate must build the app-owned Dockerfile")
	}
	for key, want := range map[string]any{
		"context": "apps/teams", "file": "apps/teams/Dockerfile",
		"platforms": "linux/arm64", "push": false, "load": true,
		"tags": "qurl-teams:ci", "provenance": false,
	} {
		if build.With[key] != want {
			t.Errorf("Teams image build %s = %#v, want %#v", key, build.With[key], want)
		}
	}
	if build.If != "" || build.ContinueOnError != nil || smoke == nil ||
		smoke.If != "" || smoke.ContinueOnError != nil ||
		strings.TrimSpace(smoke.Run) != "bash apps/teams/scripts/docker-smoke.sh qurl-teams:ci" {
		t.Error("the built Teams image must pass its smoke without a conditional or failure bypass")
	}
	assertExecutableRepoScript(t, "apps/teams/scripts/docker-smoke.sh")
}
