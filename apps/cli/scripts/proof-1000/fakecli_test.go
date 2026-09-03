package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// The tests drive the harness against this test binary re-executed as a
// fake qurl: runCLI receives os.Args[0] as the binary and an environment
// carrying PROOF_FAKE_SCRIPT, and TestMain answers from that script before
// any test runs. This is cross-platform (no shell), so the Windows matrix
// exercises the same paths.
const (
	fakeCLIEnv    = "PROOF_FAKE_SCRIPT"
	fakeCLIStates = "PROOF_FAKE_STATE"
)

// fakeRule answers the first invocation whose joined arguments contain
// Match. Times bounds how many invocations it answers (0 = unlimited) so a
// script can express "fail twice, then succeed".
type fakeRule struct {
	Match  string `json:"match"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
	Times  int    `json:"times"`
}

func TestMain(m *testing.M) {
	if script := os.Getenv(fakeCLIEnv); script != "" {
		os.Exit(runFakeCLI(script, os.Getenv(fakeCLIStates), os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakeCLI(scriptPath, stateDir string, args []string) int {
	raw, err := os.ReadFile(filepath.Clean(scriptPath))
	if err != nil {
		_, _ = os.Stderr.WriteString("fake qurl: " + err.Error() + "\n")
		return 99
	}
	var rules []fakeRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		_, _ = os.Stderr.WriteString("fake qurl: " + err.Error() + "\n")
		return 99
	}
	joined := strings.Join(args, " ")
	for i, rule := range rules {
		if !strings.Contains(joined, rule.Match) {
			continue
		}
		if rule.Times > 0 && !consumeFakeRule(stateDir, i, rule.Times) {
			continue
		}
		_, _ = os.Stdout.WriteString(rule.Stdout)
		_, _ = os.Stderr.WriteString(rule.Stderr)
		return rule.Exit
	}
	_, _ = os.Stderr.WriteString("Error: fake qurl has no rule for: " + joined + "\n")
	return 98
}

// consumeFakeRule counts invocations per rule across processes.
func consumeFakeRule(stateDir string, rule, times int) bool {
	if stateDir == "" {
		return true
	}
	path := filepath.Join(stateDir, "rule-"+strconv.Itoa(rule))
	used := 0
	if raw, err := os.ReadFile(filepath.Clean(path)); err == nil {
		used, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if used >= times {
		return false
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(used+1)), 0o600)
	return true
}

// fakeEnvironment builds an environment whose every CLI call is answered by
// the given rules. State dir and log dir are empty temp directories, so the
// daemon socket is absent and the registry is empty unless a test writes one.
func fakeEnvironment(t *testing.T, rules []fakeRule) *environment {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "script.json")
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	states := filepath.Join(dir, "rule-state")
	if err := os.MkdirAll(states, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cover"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &environment{
		StateDir:   stateDir,
		SocketPath: connectordaemon.StateSocketPath(stateDir),
		LogDir:     filepath.Join(dir, "logs"),
		QurlBin:    os.Args[0],
		ConsumeBin: os.Args[0],
		// GOCOVERDIR keeps a -cover test binary from printing its "no coverage
		// data emitted" warning on stderr, which would read as a CLI error line.
		childEnv: []string{fakeCLIEnv + "=" + script, fakeCLIStates + "=" + states, "PATH=" + os.Getenv("PATH"), "GOCOVERDIR=" + filepath.Join(dir, "cover")},
		redactor: newRedactor("/home/nobody"),
	}
}

// writeRegistry writes a v2 local share registry fixture the CLI's own
// reader accepts, keyed by resource id exactly as the CLI writes it.
func writeRegistry(t *testing.T, stateDir string, rows ...connectorstate.LocalShare) {
	t.Helper()
	shares := map[string]connectorstate.LocalShare{}
	for i := range rows {
		row := &rows[i]
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = time.Now().UTC()
		}
		if row.DesiredState == "" {
			row.DesiredState = "on"
		}
		shares[row.ResourceID] = *row
	}
	raw, err := json.Marshal(map[string]any{"version": 2, "owner_id": "auth0|fixture", "shares": shares})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, connectorstate.LocalSharesFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeOptions(t *testing.T, runName string, n int) *options {
	t.Helper()
	opts, err := parseOptions([]string{"--run", runName, "--n", strconv.Itoa(n), "--out", t.TempDir(), "--concurrency", "2", "--publish-retries", "2"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func publishOK(id, crid string, existing bool) fakeRule {
	return fakeRule{
		Match:  " --id " + id + " ",
		Stdout: `{"crid":"` + crid + `","resource_id":"RID-` + id + `","target_url":"http://127.0.0.1:1","status":"serving","found_existing":` + strconv.FormatBool(existing) + "}\n",
		Stderr: "[debug] > GET /v1/me\n[debug] < HTTP 200\n[debug] > PUT /v1/resources/" + crid + "/sharing\n[debug] < HTTP 200\n",
	}
}
