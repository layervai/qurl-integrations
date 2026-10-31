package ciworkflows

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestClaudeExecutionRequiresTerminalSuccess(t *testing.T) {
	t.Parallel()
	requireCommand(t, "jq")
	workflows := []struct{ file, job, step string }{
		{"claude.yml", "claude", "Verify reviewed pull request head"},
		{claudeCodeReviewWorkflow, claudeReviewJobID, claudeReviewPublishStepName},
	}
	cases := []struct{ name, input, want string }{
		{"success", `[{"type":"assistant","message":"reviewing"},{"type":"result","subtype":"success","is_error":false,"result":"Reviewed: no findings."}]`, "Reviewed: no findings."},
		{"last result controls recovery", `[{"type":"result","subtype":"error","is_error":true},{"type":"result","subtype":"success","is_error":false,"result":"Recovered review."}]`, "Recovered review."},
		{"provider error", `[{"type":"result","subtype":"success","is_error":true,"result":"Do not approve."}]`, ""},
		{"missing error flag", `[{"type":"result","subtype":"success","result":"Do not approve."}]`, ""},
		{"null error flag", `[{"type":"result","subtype":"success","is_error":null,"result":"Do not approve."}]`, ""},
		{"string error flag", `[{"type":"result","subtype":"success","is_error":"false","result":"Do not approve."}]`, ""},
		{"unsuccessful subtype", `[{"type":"result","subtype":"error_max_turns","is_error":false,"result":"Do not approve."}]`, ""},
		{"later error overrides earlier success", `[{"type":"result","subtype":"success","is_error":false,"result":"Earlier review."},{"type":"result","subtype":"error","is_error":true,"result":"Failed."}]`, ""},
		{"missing final text", `[{"type":"result","subtype":"success","is_error":false}]`, ""},
		{"empty final text", `[{"type":"result","subtype":"success","is_error":false,"result":""}]`, ""},
		{"whitespace final text", `[{"type":"result","subtype":"success","is_error":false,"result":" \n\t"}]`, ""},
		{"object final text", `[{"type":"result","subtype":"success","is_error":false,"result":{"review":"not terminal text"}}]`, ""},
		{"false final text", `[{"type":"result","subtype":"success","is_error":false,"result":false}]`, ""},
		{"no result", `[{"type":"assistant","message":"Still working."}]`, ""},
		{"empty array", `[]`, ""},
		{"empty file", ``, ""},
		{"whitespace file", " \n\t", ""},
		{"wrong document shape", `{"type":"result","subtype":"success","is_error":false,"result":"Not an execution array."}`, ""},
		{"multiple documents", `[{"type":"result","subtype":"success","is_error":false,"result":"First."}] [{"type":"result","subtype":"success","is_error":false,"result":"Second."}]`, ""},
		{"malformed document", `[{`, ""},
	}
	// Execute the actual filters from each workflow, not a parallel copy of the policy.
	filterPattern := regexp.MustCompile(`(?s)jq\s+(-[a-z]+)\s+'([^']+)'\s+"\$\{EXECUTION_FILE\}"`)
	for _, workflow := range workflows {
		t.Run(workflow.file, func(t *testing.T) {
			t.Parallel()
			job := readWorkflow(t, workflow.file).Jobs[workflow.job]
			if job == nil {
				t.Fatalf("missing job %s", workflow.job)
			}
			var run string
			for _, step := range job.Steps {
				if step.Name == workflow.step {
					run = step.Run
				}
			}
			matches := filterPattern.FindAllStringSubmatch(run, -1)
			if len(matches) != 1 {
				t.Fatalf("%s must validate one terminal execution result, got %d filters", workflow.step, len(matches))
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					// #nosec G204 -- jq flags and filter come from the checked-in workflow under test.
					command := exec.CommandContext(t.Context(), "jq", matches[0][1], matches[0][2])
					command.Stdin = strings.NewReader(tc.input)
					output, err := command.CombinedOutput()
					if tc.want == "" {
						if err == nil {
							t.Fatalf("invalid execution passed: %s", output)
						}
					} else if err != nil || strings.TrimSuffix(string(output), "\n") != tc.want {
						t.Fatalf("terminal text = %q, err = %v; want %q", output, err, tc.want)
					}
				})
			}
		})
	}
}
