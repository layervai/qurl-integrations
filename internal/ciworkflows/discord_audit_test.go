package ciworkflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDiscordAuditRetryBudgetFitsStepTimeout(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, "discord.yml")
	job := workflow.Jobs["build-and-test"]
	if job == nil {
		t.Fatal("discord.yml is missing build-and-test job")
	}
	var timeoutMinutes int
	foundAuditStep := false
	for i := range job.Steps {
		if job.Steps[i].Name != "Audit dependencies" {
			continue
		}
		if !strings.Contains(job.Steps[i].Run, "audit-production-dependencies.js") {
			t.Fatalf("Audit dependencies step no longer runs the retry wrapper: %q", job.Steps[i].Run)
		}
		foundAuditStep = true
		value, ok := job.Steps[i].TimeoutMinutes.(int)
		if !ok {
			t.Fatalf("Audit dependencies timeout-minutes = %#v, want integer", job.Steps[i].TimeoutMinutes)
		}
		timeoutMinutes = value
		break
	}
	if !foundAuditStep {
		t.Fatal("discord.yml is missing the Audit dependencies step")
	}
	if timeoutMinutes == 0 {
		t.Fatal("discord.yml Audit dependencies step is missing a positive timeout-minutes")
	}

	scriptPath := filepath.Join("..", "..", "apps", "discord", "scripts", "audit-production-dependencies.js")
	source, err := os.ReadFile(scriptPath) //nolint:gosec // G304: fixed checked-in path.
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	totalBudget := parseDiscordAuditMilliseconds(t, source, `TOTAL_RETRY_BUDGET_MS\s*=\s*([0-9_]+)`)
	attemptTimeout := parseDiscordAuditMilliseconds(t, source, `ATTEMPT_TIMEOUT_MS\s*=\s*([0-9_]+)`)
	retryDelays := parseDiscordAuditMillisecondList(t, source,
		`(?s)RETRY_DELAYS_MS\s*=\s*Object\.freeze\(\[([^]]*)\]\)`)
	computedBudget := (len(retryDelays) + 1) * attemptTimeout
	for _, delay := range retryDelays {
		computedBudget += delay
	}
	if totalBudget != computedBudget {
		t.Fatalf("Discord audit TOTAL_RETRY_BUDGET_MS = %d, computed from attempts and delays = %d", totalBudget, computedBudget)
	}

	stepBudget := timeoutMinutes * 60_000
	if totalBudget >= stepBudget {
		t.Fatalf("Discord audit retry budget %dms must fit inside workflow step timeout %dms", totalBudget, stepBudget)
	}
}

func parseDiscordAuditMillisecondList(t *testing.T, source []byte, pattern string) []int {
	t.Helper()
	match := regexp.MustCompile(pattern).FindSubmatch(source)
	if len(match) != 2 {
		t.Fatalf("could not parse Discord audit timing list with %q", pattern)
	}
	values := strings.Split(string(match[1]), ",")
	parsed := make([]int, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parsed = append(parsed, parseDiscordAuditInteger(t, value))
	}
	if len(parsed) == 0 {
		t.Fatal("Discord audit retry delay list must not be empty")
	}
	return parsed
}

func parseDiscordAuditMilliseconds(t *testing.T, source []byte, pattern string) int {
	t.Helper()
	match := regexp.MustCompile(pattern).FindSubmatch(source)
	if len(match) != 2 {
		t.Fatalf("could not parse Discord audit timing with %q", pattern)
	}
	return parseDiscordAuditInteger(t, string(match[1]))
}

func parseDiscordAuditInteger(t *testing.T, value string) int {
	t.Helper()
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "_", "")
	parsed, err := strconv.Atoi(normalized)
	if err != nil {
		t.Fatalf("parse Discord audit integer %q: %v", value, err)
	}
	return parsed
}
