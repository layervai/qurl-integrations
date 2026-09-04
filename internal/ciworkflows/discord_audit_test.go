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
// The install scan covers direct npm ci/install/i commands at shell-command
// boundaries and in Docker RUN instructions. Environment prefixes, npm global
// flags, shell wrappers, and Docker heredocs are outside that deliberately
// shape-sensitive contract.
//
// The audit count covers direct shell invocations of npm audit and the
// repository's Node wrapper. It does not infer audits hidden behind package
// scripts, npx commands, or uses actions. Every direct npm audit kind,
// including signatures, is deliberately review-gated by the count below.
func TestDiscordDependencyAuditContract(t *testing.T) {
	t.Parallel()

	spec := requiredWorkflowSpecByName(t, "discord")
	appPath := filepath.Join("..", "..", "apps", spec.name)
	shellContinuation := regexp.MustCompile(`\\\r?\n[ \t]*`)
	const npmInstallPattern = `npm[ \t]+(ci|install|i)\b[^&|;\r\n]*`
	installCommand := regexp.MustCompile(`(?m)(?:^[ \t]*|(?:&&|\|\||;)[ \t]*)` + npmInstallPattern)
	dockerInstallCommand := regexp.MustCompile(`(?:^[ \t]*(?i:RUN)(?:[ \t]+--[^ \t]+)*[ \t]+|(?:&&|\|\||;)[ \t]*)` + npmInstallPattern)

	t.Run("workflow", func(t *testing.T) {
		t.Parallel()

		workflow := readWorkflow(t, spec.path)
		auditCommand := regexp.MustCompile(`(?m)(?:^[ \t]*|(?:&&|\|\||;)[ \t]*)(?:npm[ \t]+audit\b|node[ \t]+scripts/audit-production-dependencies\.js\b)[^&|;\r\n]*`)

		const auditStepName = "Audit dependencies"
		var audit *step
		installCount := 0
		directAuditCommandCount := 0
		for jobID, job := range workflow.Jobs {
			if job == nil {
				t.Errorf("%s job %q is null; cannot inspect dependency commands", spec.path, jobID)
				continue
			}
			for index := range job.Steps {
				current := &job.Steps[index]
				logicalRun := shellContinuation.ReplaceAllString(current.Run, " ")
				installs := installCommand.FindAllStringSubmatch(logicalRun, -1)
				location := fmt.Sprintf("%s job %q step %q", spec.path, jobID, current.Name)
				installCount += checkLockfileInstalls(t, location, installs)
				directAuditCommandCount += len(auditCommand.FindAllString(logicalRun, -1))
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
		if directAuditCommandCount != 1 {
			t.Errorf("Discord workflow direct audit commands = %d, want exactly one; review the contract before adding another audit kind", directAuditCommandCount)
		}

		const explicitAudit = "node scripts/audit-production-dependencies.js"
		if audit == nil {
			t.Fatalf("%s is missing the %q step", spec.path, auditStepName)
		}
		if strings.TrimSpace(audit.Run) != explicitAudit {
			t.Errorf("Discord workflow explicit audit = %q, want %q", audit.Run, explicitAudit)
		}
	})

	t.Run("dockerfile", func(t *testing.T) {
		t.Parallel()

		dockerfilePath := filepath.Join(appPath, "Dockerfile")
		dockerfile, err := os.ReadFile(dockerfilePath) // #nosec G304 -- fixed checked-in path.
		if err != nil {
			t.Fatalf("read %s: %v", dockerfilePath, err)
		}
		logicalDockerfile := shellContinuation.ReplaceAllString(string(dockerfile), " ")
		runInstructions := regexp.MustCompile(`(?im)^[ \t]*RUN\b[^\r\n]*`).FindAllString(logicalDockerfile, -1)
		dockerInstallCount := 0
		for _, instruction := range runInstructions {
			installs := dockerInstallCommand.FindAllStringSubmatch(instruction, -1)
			dockerInstallCount += checkLockfileInstalls(t, "Discord Dockerfile", installs)
		}
		if dockerInstallCount == 0 {
			t.Fatalf("Discord Dockerfile has no npm dependency-install commands")
		}
	})

	t.Run("Makefile mirrors", func(t *testing.T) {
		t.Parallel()

		makefilePath := filepath.Join("..", "..", "Makefile")
		makefile, err := os.ReadFile(makefilePath) // #nosec G304 -- fixed checked-in path.
		if err != nil {
			t.Fatalf("read %s: %v", makefilePath, err)
		}
		for _, target := range []string{"test-discord", "check-discord"} {
			t.Run(target, func(t *testing.T) {
				t.Parallel()

				pattern := `(?m)^` + regexp.QuoteMeta(target) + `:[^\r\n]*\r?\n((?:\t[^\r\n]*(?:\r?\n|$))+)`
				recipe := regexp.MustCompile(pattern).FindSubmatch(makefile)
				if len(recipe) != 2 {
					t.Fatalf("%s has no %s recipe", makefilePath, target)
				}
				logicalRecipe := shellContinuation.ReplaceAllString(string(recipe[1]), " ")
				installs := installCommand.FindAllStringSubmatch(logicalRecipe, -1)
				location := fmt.Sprintf("Makefile target %q", target)
				if checkLockfileInstalls(t, location, installs) == 0 {
					t.Fatalf("%s has no npm dependency-install commands", location)
				}
			})
		}
	})
}

func checkLockfileInstalls(t *testing.T, location string, installs [][]string) int {
	t.Helper()

	for _, install := range installs {
		if install[1] != "ci" {
			t.Errorf("%s uses non-lockfile npm %s; use npm ci for application dependencies or update the contract for an intentional tool install: %q", location, install[1], install[0])
		}
		if !slices.Contains(strings.Fields(install[0]), "--no-audit") {
			t.Errorf("%s install still runs npm's implicit audit: %q", location, install[0])
		}
	}
	return len(installs)
}

func TestDiscordAuditRetryBudgetFitsStepTimeout(t *testing.T) {
	t.Parallel()

	spec := requiredWorkflowSpecByName(t, "discord")
	workflow := readWorkflow(t, spec.path)
	job := workflow.Jobs["build-and-test"]
	if job == nil {
		t.Fatalf("%s is missing build-and-test job", spec.path)
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
		t.Fatalf("%s Audit dependencies step is missing a positive timeout-minutes", spec.path)
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
