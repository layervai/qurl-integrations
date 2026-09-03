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
	for i := range job.Steps {
		if job.Steps[i].Name == "Audit dependencies" {
			value, ok := job.Steps[i].TimeoutMinutes.(int)
			if !ok {
				t.Fatalf("Audit dependencies timeout-minutes = %#v, want integer", job.Steps[i].TimeoutMinutes)
			}
			timeoutMinutes = value
			break
		}
	}
	if timeoutMinutes == 0 {
		t.Fatal("discord.yml Audit dependencies step is missing a positive timeout-minutes")
	}

	scriptPath := filepath.Join("..", "..", "apps", "discord", "scripts", "audit-production-dependencies.js")
	source, err := os.ReadFile(scriptPath) // #nosec G304 -- fixed checked-in path.
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	attemptTimeout := parseDiscordAuditMilliseconds(t, source, `ATTEMPT_TIMEOUT_MS\s*=\s*([0-9_]+)`)
	delayBlock := regexp.MustCompile(`RETRY_DELAYS_MS\s*=\s*Object\.freeze\(\[([^]]+)\]\)`).FindSubmatch(source)
	if len(delayBlock) != 2 {
		t.Fatal("could not parse RETRY_DELAYS_MS from Discord audit wrapper")
	}
	totalBudget := 0
	delays := strings.Split(string(delayBlock[1]), ",")
	for _, delay := range delays {
		totalBudget += parseDiscordAuditInteger(t, delay)
	}
	totalBudget += (len(delays) + 1) * attemptTimeout

	stepBudget := timeoutMinutes * 60_000
	if totalBudget >= stepBudget {
		t.Fatalf("Discord audit retry budget %dms must fit inside workflow step timeout %dms", totalBudget, stepBudget)
	}
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
