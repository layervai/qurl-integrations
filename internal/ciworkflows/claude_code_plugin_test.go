package ciworkflows

import (
	"bufio"
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Claude Code plugin is installed straight from this repository: a person
// runs `/plugin marketplace add layervai/qurl-integrations` and then
// `/plugin install qurl@layerv`, and Claude Code reads the two JSON files
// below from main. Nothing builds or releases them, so these tests are the
// only check they get before they reach people. They run offline on every PR
// (workflow-contract.yml runs this package unfiltered). Whether the bundled
// skill is still the served one cannot be known offline, so that is checked
// every day by claudeCodePluginSkillWorkflow instead.
//
// `claude plugin validate` is the authoritative check, and it is not run here:
// CI has no Claude Code. The rules below are the ones the Claude Code plugin
// reference gives, for the fields this plugin uses.
//
// TODO(upstream-contract): the file layout, the field names and the naming
// rules are Claude Code's; when its plugin reference changes them, change them
// here.
const (
	claudeCodeMarketplaceFile     = ".claude-plugin/marketplace.json"
	claudeCodeMarketplaceName     = "layerv"
	claudeCodePluginName          = "qurl"
	claudeCodePluginSource        = "./plugins/claude-code/qurl"
	claudeCodeBundledSkill        = "plugins/claude-code/qurl/skills/qurl/SKILL.md"
	claudeCodePluginSkillWorkflow = "claude-code-plugin-skill.yml"
	claudeCodePluginSkillStep     = "Verify the plugin's skill is the served skill"
)

type claudeCodeMarketplace struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"owner"`
	Plugins []struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		Description string `json:"description"`
		Category    string `json:"category"`
	} `json:"plugins"`
}

type claudeCodePluginManifest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"author"`
	Homepage   string   `json:"homepage"`
	Repository string   `json:"repository"`
	License    string   `json:"license"`
	Keywords   []string `json:"keywords"`
}

var (
	// semverPattern is Semantic Versioning 2.0.0's own expression. Claude
	// Code takes any string, but it installs a new copy only when the string
	// changes, so the plugin keeps to one that says which way it moved.
	semverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	// claudeCodeNamePattern is the marketplace reference's rule for a
	// marketplace name and a plugin entry's name.
	claudeCodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// readRepoJSON decodes a checked-in JSON file into out, refusing a field out
// does not declare: Claude Code drops an unknown field without a word, so a
// misspelled one would otherwise load as if it were not there.
func readRepoJSON(t *testing.T, name string, out any) {
	t.Helper()
	// #nosec G304 -- a fixed path in this repository.
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("%s is not the JSON this test expects (a field it does not know is an error; add the field to the struct here once the plugin reference says Claude Code reads it): %v", name, err)
	}
	if decoder.More() {
		t.Fatalf("%s holds more than one JSON value", name)
	}
}

func TestClaudeCodeMarketplaceListsThePlugin(t *testing.T) {
	t.Parallel()

	var marketplace claudeCodeMarketplace
	readRepoJSON(t, claudeCodeMarketplaceFile, &marketplace)

	// The install command people are given names the marketplace.
	if marketplace.Name != claudeCodeMarketplaceName {
		t.Errorf("marketplace name = %q, want %q: the documented install command is /plugin install %s@%s",
			marketplace.Name, claudeCodeMarketplaceName, claudeCodePluginName, claudeCodeMarketplaceName)
	}
	if !claudeCodeNamePattern.MatchString(marketplace.Name) || strings.Contains(marketplace.Name, "..") {
		t.Errorf("marketplace name %q breaks Claude Code's naming rule", marketplace.Name)
	}
	if strings.TrimSpace(marketplace.Owner.Name) == "" {
		t.Error("marketplace owner.name is required")
	}
	if strings.TrimSpace(marketplace.Description) == "" {
		t.Error("marketplace description is empty; claude plugin validate warns about it")
	}
	if len(marketplace.Plugins) != 1 {
		t.Fatalf("marketplace lists %d plugins, want the one qURL plugin", len(marketplace.Plugins))
	}
	entry := marketplace.Plugins[0]
	if entry.Name != claudeCodePluginName || !claudeCodeNamePattern.MatchString(entry.Name) {
		t.Errorf("plugin entry name = %q, want %q", entry.Name, claudeCodePluginName)
	}
	if entry.Source != claudeCodePluginSource {
		t.Errorf("plugin entry source = %q, want %q", entry.Source, claudeCodePluginSource)
	}
	// A relative source resolves from the repository root and may not leave
	// it; one that names no directory passes validation and fails at install.
	if !strings.HasPrefix(entry.Source, "./") || slices.Contains(strings.Split(entry.Source, "/"), "..") {
		t.Errorf("plugin entry source %q is not a ./ path inside the repository", entry.Source)
	}
	if info, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(entry.Source))); err != nil || !info.IsDir() {
		t.Errorf("plugin entry source %q is not a directory in this repository: %v", entry.Source, err)
	}
	if strings.TrimSpace(entry.Description) == "" {
		t.Error("plugin entry description is empty; it is the line people see in /plugin")
	}
}

func TestClaudeCodePluginManifest(t *testing.T) {
	t.Parallel()

	var manifest claudeCodePluginManifest
	readRepoJSON(t, path.Join(strings.TrimPrefix(claudeCodePluginSource, "./"), ".claude-plugin", "plugin.json"), &manifest)

	// The entry name is what people install; the manifest name prefixes the
	// plugin's skills. Claude Code says to keep them the same.
	if manifest.Name != claudeCodePluginName {
		t.Errorf("plugin.json name = %q, want %q, the marketplace entry's name", manifest.Name, claudeCodePluginName)
	}
	// The version lives here and only here: when the marketplace entry sets
	// one too, plugin.json wins and claude plugin validate warns.
	if !semverPattern.MatchString(manifest.Version) {
		t.Errorf("plugin.json version %q is not a semantic version", manifest.Version)
	}
	if strings.TrimSpace(manifest.Description) == "" || strings.TrimSpace(manifest.Author.Name) == "" {
		t.Errorf("plugin.json needs a description and author.name: description=%q author.name=%q", manifest.Description, manifest.Author.Name)
	}
	// A homepage that does not parse as a URL keeps the plugin from loading.
	for field, value := range map[string]string{"homepage": manifest.Homepage, "repository": manifest.Repository, "author.url": manifest.Author.URL} {
		if parsed, err := url.Parse(value); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			t.Errorf("plugin.json %s %q is not an https URL", field, value)
		}
	}
	// The plugin ships under the repository's license.
	license, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	firstLine, _, _ := strings.Cut(string(license), "\n")
	if want := strings.TrimSuffix(strings.TrimSpace(firstLine), " License"); manifest.License != want {
		t.Errorf("plugin.json license = %q, want %q from LICENSE", manifest.License, want)
	}
}

// TestClaudeCodePluginSkills reads each skill's frontmatter. The bundled skill
// is the served file and is not held to anything here but its name, which
// sets the command people type; the plugin's own two commands must run only
// when a person types them, so they never load into a session on their own
// and never add to its context.
func TestClaudeCodePluginSkills(t *testing.T) {
	t.Parallel()

	skillsDir := filepath.Join("..", "..", filepath.FromSlash(strings.TrimPrefix(claudeCodePluginSource, "./")), "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatalf("read %s: %v", skillsDir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if want := []string{"approve", "qurl", "share"}; !slices.Equal(names, want) {
		t.Fatalf("plugin skills = %v, want %v; a new one needs its expectations here", names, want)
	}

	for _, name := range names {
		frontmatter := readSkillFrontmatter(t, filepath.Join(skillsDir, name, "SKILL.md"))
		description, _ := frontmatter["description"].(string)
		if strings.TrimSpace(description) == "" {
			t.Errorf("skill %s has no description", name)
		}
		if name == "qurl" {
			if frontmatter["name"] != "qurl" {
				t.Errorf("the bundled skill's name = %v, want qurl", frontmatter["name"])
			}
			continue
		}
		if frontmatter["disable-model-invocation"] != true {
			t.Errorf("skill %s does not set disable-model-invocation: true", name)
		}
		if _, ok := frontmatter["argument-hint"].(string); !ok {
			t.Errorf("skill %s has no argument-hint", name)
		}
		// The rules live in the bundled skill; a command points to it.
		body, err := os.ReadFile(filepath.Join(skillsDir, name, "SKILL.md")) // #nosec G304 -- a fixed path in this repository
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte("`qurl:qurl`")) {
			t.Errorf("skill %s does not send the agent to the bundled skill, qurl:qurl", name)
		}
		// CLIs before 3.4.0 publish a resource that anyone with its address
		// can open, and 3.4.0 itself was never published: a command that
		// accepts an older CLI can make a share public that the user was
		// told is private.
		if !bytes.Contains(body, []byte("older than "+claudeCodePluginMinCLI)) {
			t.Errorf("skill %s does not require the qURL CLI %s or newer", name, claudeCodePluginMinCLI)
		}
	}
}

// claudeCodePluginMinCLI is the oldest qURL CLI the plugin's commands accept:
// the first published release whose publish is private by default and which
// has `qurl approve`.
const claudeCodePluginMinCLI = "3.4.1"

// readSkillFrontmatter returns the YAML between the `---` lines that open a
// SKILL.md. Claude Code reads frontmatter only when the first line is `---`.
func readSkillFrontmatter(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(name) // #nosec G304 -- a path under the plugin directory
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !scanner.Scan() || scanner.Text() != "---" {
		t.Fatalf("%s does not open with a --- line", name)
	}
	var lines []string
	closed := false
	for scanner.Scan() {
		if scanner.Text() == "---" {
			closed = true
			break
		}
		lines = append(lines, scanner.Text())
	}
	if !closed {
		t.Fatalf("%s opens frontmatter and never closes it", name)
	}
	frontmatter := map[string]any{}
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &frontmatter); err != nil {
		t.Fatalf("%s frontmatter is not YAML (Claude Code would load the skill with no fields): %v", name, err)
	}
	return frontmatter
}

// TestClaudeCodeBundledSkillIsKeptByteForByte holds the checkout to the bytes
// the daily check compares: git's line-ending conversion on a Windows
// checkout, a plugin install among them, would otherwise change them.
func TestClaudeCodeBundledSkillIsKeptByteForByte(t *testing.T) {
	t.Parallel()

	attributes, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}
	if !slices.Contains(strings.Split(string(attributes), "\n"), claudeCodeBundledSkill+" -text") {
		t.Errorf(".gitattributes has no %q line", claudeCodeBundledSkill+" -text")
	}
}

// TestClaudeCodePluginSkillRunsDailyWithoutGating holds the daily comparison
// to the shape of the served-skill check it copies (see
// TestServedSkillRunsDailyWithoutGating for why each part matters), and to
// the bundled skill's path, so a moved file cannot leave it comparing nothing.
func TestClaudeCodePluginSkillRunsDailyWithoutGating(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, claudeCodePluginSkillWorkflow)
	triggers := parseWorkflowTriggers(t, claudeCodePluginSkillWorkflow, workflow.On)
	if got := slices.Sorted(maps.Keys(triggers)); !slices.Equal(got, []string{"schedule", "workflow_dispatch"}) {
		t.Errorf("%s triggers = %v, want schedule and workflow_dispatch only", claudeCodePluginSkillWorkflow, got)
	}
	if schedule, _ := triggers["schedule"].([]any); len(schedule) != 1 {
		t.Errorf("%s schedule = %v, want one cron entry", claudeCodePluginSkillWorkflow, triggers["schedule"])
	}
	uncommented := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAll(readWorkflowBytes(t, claudeCodePluginSkillWorkflow), nil)
	if key := regexp.MustCompile(`\b(continue-on-error|defaults)\s*:`).FindSubmatch(uncommented); key != nil {
		t.Errorf("%s uses %s, which can let a failing check end green", claudeCodePluginSkillWorkflow, key[1])
	}
	job := workflow.Jobs["check"]
	if job == nil || len(workflow.Jobs) != 1 {
		t.Fatalf("%s jobs = %v, want only check", claudeCodePluginSkillWorkflow, slices.Sorted(maps.Keys(workflow.Jobs)))
	}
	if job.If != "" || job.Needs != nil || len(job.Steps) != 3 {
		t.Fatalf("check is conditional, waits on another job, or has %d steps, not checkout, setup-go and the check", len(job.Steps))
	}
	source, toolchain, guard := &job.Steps[0], &job.Steps[1], &job.Steps[2]
	if !strings.HasPrefix(source.Uses, checkoutActionPrefix) || source.If != "" ||
		len(source.With) != 1 || source.With["persist-credentials"] != false {
		t.Errorf("check does not start with a plain checkout of the run's own ref: uses=%q if=%q with=%v", source.Uses, source.If, source.With)
	}
	if !strings.HasPrefix(toolchain.Uses, setupGoActionPrefix) || toolchain.If != "" {
		t.Errorf("check does not set up the repository's Go before it runs: uses=%q if=%q", toolchain.Uses, toolchain.If)
	}
	if guard.Name != claudeCodePluginSkillStep || guard.If != "" || (guard.Shell != "" && guard.Shell != "bash") {
		t.Errorf("the last step is not an unconditional %q: name=%q if=%q shell=%q", claudeCodePluginSkillStep, guard.Name, guard.If, guard.Shell)
	}
	lines := strings.Split(strings.TrimSpace(guard.Run), "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	if len(lines) != 5 || lines[0] != "set -euo pipefail" ||
		lines[1] != "go run ./"+servedSkillCheckerDir+" -bundled "+claudeCodeBundledSkill+" || {" ||
		!strings.HasPrefix(lines[2], `echo "::error::`) || !strings.HasSuffix(lines[2], `" >&2`) ||
		lines[3] != "exit 1" || lines[4] != "}" {
		t.Errorf("the bundled-skill step is not: strict mode, the checker on %s, and on its failure one annotation and exit 1:\n%s", claudeCodeBundledSkill, guard.Run)
	}
	if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(claudeCodeBundledSkill))); err != nil {
		t.Errorf("the bundled skill the workflow compares is not at %s: %v", claudeCodeBundledSkill, err)
	}
}
