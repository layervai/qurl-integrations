package ciworkflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	// Accept environment prefixes, quoted shell snippets, Docker heredocs, and
	// supported npm options before the subcommand. The explicit path-scoping
	// forms consume their separate value; other supported flags are value-less
	// or use --flag=value. unclassifiedNPMInvocations makes an unknown form
	// fail closed rather than silently escaping this matcher.
	npmInstallCommand  = regexp.MustCompile(`(?m)\bnpm\b[ \t]+(?:(?:(?:--(?:prefix|workspace|registry|cache|userconfig)|-[wC])[ \t]+[^ \t&|;"'\r\n]+|--?[^ \t&|;"'\r\n]+)[ \t]+)*(ci|install|i)\b[^&|;\r\n]*`)
	noAuditFlag        = regexp.MustCompile(`(?:^|[ \t"'])--no-audit(?:$|[ \t"'\\])`)
	shellContinuation  = regexp.MustCompile(`\\\r?\n[ \t]*`)
	npmInvocation      = regexp.MustCompile(`(?m)\bnpm\b[ \t]+[^&|;\r\n]*`)
	npmNonInstall      = regexp.MustCompile(`^npm[ \t]+(?:run|test|cache|audit|exec|version|--version)\b`)
	directAuditCommand = regexp.MustCompile(
		`(?m)\b(?:npm[ \t]+audit\b|node[ \t]+scripts/audit-production-dependencies\.js\b)[^&|;\r\n]*`,
	)
	trailingShellComment = regexp.MustCompile(`[ \t]+#.*$`)
)

const discordAuditStepName = "Audit dependencies"

func findNPMInstallCommands(source string) [][]string {
	return npmInstallCommand.FindAllStringSubmatch(source, -1)
}

func hasNoAuditFlag(command string) bool {
	return noAuditFlag.MatchString(trailingShellComment.ReplaceAllString(command, ""))
}

func findDirectAuditCommands(source string) []string {
	var commands []string
	for line := range strings.SplitSeq(source, "\n") {
		withoutComment := trailingShellComment.ReplaceAllString(line, "")
		commands = append(commands, directAuditCommand.FindAllString(withoutComment, -1)...)
	}
	return commands
}

func logicalShellSource(source string) string {
	var uncommented strings.Builder
	for line := range strings.SplitSeq(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		uncommented.WriteString(line)
		uncommented.WriteByte('\n')
	}
	return shellContinuation.ReplaceAllString(uncommented.String(), " ")
}

func unclassifiedNPMInvocations(source string) []string {
	var unclassified []string
	for _, invocation := range npmInvocation.FindAllString(source, -1) {
		trimmed := strings.TrimSpace(invocation)
		if npmInstallCommand.MatchString(trimmed) || npmNonInstall.MatchString(trimmed) {
			continue
		}
		unclassified = append(unclassified, trimmed)
	}
	return unclassified
}

func checkNPMInvocationsClassified(t *testing.T, location, source string) {
	t.Helper()
	if unclassified := unclassifiedNPMInvocations(source); len(unclassified) > 0 {
		t.Errorf("%s contains npm invocation(s) the dependency-audit contract cannot classify; extend the parser before merging: %q", location, unclassified)
	}
}

func TestDiscordNPMInstallCommandShapes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		source     string
		mode       string
		hasNoAudit bool
	}{
		"environment prefix":             {`NODE_ENV=production npm ci --no-audit`, "ci", true},
		"npm prefix option":              {`npm --prefix apps/discord ci --no-audit`, "ci", true},
		"npm registry option":            {`npm --registry https://registry.example ci --no-audit`, "ci", true},
		"npm cache option":               {`npm --cache /tmp/npm ci --no-audit`, "ci", true},
		"npm userconfig option":          {`npm --userconfig .npmrc ci --no-audit`, "ci", true},
		"npm short workspace":            {`npm -w apps/discord ci --no-audit`, "ci", true},
		"npm short global":               {`npm -g i --no-audit`, "i", true},
		"quoted shell":                   {`sh -c "npm ci --no-audit"`, "ci", true},
		"Docker heredoc":                 {"RUN <<EOF\nnpm ci --no-audit\nEOF", "ci", true},
		"comment before continuation":    {"# example \\\nRUN npm ci --no-audit", "ci", true},
		"continued production chain":     {"npm ci --omit=dev --ignore-scripts --no-audit \\\n  && npm cache clean --force", "ci", true},
		"bare install":                   {`npm ci`, "ci", false},
		"non-lockfile install":           {`npm install --no-audit`, "install", true},
		"inverted no-audit flag":         {`npm ci --no-audit=false`, "ci", false},
		"environment audit suppression":  {`NPM_CONFIG_AUDIT=false npm ci`, "ci", false},
		"trailing comment is not a flag": {`npm ci # --no-audit is configured elsewhere`, "ci", false},
		"flag before trailing comment":   {`npm ci --no-audit # explicit suppression`, "ci", true},
		"quoted no-audit flag":           {`npm ci "--no-audit"`, "ci", true},
		"plural lookalike flag":          {`npm ci --no-audits`, "ci", false},
	}

	t.Run("unknown npm option cannot hide an install", func(t *testing.T) {
		t.Parallel()
		source := logicalShellSource(`npm --future-option value ci --no-audit`)
		if got := unclassifiedNPMInvocations(source); len(got) != 1 {
			t.Fatalf("unclassified npm invocations = %q, want the future-option install", got)
		}
	})
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			installs := findNPMInstallCommands(logicalShellSource(test.source))
			if len(installs) != 1 {
				t.Fatalf("install matches = %d, want 1 for %q", len(installs), test.source)
			}
			if installs[0][1] != test.mode {
				t.Fatalf("install command = %q, want %q", installs[0][1], test.mode)
			}
			if got := hasNoAuditFlag(installs[0][0]); got != test.hasNoAudit {
				t.Fatalf("hasNoAuditFlag(%q) = %t, want %t", installs[0][0], got, test.hasNoAudit)
			}
		})
	}

	falsePositives := map[string]string{
		"run script":    `npm run ci`,
		"test ci flag":  `npm test -- --ci --silent`,
		"npx command":   `npx jest --ci`,
		"shell comment": `# npm install --no-audit`,
	}
	for name, source := range falsePositives {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if installs := findNPMInstallCommands(logicalShellSource(source)); len(installs) != 0 {
				t.Fatalf("install matches = %q, want none for %q", installs, source)
			}
		})
	}
}

func TestDiscordDirectAuditCommandShapes(t *testing.T) {
	t.Parallel()

	for name, source := range map[string]string{
		"plain":              `npm audit --json`,
		"environment prefix": `NPM_CONFIG_LOGLEVEL=silly npm audit --json`,
		"single pipe":        `printf ready | npm audit --json`,
		"quoted shell":       `sh -c "npm audit signatures"`,
		"command wrapper":    `time npm audit --json`,
		"wrapper":            `node scripts/audit-production-dependencies.js`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := findDirectAuditCommands(logicalShellSource(source)); len(got) != 1 {
				t.Fatalf("direct audit matches = %q, want one for %q", got, source)
			}
		})
	}

	for name, source := range map[string]string{
		"install":          `npm ci --no-audit`,
		"line comment":     `# npm audit --json`,
		"trailing comment": `npm ci --no-audit # npm audit runs separately`,
	} {
		t.Run("no match: "+name, func(t *testing.T) {
			t.Parallel()
			if got := findDirectAuditCommands(logicalShellSource(source)); len(got) != 0 {
				t.Fatalf("direct audit matches = %q, want none for %q", got, source)
			}
		})
	}
}

// TestDiscordDependencyAuditContract keeps dependency installation separate
// from the one explicit, production-only audit gate. npm ci enables its own
// registry audit by default; leaving that enabled duplicates the
// network-dependent request and can stall both the test job and image build
// before the intentional gate reports a useful result.
//
// The audit count covers direct shell invocations of npm audit and the
// repository's Node wrapper. It does not infer audits hidden behind package
// scripts (including install/prepare hooks), npx commands, or uses actions.
// Every direct npm audit kind, including signatures, is deliberately
// review-gated by the count below.
// Each install must carry a literal --no-audit: NPM_CONFIG_AUDIT=false and an
// .npmrc audit=false default intentionally do not satisfy this contract.
func TestDiscordDependencyAuditContract(t *testing.T) {
	t.Parallel()

	spec := requiredWorkflowSpecByName(t, "discord")
	appPath := filepath.Join("..", "..", "apps", "discord")

	t.Run("workflow", func(t *testing.T) {
		t.Parallel()

		workflow := readWorkflow(t, spec.path)
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
				logicalRun := logicalShellSource(current.Run)
				installs := findNPMInstallCommands(logicalRun)
				location := fmt.Sprintf("%s job %q step %q", spec.path, jobID, current.Name)
				checkNPMInvocationsClassified(t, location, logicalRun)
				installCount += checkLockfileInstalls(t, location, installs)
				directAuditCommandCount += len(findDirectAuditCommands(logicalRun))
				if current.Name == discordAuditStepName {
					if audit != nil {
						t.Fatalf("%s has more than one %q step", spec.path, discordAuditStepName)
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
			t.Fatalf("%s is missing the %q step", spec.path, discordAuditStepName)
		}
		auditFields := strings.Fields(audit.Run)
		if len(auditFields) < 2 || auditFields[0] != "node" || auditFields[1] != "scripts/audit-production-dependencies.js" {
			t.Errorf("Discord workflow explicit audit = %q, want an invocation of %q", audit.Run, explicitAudit)
		}
	})

	t.Run("dockerfile", func(t *testing.T) {
		t.Parallel()

		dockerfilePath := filepath.Join(appPath, "Dockerfile")
		dockerfile, err := os.ReadFile(dockerfilePath) //nolint:gosec // G304: fixed checked-in path.
		if err != nil {
			t.Fatalf("read %s: %v", dockerfilePath, err)
		}
		logicalDockerfile := logicalShellSource(string(dockerfile))
		checkNPMInvocationsClassified(t, "Discord Dockerfile", logicalDockerfile)
		dockerInstallCount := checkLockfileInstalls(
			t,
			"Discord Dockerfile",
			findNPMInstallCommands(logicalDockerfile),
		)
		if dockerInstallCount == 0 {
			t.Fatalf("Discord Dockerfile has no npm dependency-install commands")
		}
	})

	t.Run("Makefile mirrors", func(t *testing.T) {
		t.Parallel()

		makefilePath := filepath.Join("..", "..", "Makefile")
		makefile, err := os.ReadFile(makefilePath) //nolint:gosec // G304: fixed checked-in path.
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
				logicalRecipe := logicalShellSource(string(recipe[1]))
				installs := findNPMInstallCommands(logicalRecipe)
				location := fmt.Sprintf("Makefile target %q", target)
				checkNPMInvocationsClassified(t, location, logicalRecipe)
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
		if !hasNoAuditFlag(install[0]) {
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
		if job.Steps[i].Name != discordAuditStepName {
			continue
		}
		if !strings.Contains(job.Steps[i].Run, "audit-production-dependencies.js") {
			t.Fatalf("%s %q step no longer runs the retry wrapper: %q", spec.path, discordAuditStepName, job.Steps[i].Run)
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
		t.Fatalf("%s is missing the %q step", spec.path, discordAuditStepName)
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
