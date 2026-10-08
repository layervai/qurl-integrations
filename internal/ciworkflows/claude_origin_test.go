package ciworkflows

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Both Claude workflows accept origin after the action only as the local
// snapshot or as the one remote the action writes for this repository. That
// second arm is a pattern, and a pattern that is looser than the URL grammar
// git uses accepts a remote that addresses another host. These tests execute
// the pattern from each workflow file against the shapes that matter.
const (
	claudeOriginPatternStart = `origin_host="${GITHUB_SERVER_URL:-}"`
	claudeOriginPatternEnd   = `action_origin_re="^https://x-access-token:`
)

func claudeOriginPatternScript(t *testing.T, workflow string) string {
	t.Helper()

	text := string(readWorkflowBytes(t, workflow))
	if strings.Count(text, claudeOriginPatternStart) != 1 || strings.Count(text, claudeOriginPatternEnd) != 1 {
		t.Fatalf("%s must build the action origin pattern exactly once", workflow)
	}
	start := strings.Index(text, claudeOriginPatternStart)
	end := strings.Index(text, claudeOriginPatternEnd)
	if end < start {
		t.Fatalf("%s builds the action origin pattern before validating its host", workflow)
	}
	end += strings.Index(text[end:], "\n")
	return "set -euo pipefail\n" + text[start:end] + "\n" +
		`[[ "${ORIGIN_CANDIDATE}" =~ ${action_origin_re} ]] && echo matched || echo unmatched` + "\n"
}

func runClaudeOriginPattern(t *testing.T, script, serverURL, candidate string) (string, error) {
	t.Helper()

	// #nosec G204 -- the script is cut from a checked-in workflow file.
	command := exec.Command("bash", "-c", script)
	command.Env = append(os.Environ(),
		"GITHUB_SERVER_URL="+serverURL,
		"GITHUB_REPOSITORY=layervai/qurl-integrations",
		"ORIGIN_CANDIDATE="+candidate,
	)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func TestClaudeActionOriginPatternAddressesOnlyThisRepository(t *testing.T) {
	const repository = "github.com/layervai/qurl-integrations.git"
	cases := []struct {
		name      string
		candidate string
		want      string
	}{
		{"action remote", "https://x-access-token:ghs_Example-token.123@" + repository, "matched"},
		{"other host", "https://x-access-token:t@evil.example.com/layervai/qurl-integrations.git", "unmatched"},
		{"other repository", "https://x-access-token:t@github.com/attacker/exfil.git", "unmatched"},
		{"no credential", "https://" + repository, "unmatched"},
		{"other user", "https://evil.example.com@" + repository, "unmatched"},
		// A URL client ends the host at "#", "?" or "\": git reads each of
		// these as a host named x-access-token, not as this repository.
		{"host ended by fragment", "https://x-access-token:80#@" + repository, "unmatched"},
		{"host ended by query", "https://x-access-token:80?@" + repository, "unmatched"},
		{"host ended by backslash", `https://x-access-token:80\@` + repository, "unmatched"},
		{"second colon", "https://x-access-token:80:x@" + repository, "unmatched"},
		{"path suffix", "https://x-access-token:t@" + repository + "/extra", "unmatched"},
	}
	for _, workflow := range []string{claudeCodeReviewWorkflow, "claude.yml"} {
		script := claudeOriginPatternScript(t, workflow)
		for _, tc := range cases {
			t.Run(workflow+"/"+tc.name, func(t *testing.T) {
				output, err := runClaudeOriginPattern(t, script, "https://github.com", tc.candidate)
				if err != nil {
					t.Fatalf("pattern script failed: %v\n%s", err, output)
				}
				if output != tc.want {
					t.Fatalf("candidate %q: got %q, want %q", tc.candidate, output, tc.want)
				}
			})
		}
	}
}

func TestClaudeActionOriginPatternFailsClosedWithoutARunnerHost(t *testing.T) {
	for _, workflow := range []string{claudeCodeReviewWorkflow, "claude.yml"} {
		script := claudeOriginPatternScript(t, workflow)
		for _, serverURL := range []string{"", "https://", "http://github.com", "https://github.com/extra"} {
			t.Run(workflow+"/"+serverURL, func(t *testing.T) {
				output, err := runClaudeOriginPattern(t, script, serverURL,
					"https://x-access-token:t@github.com/layervai/qurl-integrations.git")
				if err == nil {
					t.Fatalf("server URL %q was accepted: %s", serverURL, output)
				}
				if !strings.Contains(output, "origin comparison targets are unavailable") {
					t.Fatalf("server URL %q: got %q, want the fail-closed error", serverURL, output)
				}
			})
		}
	}
}
