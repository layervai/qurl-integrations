package ciworkflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestDiscordDependencyAuditContract keeps dependency installation separate
// from the one explicit, production-only audit gate. npm ci enables its own
// registry audit by default; leaving that enabled duplicates the
// network-dependent request and can stall both the test job and image build
// before the intentional gate reports a useful result.
//
// The audit count covers direct shell invocations of npm audit and the
// repository's Node wrapper. It does not infer audits hidden behind package
// scripts, npx commands, or uses actions.
func TestDiscordDependencyAuditContract(t *testing.T) {
	t.Parallel()

	spec := requiredWorkflowSpecByName(t, "discord")
	appPath := filepath.Join("..", "..", "apps", spec.name)
	workflow := readWorkflow(t, spec.path)
	shellContinuation := regexp.MustCompile(`\\\r?\n[ \t]*`)
	installCommand := regexp.MustCompile(`(?m)(?:^[ \t]*|(?:&&|\|\||;)[ \t]*)npm[ \t]+(ci|install|i)\b[^&|;\r\n]*`)
	auditCommand := regexp.MustCompile(`(?m)(?:^[ \t]*|(?:&&|\|\||;)[ \t]*)(?:npm[ \t]+audit\b|node[ \t]+scripts/audit-production-dependencies\.js\b)[^&|;\r\n]*`)

	const auditStepName = "Audit dependencies"
	var audit *step
	installCount := 0
	auditGateCount := 0
	for jobID, job := range workflow.Jobs {
		for index := range job.Steps {
			current := &job.Steps[index]
			logicalRun := shellContinuation.ReplaceAllString(current.Run, " ")
			installs := installCommand.FindAllStringSubmatch(logicalRun, -1)
			location := fmt.Sprintf("%s job %q step %q", spec.path, jobID, current.Name)
			installCount += checkLockfileInstalls(t, location, installs)
			auditGateCount += len(auditCommand.FindAllString(logicalRun, -1))
			if current.Name == auditStepName {
				if audit != nil {
					t.Fatalf("%s has more than one %q step", spec.path, auditStepName)
				}
				audit = current
			}
		}
	}
	if installCount == 0 {
		t.Fatalf("%s has no npm dependency-install commands", spec.path)
	}
	if auditGateCount != 1 {
		t.Errorf("Discord workflow explicit audit gates = %d, want exactly one", auditGateCount)
	}

	const explicitAudit = "node scripts/audit-production-dependencies.js"
	if audit == nil {
		t.Fatalf("%s is missing the %q step", spec.path, auditStepName)
	}
	if strings.TrimSpace(audit.Run) != explicitAudit {
		t.Errorf("Discord workflow explicit audit = %q, want %q", audit.Run, explicitAudit)
	}

	dockerfilePath := filepath.Join(appPath, "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath) // #nosec G304 -- fixed checked-in path.
	if err != nil {
		t.Fatalf("read %s: %v", dockerfilePath, err)
	}
	logicalDockerfile := shellContinuation.ReplaceAllString(string(dockerfile), " ")
	runInstructions := regexp.MustCompile(`(?im)^[ \t]*RUN\b[^\r\n]*`).FindAllString(logicalDockerfile, -1)
	dockerInstallCommand := regexp.MustCompile(`(?:^[ \t]*(?i:RUN)(?:[ \t]+--[^ \t]+)*[ \t]+|(?:&&|\|\||;)[ \t]*)npm[ \t]+(ci|install|i)\b[^&|;\r\n]*`)
	dockerInstallCount := 0
	for _, instruction := range runInstructions {
		installs := dockerInstallCommand.FindAllStringSubmatch(instruction, -1)
		dockerInstallCount += checkLockfileInstalls(t, "Discord Dockerfile", installs)
	}
	if dockerInstallCount == 0 {
		t.Fatalf("Discord Dockerfile has no npm dependency-install commands")
	}
}

func checkLockfileInstalls(t *testing.T, location string, installs [][]string) int {
	t.Helper()

	for _, install := range installs {
		if install[1] != "ci" {
			t.Errorf("%s uses npm %s, want npm ci: %q", location, install[1], install[0])
		}
		if !slices.Contains(strings.Fields(install[0]), "--no-audit") {
			t.Errorf("%s install still runs npm's implicit audit: %q", location, install[0])
		}
	}
	return len(installs)
}

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
