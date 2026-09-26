package ciworkflows

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This repository is PUBLIC. Its source and docs describe how the qURL
// connector talks to services that live in private repositories, and that is
// where a private repository name slips in unnoticed -- a comment in
// apps/cli/internal/connector/supervisor named the private tunnel-server
// repository until the change that added this guard.
//
// Ported from the equivalent guard in layervai/qurl-connector, with two
// deliberate reductions. Both exist because a guard that has to be muted is
// not a guard:
//
//   - No hostname scan. There, a CLI names two documented endpoints and
//     anything else is suspicious. Here, layerv.ai hostnames are the product
//     surface: the browser extension ships them to users.
//
//   - A shorter banned-name list. "qurl-service" appears in over 100 files
//     as this repository's ordinary architectural vocabulary, and it is
//     internal rather than private, so its bare name stays allowed; it is
//     still caught in the `layervai/<repo>` form below.
//
// The private infrastructure repository used to get the same pass, on the
// theory that banning it was "a migration, not a check". That exemption is
// how 45 files -- apps/discord/src/constants.js among them -- came to name it
// in comments, issue references (name#123) and path references
// (name/qurl-bot-discord/terraform/main.tf) with the guard green. None of
// those forms is the `layervai/<repo>` form, so nothing looked at them. The
// migration has now been done and the bare name is banned like any other
// private repository; describe it by role ("the infra repo").
//
// Forbidden literals are split so this file does not itself contain the terms
// it bans.
var (
	sanitizeAppID = regexp.MustCompile(`(?i)app[_ -]?(?:id|client[_ -]?id)[[:space:]]*[:=][[:space:]]*["']?([0-9]+)`)
	// Exclude only a preceding "@", so an @layervai/<team> CODEOWNERS entry is
	// not read as a repository reference -- teams are not repositories and are
	// not disclosures.
	//
	// A preceding "/" must NOT be excluded: github.com/layervai/<repo> is the
	// URL form, and that is precisely how a private repository gets named in a
	// doc or a workflow. An earlier draft excluded it and let the URL form
	// through entirely.
	sanitizeLayerVRepo = regexp.MustCompile(`(?i)(?:^|[^@\w])layervai/([a-z0-9][a-z0-9_-]*)`)
	// Bare names that carry no legitimate use in this repository. Each is
	// verified to appear zero times outside this file and the functional
	// references in sanitizeKnownReference. Matched as a case-insensitive
	// substring, so "qurl-<name>", "<name>#123" and "<name>/path" all hit.
	sanitizePrivateRepo = []string{
		"qurl-" + "reverse-tunnel-server",
		"traefik-" + "plugins",
		"integrations-" + "infra",
	}
	// Private repository names that are too short, or too much like ordinary
	// words, for a substring match. "nhp" in particular is also the name of
	// the protocol, which this repository legitimately discusses everywhere;
	// what is banned is the name used as a repository -- an issue or PR
	// reference. "qrts" has no use here other than naming the repository.
	// A preceding "/" is excluded from the nhp form because layervai/nhp#N is
	// the owner-qualified form, which sanitizeLayerVRepo already judges
	// (including its sanitizeKnownReference ratchet).
	sanitizePrivateRepoRef = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b` + `qrts\b`),
		regexp.MustCompile(`(?i)(?:^|[^/\w])nhp` + ` ?(?:pr ?)?#\d+`),
	}
	// Match a CREDENTIAL-BEARING webhook, not the bare host. apps/slack and
	// apps/discord legitimately name these hosts -- posting to them is what
	// they do. What must never appear is a full URL with the secret path.
	sanitizeSecretEndpoint = []*regexp.Regexp{
		regexp.MustCompile(`(?i)hooks` + `\.slack\.com/services/[A-Za-z0-9_-]{6,}`),
		regexp.MustCompile(`(?i)discord\.com/api/` + `webhooks/\d{6,}/[A-Za-z0-9_-]{6,}`),
		regexp.MustCompile(`(?i)[a-z0-9]{6,}\.execute-api` + `\.[a-z0-9-]+\.amazonaws\.com`),
	}
)

// sanitizePublicLayerVRepos are the LayerV repositories this repository may
// name in `layervai/<repo>` form. Every entry was checked against the GitHub
// API when this guard was written.
//
// Adding an entry is a deliberate disclosure decision: confirm the repository
// is actually public first, because naming a private one here silently
// disarms the check rather than failing it.
var sanitizePublicLayerVRepos = map[string]bool{
	"frp":                    true,
	"homebrew-tap":           true,
	"ops-routines-workflows": true,
	"qurl-conformance":       true,
	"qurl-connector":         true,
	"qurl-go":                true,
	"qurl-integrations":      true,
	"qurl-mcp":               true,
	// qurl-service has "internal" visibility rather than public, but it is
	// this repository's ordinary architectural vocabulary: it appears in 114
	// files, including throughout apps/slack source, and did so long before
	// this guard. Listing it is an accurate description of how the project
	// already treats the name, not a new disclosure. If that judgement ever
	// changes, removing this line turns the backlog into a work item rather
	// than a surprise.
	"qurl-service":    true,
	"qurl-python":     true,
	"qurl-typescript": true,
	// GoReleaser and Homebrew spell the tap "layervai/tap"; Homebrew expands
	// that to the public layervai/homebrew-tap.
	"tap": true,
	// ghcr.io/layervai/qurl_connector is the container namespace for the
	// public connector image, not a repository. Underscores are captured
	// (above) so it matches whole instead of truncating to "layervai/qurl".
	"qurl_connector": true,
}

// sanitizeReviewedArtifactNamespace permits the public customer image name
// without treating the same owner/name text as a reviewed GitHub repository.
// sanitizeLayerVRepo includes the slash before "layervai" in matchStart.
func sanitizeReviewedArtifactNamespace(text string, matchStart int, repo string) bool {
	if repo != "qurl" {
		return false
	}
	for _, registry := range []string{"ghcr.io", `ghcr\.io`} {
		if matchStart >= len(registry) && strings.EqualFold(text[matchStart-len(registry):matchStart], registry) {
			return true
		}
	}
	return false
}

func TestReviewedArtifactNamespaceDoesNotPermitRepositoryReferences(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		text string
		want bool
	}{
		{name: "public image", text: "ghcr.io/layervai/qurl@sha256:abc", want: true},
		{name: "public image validation regex", text: `^ghcr\.io/layervai/qurl@sha256:`, want: true},
		{name: "repository URL", text: "https://github.com/layervai/qurl", want: false},
		{name: "bare repository", text: "layervai/qurl", want: false},
		{name: "different image", text: "ghcr.io/layervai/private-runtime:latest", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			match := sanitizeLayerVRepo.FindStringSubmatchIndex(test.text)
			if match == nil {
				t.Fatalf("test input did not match repository scanner: %q", test.text)
			}
			repo := strings.ToLower(test.text[match[2]:match[3]])
			if got := sanitizeReviewedArtifactNamespace(test.text, match[0], repo); got != test.want {
				t.Fatalf("reviewed artifact namespace = %t, want %t", got, test.want)
			}
		})
	}
}

// sanitizeKnownReference freezes the private-repository references that
// already existed when this guard was added. They are FUNCTIONAL rather than
// prose leaks: a workflow that dispatches to, or gates on, a private
// repository has to name it.
//
// This is a ratchet, not an absolution. The backlog is listed here so it is
// reviewable, and every entry is keyed by (file, term) so a DIFFERENT private
// repository appearing in an already-listed file still fails.
//
// Do not add to this map to make a new failure go away. A new reference is
// either functional -- in which case it deserves the same explicit decision
// these entries got -- or it is a leak, which is the case this guard exists
// for.
var sanitizeKnownReference = map[string]bool{
	".github/workflows/cli-connector-resource-proof.yml|layervai/nhp":      true,
	".github/workflows/validate-issue-templates.yml|layervai/nhp":          true,
	".github/workflows/validate-issue-templates.yml|layervai/ops-routines": true,
	// Contract test asserting the shape of that workflow's run URL.
	"internal/ciworkflows/connector_resource_proof_test.go|layervai/nhp": true,
}

// sanitizeKnownLine is the line-exact form of sanitizeKnownReference, for a
// file where one line functionally has to name a private repository but the
// rest of the file must not: exempting the whole file would let prose next to
// the functional line leak the same name unnoticed. Only lines whose trimmed
// text equals the value are removed before the bare-name scan.
var sanitizeKnownLine = map[string]string{
	// Deploy dispatch: `target_repo:` must name the repository it dispatches
	// to. Comments in these files describe it by role.
	".github/workflows/discord.yml": "target_repo: qurl-" + "integrations-" + "infra",
	".github/workflows/slack.yml":   "target_repo: qurl-" + "integrations-" + "infra",
}

// sanitizeStripKnownLine removes rel's reviewed functional line, if it has one.
func sanitizeStripKnownLine(rel, text string) string {
	known, ok := sanitizeKnownLine[rel]
	if !ok {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != known {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestPublicSourceNamesNoPrivateLayerVMaterial(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	self := filepath.Join("internal", "ciworkflows", "public_source_sanitization_test.go")

	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch filepath.Base(rel) {
			// .worktrees holds other sessions' checkouts of this repository;
			// scanning them reports their files under the wrong path.
			case ".git", ".worktrees", "node_modules", "dist", "build", "bin", ".next", "coverage":
				return filepath.SkipDir
			}
			return nil
		}
		// This file necessarily contains the split literals it bans.
		if rel == self {
			return nil
		}
		switch {
		case strings.HasSuffix(rel, "package-lock.json"),
			strings.HasSuffix(rel, ".min.js"),
			strings.HasSuffix(rel, ".map"):
			return nil
		}
		// G304 flags reading a variable path. The variable is supplied by
		// WalkDir over this repository's own checkout, which is the entire
		// point of the test; there is no caller-controlled input to constrain.
		body, err := os.ReadFile(path) //nolint:gosec // G304: scanning this repo's own tree is the test
		if err != nil {
			return nil //nolint:nilerr // non-source entries are not the subject of this check
		}
		for _, finding := range sanitizeFindings(rel, string(body)) {
			t.Error(finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sanitizeFindings returns every violation in one file's text. rel is the
// file's slash-separated path relative to the repository root, which keys the
// sanitizeKnownReference ratchet.
func sanitizeFindings(rel, text string) []string {
	var findings []string
	lower := strings.ToLower(sanitizeStripKnownLine(rel, text))

	for _, name := range sanitizePrivateRepo {
		if !strings.Contains(lower, name) || sanitizeKnownReference[rel+"|"+name] {
			continue
		}
		findings = append(findings, fmt.Sprintf("%s names private LayerV repository %q; describe it by role instead (for example \"the infra repo\")", rel, name))
	}
	for _, ref := range sanitizePrivateRepoRef {
		if hit := ref.FindString(text); hit != "" {
			findings = append(findings, fmt.Sprintf("%s refers to a private LayerV repository as %q; describe it by role instead", rel, hit))
		}
	}
	for _, match := range sanitizeLayerVRepo.FindAllStringSubmatchIndex(text, -1) {
		repo := strings.ToLower(text[match[2]:match[3]])
		if sanitizeReviewedArtifactNamespace(text, match[0], repo) {
			continue
		}
		if !sanitizePublicLayerVRepos[repo] && !sanitizeKnownReference[rel+"|layervai/"+repo] {
			findings = append(findings, fmt.Sprintf("%s refers to LayerV repository %q, which is not on the reviewed-public list in this test", rel, "layervai/"+repo))
		}
	}
	for _, endpoint := range sanitizeSecretEndpoint {
		if hit := endpoint.FindString(text); hit != "" {
			findings = append(findings, fmt.Sprintf("%s contains what looks like a credential-bearing endpoint %q", rel, hit))
		}
	}
	if match := sanitizeAppID.FindStringSubmatch(text); match != nil {
		findings = append(findings, fmt.Sprintf("%s contains a literal GitHub App identifier %s", rel, match[1]))
	}
	return findings
}

// The guard once passed while apps/discord/src/constants.js named the private
// infrastructure repository in all three of the shapes below, because only
// the `layervai/<repo>` form was checked. Each "flag" case is one of those
// real comment lines (with the name spliced back together at run time).
func TestSanitizeFindingsCatchesBarePrivateRepoNames(t *testing.T) {
	t.Parallel()
	infra := "qurl-integrations-" + "infra"
	const file = "apps/discord/src/constants.js"
	for _, test := range []struct {
		name string
		rel  string
		text string
		flag bool
	}{
		{name: "path reference", rel: file, text: "// filters at " + infra + "/qurl-bot-discord/terraform/main.tf", flag: true},
		{name: "issue reference", rel: file, text: "// Justin's review comment on " + infra + "#309.", flag: true},
		{name: "possessive prose", rel: file, text: "// TODO(upstream-contract): keep " + infra + "'s", flag: true},
		{name: "upper case", rel: file, text: "// see " + strings.ToUpper(infra), flag: true},
		{name: "tunnel server acronym", rel: "apps/cli/cmd/x_test.go", text: "// q" + "RTS currently uses FRP's 90-second stale", flag: true},
		{name: "nhp issue reference", rel: "apps/slack/internal/x.go", text: "// the GSI key handler from nh" + "p #1825", flag: true},
		{name: "nhp PR reference", rel: "apps/slack/internal/x.go", text: "// landed in NH" + "P PR #12", flag: true},
		{name: "ratchet does not cover other files", rel: ".github/workflows/cli.yml", text: "target_repo: " + infra, flag: true},
		{name: "owner-qualified nhp reference is left to the ratchet", rel: ".github/workflows/validate-issue-templates.yml", text: "# hit this in layervai/nh" + "p#1307", flag: false},
		{name: "role description", rel: file, text: "// filters at the infra repo's qurl-bot-discord/terraform/main.tf (infra repo #309)", flag: false},
		{name: "protocol name", rel: file, text: "// the NHP knock precedes every connection; OpenNHP is the protocol", flag: false},
		{name: "internal service name", rel: file, text: "// the landing URL qurl-service returns from POST /v1/qurls", flag: false},
		{name: "functional dispatch target", rel: ".github/workflows/discord.yml", text: "    with:\n      target_repo: " + infra + "\n", flag: false},
		{name: "prose beside the dispatch target", rel: ".github/workflows/discord.yml", text: "      target_repo: " + infra + "\n      # receiver lives in " + infra + "\n", flag: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			findings := sanitizeFindings(test.rel, test.text)
			if got := len(findings) > 0; got != test.flag {
				t.Fatalf("flagged = %t, want %t (findings: %q)", got, test.flag, findings)
			}
		})
	}
}
