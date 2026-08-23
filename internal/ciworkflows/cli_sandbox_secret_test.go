package ciworkflows

import (
	"strings"
	"testing"
)

func TestCLISandboxMaterializesBearerBeforeAnyChildProcess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		workflow string
		jobID    string
	}{
		{workflow: "cli.yml", jobID: "sandbox-e2e"},
		{workflow: "cli-nightly.yml", jobID: "sandbox-extended"},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			t.Parallel()
			job := readWorkflow(t, tc.workflow).Jobs[tc.jobID]
			if job == nil {
				t.Fatalf("%s is missing job %q", tc.workflow, tc.jobID)
			}
			var materialize *step
			for i := range job.Steps {
				if job.Steps[i].Name == "Materialize sandbox API key for the test process" {
					materialize = &job.Steps[i]
					break
				}
			}
			if materialize == nil {
				t.Fatal("sandbox job has no API-key materializer")
			}
			want := strings.Join([]string{
				"set -euo pipefail",
				`test -n "$QURL_API_KEY_SOURCE"`,
				`secret_file="$RUNNER_TEMP/qurl-sandbox-api-key"`,
				"umask 077",
				`printf '%s' "$QURL_API_KEY_SOURCE" > "$secret_file"`,
				"unset QURL_API_KEY_SOURCE",
				`chmod 0600 "$secret_file"`,
				`echo "QURL_API_KEY_FILE=$secret_file" >> "$GITHUB_ENV"`,
			}, "\n")
			if strings.TrimSpace(materialize.Run) != want {
				t.Fatalf("%s materializer can expose the bearer to a child or drift from the file-only contract:\n%s", tc.workflow, materialize.Run)
			}
			if strings.Contains(readWorkflowSource(t, tc.workflow), "\n          QURL_API_KEY: ${{") {
				t.Fatal("sandbox workflow injects the plaintext API key into the Go test process")
			}
		})
	}
}
