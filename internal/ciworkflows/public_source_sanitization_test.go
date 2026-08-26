package ciworkflows

import (
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
//   - A shorter banned-name list. "qurl-service" appears in 114 files and
//     "qurl-integrations-infra" in 41 -- they are this repository's ordinary
//     architectural vocabulary, and qurl-service is internal rather than
//     private. Banning them is a migration, not a check. Both are still caught
//     in the `layervai/<repo>` form below, which is the form that reads as a
//     repository reference rather than a service name.
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
	// verified to appear zero times outside this file.
	sanitizePrivateRepo = []string{
		"qurl-" + "reverse-tunnel-server",
		"traefik-" + "plugins",
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
			case ".git", "node_modules", "dist", "build", "bin", ".next", "coverage":
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
		text := string(body)
		lower := strings.ToLower(text)

		for _, name := range sanitizePrivateRepo {
			if strings.Contains(lower, name) && !sanitizeKnownReference[rel+"|"+name] {
				t.Errorf("%s names private LayerV repository %q; describe it by role instead (for example \"the producer repository\")", rel, name)
			}
		}
		for _, match := range sanitizeLayerVRepo.FindAllStringSubmatch(text, -1) {
			repo := strings.ToLower(match[1])
			if !sanitizePublicLayerVRepos[repo] && !sanitizeKnownReference[rel+"|layervai/"+repo] {
				t.Errorf("%s refers to LayerV repository %q, which is not on the reviewed-public list in this test", rel, "layervai/"+repo)
			}
		}
		for _, endpoint := range sanitizeSecretEndpoint {
			if hit := endpoint.FindString(text); hit != "" {
				t.Errorf("%s contains what looks like a credential-bearing endpoint %q", rel, hit)
			}
		}
		if match := sanitizeAppID.FindStringSubmatch(text); match != nil {
			t.Errorf("%s contains a literal GitHub App identifier %s", rel, match[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
