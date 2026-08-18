package ciworkflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The set of contexts `main` requires is written down in four places, and
// nothing used to tie them together:
//
//  1. CONTRIBUTING.md's "Merge-result checks" section and README.md's CI section
//  2. the job `name:` fields in .github/workflows that produce the contexts
//  3. requiredWorkflowSpecs in workflow_test.go
//  4. the live branch-protection setting
//
// Sites 1-3 are checked against each other below, all of it offline. Site 4
// needs administration:read, which no CI token here can hold, so it is an
// opt-in local diff — see TestLiveBranchProtectionMatchesDocumentedContexts.
//
// The failure this defends against is silent by construction: a context that
// matches no job never reports, and an unreported required check reads as
// "Expected — Waiting for status to be reported" rather than as a failure. On
// 2026-08-14 that let a required list of `Workflow contract` (lowercase c,
// matching no job) stand for three days while every merge used an admin
// override. Every assertion below therefore prints the actual mismatch.
const (
	contributingPath = "CONTRIBUTING.md"
	readmePath       = "README.md"

	requiredContextsBegin = "<!-- BEGIN required-contexts -->"
	requiredContextsEnd   = "<!-- END required-contexts -->"

	// Reusable-workflow calls report as "<caller job> / <inner job>".
	contextSeparator = " / "
	aggregateSuffix  = contextSeparator + "required"

	liveProtectionEnv = "QURL_LIVE_BRANCH_PROTECTION"
	// Pinned to the canonical repo rather than derived from the local remote:
	// the docs in this checkout describe upstream's protection, so a fork
	// should still diff against upstream. A fork with its own protection is
	// not what this test is for.
	protectionRepo    = "layervai/qurl-integrations"
	protectionAPIPath = "repos/" + protectionRepo + "/branches/main/protection/required_status_checks"
)

// TestDocumentedRequiredContextsResolveToWorkflowJobs is the assertion that
// would have caught the 2026-08-14 typo had it been written into the docs
// alongside the settings change, which is the process CONTRIBUTING.md asks for.
// It applies the same spelling rule that section documents: a job's `name:`,
// its job id where it sets none, and "<caller-job> / <inner-job>" for a
// reusable-workflow call.
func TestDocumentedRequiredContextsResolveToWorkflowJobs(t *testing.T) {
	t.Parallel()

	reported := workflowReportedContexts(t)

	upstream := []string{}
	for _, wanted := range documentedRequiredContexts(t) {
		if files, ok := reported.direct[wanted]; ok {
			// Two jobs reporting the same context makes it ambiguous which one
			// a green check came from, and lets either be renamed without this
			// test noticing.
			if len(files) > 1 {
				sort.Strings(files)
				t.Errorf("required context %q is reported by more than one job: %s", wanted, strings.Join(files, ", "))
			}
			continue
		}

		caller, inner, isPrefixed := strings.Cut(wanted, contextSeparator)
		if isPrefixed {
			if _, ok := reported.reusable[caller]; ok {
				upstream = append(upstream, inner)
				continue
			}
		}

		t.Errorf("required context %q is documented in %s but no job in .github/workflows reports it%s",
			wanted, contributingPath, unresolvedContextHint(wanted, reported))
	}

	// Named rather than silently accepted: the inner half of these lives in the
	// workflow the caller `uses:`, in another repository, so an upstream rename
	// is invisible here and surfaces only as a required check that stops
	// reporting. TestReusableCallerJobsCoverTheirDocumentedContexts covers the
	// half that is local.
	if len(upstream) > 0 {
		sort.Strings(upstream)
		t.Logf("inner jobs defined upstream and unverified by this checkout: %s", strings.Join(upstream, ", "))
	}
}

// TestReusableCallerJobsCoverTheirDocumentedContexts closes the hole that a
// shared caller name opens. All four age-check workflows key their calling job
// `age-check`, so any one of them satisfies a lookup for that prefix and three
// of the four could be renamed with every context still resolving. Each
// "<caller> / <inner>" context needs its own calling job, so the counts must
// match. A reusable workflow that ever contributes two required inner jobs from
// one caller breaks that assumption — and should fail here rather than quietly
// widen it.
func TestReusableCallerJobsCoverTheirDocumentedContexts(t *testing.T) {
	t.Parallel()

	reported := workflowReportedContexts(t)

	wanted := map[string]int{}
	for _, name := range documentedRequiredContexts(t) {
		caller, _, isPrefixed := strings.Cut(name, contextSeparator)
		if !isPrefixed {
			continue
		}
		if _, ok := reported.reusable[caller]; ok {
			wanted[caller]++
		}
	}
	if len(wanted) == 0 {
		t.Fatalf("no documented context resolves to a reusable-workflow caller; the scan matched nothing and this assertion is vacuous")
	}

	for caller, count := range wanted {
		files := reported.reusable[caller]
		if len(files) == count {
			continue
		}
		sort.Strings(files)
		t.Errorf("%d documented contexts are prefixed %q, but %d job(s) named or keyed %q call a reusable workflow (%s) — one of those contexts has no job to report it",
			count, caller+contextSeparator, len(files), caller, strings.Join(files, ", "))
	}
}

// TestDocumentedAggregatesMatchRequiredWorkflowSpecs ties the documented list
// to the spec table. TestRequiredWorkflowSpecsCoverEveryAggregate already
// forces a workflow's `required` job to have a spec; this closes the other
// half, so an aggregate cannot become required-in-practice while the docs
// still describe the old set.
func TestDocumentedAggregatesMatchRequiredWorkflowSpecs(t *testing.T) {
	t.Parallel()

	registered := make([]string, 0, len(requiredWorkflowSpecs))
	for i := range requiredWorkflowSpecs {
		registered = append(registered, requiredWorkflowSpecs[i].requiredName)
	}

	assertSameContexts(t,
		contributingPath+" required-contexts block", aggregateContexts(documentedRequiredContexts(t)),
		"requiredWorkflowSpecs", registered)
}

// TestReadmeAggregatesMatchDocumentedAggregates enforces README.md's own
// instruction to keep its list in step with CONTRIBUTING.md. It is two-way:
// README's list exists only to mirror the aggregates, so an extra name there
// is as wrong as a missing one.
func TestReadmeAggregatesMatchDocumentedAggregates(t *testing.T) {
	t.Parallel()

	assertSameContexts(t,
		contributingPath+" required-contexts block", aggregateContexts(documentedRequiredContexts(t)),
		readmePath, backtickedAggregates(readRepoFile(t, readmePath)))
}

// TestContributingProseAggregatesAreDocumented keeps the surrounding prose from
// naming an aggregate the block does not list. It is deliberately one-way:
// that section also narrates the 2026-08-14 incident, and a historical
// sentence legitimately names contexts without re-listing the current set.
func TestContributingProseAggregatesAreDocumented(t *testing.T) {
	t.Parallel()

	documented := stringSet(aggregateContexts(documentedRequiredContexts(t)))

	prose := backtickedAggregates(readRepoFile(t, contributingPath))
	if len(prose) == 0 {
		t.Fatalf("no backticked `<app> / required` names found in %s; the scan matched nothing and this assertion is vacuous", contributingPath)
	}

	for _, name := range prose {
		if !documented[name] {
			t.Errorf("%s prose names aggregate %q, which the required-contexts block does not list — either the block is missing it or the prose is stale", contributingPath, name)
		}
	}
}

// TestDocumentedRequiredContextsIncludeWorkflowContractCheck pins the one
// context this package produces itself. Renaming the contract job without
// touching the docs and the live setting would silence the check that polices
// every other workflow.
func TestDocumentedRequiredContextsIncludeWorkflowContractCheck(t *testing.T) {
	t.Parallel()

	if documented := stringSet(documentedRequiredContexts(t)); !documented[workflowContractCheckName] {
		t.Errorf("%s required-contexts block omits %q, the check that runs this package", contributingPath, workflowContractCheckName)
	}
}

// TestContributingCountsMatchTheDocumentedSet checks the two spelled-out counts
// in the prose. A truncated list reads as plausible; a list of fifteen
// described as "fifteen in all" that now holds fourteen does not.
func TestContributingCountsMatchTheDocumentedSet(t *testing.T) {
	t.Parallel()

	documented := documentedRequiredContexts(t)
	body := readRepoFile(t, contributingPath)

	tests := []struct {
		name    string
		pattern *regexp.Regexp
		want    int
	}{
		// Each pattern is pinned to a specific sentence in the Merge-result
		// checks section, so that prose is load-bearing: reword it and the
		// exactly-one-match assertion below fails loudly rather than passing
		// on no match. Update the pattern in the same edit.
		{name: "aggregate count", pattern: regexp.MustCompile(`branch protection requires all (\w+):`), want: len(aggregateContexts(documented))},
		{name: "full set count", pattern: regexp.MustCompile(`contexts, (\w+) in all`), want: len(documented)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Exactly one match, not the first of several: silently reading the
			// wrong occurrence is the same class of quiet miss this file exists
			// to remove.
			matches := test.pattern.FindAllStringSubmatch(body, -1)
			if len(matches) != 1 {
				t.Fatalf("%s contains %d phrases matching %s, want exactly 1 — reword the check alongside the prose",
					contributingPath, len(matches), test.pattern)
			}
			got, ok := numberWords[matches[0][1]]
			if !ok {
				t.Fatalf("%s says %q, which is not a number word this test knows", contributingPath, matches[0][1])
			}
			if got != test.want {
				t.Errorf("%s says %q (%d), but the required-contexts block holds %d entries", contributingPath, matches[0][1], got, test.want)
			}
		})
	}
}

// TestLiveBranchProtectionMatchesDocumentedContexts is the only assertion here
// that can see a settings edit made outside a PR — the actual shape of the
// 2026-08-14 incident. It cannot run in CI: reading branch protection needs
// administration:read, `GITHUB_TOKEN` has no such permission key, no repo or
// org secret carries one, the endpoint rejects unauthenticated reads, and this
// repo uses classic protection rather than rulesets, so the low-permission
// rules endpoints return nothing. Run it by hand after any protection change,
// with a gh login that has admin on the repo.
func TestLiveBranchProtectionMatchesDocumentedContexts(t *testing.T) {
	if os.Getenv(liveProtectionEnv) == "" {
		t.Skipf("set %s=1, with a gh login that has admin on %s, to diff live branch protection against %s",
			liveProtectionEnv, protectionRepo, contributingPath)
	}
	requireCommand(t, "gh")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", protectionAPIPath)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	body, err := cmd.Output()
	if err != nil {
		t.Fatalf("gh api %s: %v\n%s\nA 404 or 403 here usually means the login lacks admin on %s rather than that protection is off.",
			protectionAPIPath, err, stderr.String(), protectionRepo)
	}

	var live struct {
		Strict bool `json:"strict"`
		// contexts is deprecated in favor of checks, and both are populated
		// today. Read checks first so this keeps working when the old field
		// goes away: an empty contexts would otherwise read as "every
		// documented context is missing" — a spurious full mismatch from the
		// one assertion whose whole job is to be believed.
		Checks []struct {
			Context string `json:"context"`
		} `json:"checks"`
		Contexts []string `json:"contexts"`
	}
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatalf("parse %s response: %v", protectionAPIPath, err)
	}

	source, required := "checks[].context", make([]string, 0, len(live.Checks))
	for _, check := range live.Checks {
		required = append(required, check.Context)
	}
	if len(required) == 0 {
		// Not a fatal: protection really having no required checks is itself
		// drift worth reporting, and it surfaces below as every documented
		// context missing.
		source, required = "contexts", live.Contexts
	}
	t.Logf("read %d live context(s) from %s", len(required), source)

	// Strict is what makes a green check mean "green against current main".
	// Without it the documented set can be complete and still gate nothing
	// meaningful, so it belongs in the same diff.
	if !live.Strict {
		t.Errorf("live branch protection has strict = false, but %s documents strict status checks", contributingPath)
	}

	assertSameContexts(t,
		contributingPath+" required-contexts block", documentedRequiredContexts(t),
		"live "+protectionRepo+" main protection", required)
}

// numberWords covers the counts this repo's docs plausibly spell out. A count
// beyond it fails loudly in TestContributingCountsMatchTheDocumentedSet rather
// than being read as zero.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20,
}

var backtickedAggregatePattern = regexp.MustCompile("`([^`]+" + regexp.QuoteMeta(aggregateSuffix) + ")`")

// workflowContexts is what .github/workflows would report, split by how a
// context resolves: direct jobs report under one name, reusable-workflow calls
// contribute only the prefix of a two-part context.
type workflowContexts struct {
	direct   map[string][]string // context name -> workflow files reporting it
	reusable map[string][]string // caller job display name -> workflow files
}

func workflowReportedContexts(t *testing.T) workflowContexts {
	t.Helper()

	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	found := workflowContexts{direct: map[string][]string{}, reusable: map[string][]string{}}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		for id, job := range readWorkflow(t, name).Jobs {
			// A check renders under the job's `name:` when it sets one and
			// under its job id otherwise.
			display := job.Name
			if display == "" {
				display = id
			}
			if strings.TrimSpace(job.Uses) != "" {
				found.reusable[display] = append(found.reusable[display], name)
				continue
			}
			found.direct[display] = append(found.direct[display], name)
		}
	}

	// Guard against a scan that matched nothing — a renamed directory or
	// changed extension would otherwise make every assertion above pass by
	// finding no contexts to contradict.
	if len(found.direct) == 0 {
		t.Fatal("no direct jobs found in .github/workflows; the scan matched nothing and every resolution check would be vacuous")
	}
	return found
}

// documentedRequiredContexts reads the block CONTRIBUTING.md marks as the
// source of truth. It parses a delimited block rather than the prose because
// that section deliberately quotes the bad 2026-08-14 spelling, and a scan of
// every backticked name there would read the typo as a requirement.
func documentedRequiredContexts(t *testing.T) []string {
	t.Helper()

	_, afterBegin, ok := strings.Cut(readRepoFile(t, contributingPath), requiredContextsBegin)
	if !ok {
		t.Fatalf("%s is missing the %s marker that pins the required-context list", contributingPath, requiredContextsBegin)
	}
	block, _, ok := strings.Cut(afterBegin, requiredContextsEnd)
	if !ok {
		t.Fatalf("%s has %s with no matching %s", contributingPath, requiredContextsBegin, requiredContextsEnd)
	}

	contexts := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if seen[line] {
			t.Errorf("%s lists required context %q twice", contributingPath, line)
			continue
		}
		seen[line] = true
		contexts = append(contexts, line)
	}
	if len(contexts) == 0 {
		t.Fatalf("%s required-contexts block is empty", contributingPath)
	}
	return contexts
}

func aggregateContexts(contexts []string) []string {
	aggregates := []string{}
	for _, name := range contexts {
		if strings.HasSuffix(name, aggregateSuffix) {
			aggregates = append(aggregates, name)
		}
	}
	return aggregates
}

func backtickedAggregates(body string) []string {
	names := []string{}
	seen := map[string]bool{}
	for _, match := range backtickedAggregatePattern.FindAllStringSubmatch(body, -1) {
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	return names
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()

	// #nosec G304 -- name is one of the checked-in doc paths declared above.
	body, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// assertSameContexts reports the difference between two context lists as the
// lists themselves, not as a count or a bool. Every caller here is diagnosing a
// name that silently matches nothing, so the useful output is which name is on
// which side — and, above all, whether two names differ only in case.
func assertSameContexts(t *testing.T, wantLabel string, want []string, gotLabel string, got []string) {
	t.Helper()

	wantSet, gotSet := stringSet(want), stringSet(got)
	missing, extra := []string{}, []string{}
	for _, name := range want {
		if !gotSet[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range got {
		if !wantSet[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	sort.Strings(missing)
	sort.Strings(extra)

	report := &strings.Builder{}
	fmt.Fprintf(report, "%s and %s disagree:\n", wantLabel, gotLabel)
	for _, name := range missing {
		fmt.Fprintf(report, "  - %s: in %s, absent from %s%s\n", strconv.Quote(name), wantLabel, gotLabel, caseOnlyNote(name, extra))
	}
	for _, name := range extra {
		fmt.Fprintf(report, "  + %s: in %s, absent from %s%s\n", strconv.Quote(name), gotLabel, wantLabel, caseOnlyNote(name, missing))
	}
	fmt.Fprintf(report, "\n%s (%d):\n%s\n\n%s (%d):\n%s",
		wantLabel, len(want), indentQuoted(want), gotLabel, len(got), indentQuoted(got))
	t.Error(report.String())
}

// caseOnlyNote is the whole point of the exercise: GitHub matches required
// contexts case-sensitively, so a pair differing only in case looks identical
// in a review and can never be satisfied.
func caseOnlyNote(name string, others []string) string {
	for _, other := range others {
		if other != name && strings.EqualFold(other, name) {
			return " — differs from " + strconv.Quote(other) + " only in case, and required contexts match case-sensitively"
		}
	}
	return ""
}

func unresolvedContextHint(wanted string, reported workflowContexts) string {
	if match, ok := caseInsensitiveKey(reported.direct, wanted); ok {
		return "; job " + strconv.Quote(match) + " differs only in case, and required contexts match case-sensitively, so this one can never be satisfied"
	}

	// "<a> / <b>" is how an aggregate job names itself and also how a reusable
	// call renders, so the string alone does not say which shape was intended.
	// Suggest the reusable fix only on evidence: assuming it sends the reader
	// after a missing caller job when an aggregate was simply renamed.
	caller, _, isPrefixed := strings.Cut(wanted, contextSeparator)
	if isPrefixed {
		if match, ok := caseInsensitiveKey(reported.reusable, caller); ok {
			return "; the reusable-workflow caller is spelled " + strconv.Quote(match) + ", not " + strconv.Quote(caller)
		}
	}

	if candidates := sameFirstWord(wanted, reported.direct); len(candidates) > 0 {
		return "; jobs with a similar name: " + strings.Join(candidates, ", ")
	}
	if isPrefixed {
		if candidates := sameFirstWord(caller, reported.reusable); len(candidates) > 0 {
			return "; reusable-workflow callers with a similar name: " + strings.Join(candidates, ", ")
		}
		return "; no job reports it directly, and no job named or keyed " + strconv.Quote(caller) +
			" calls a reusable workflow"
	}
	return fmt.Sprintf("; none of the %d job names scanned resemble it", len(reported.direct))
}

func caseInsensitiveKey(names map[string][]string, wanted string) (string, bool) {
	for name := range names {
		if strings.EqualFold(name, wanted) {
			return name, true
		}
	}
	return "", false
}

func sameFirstWord(wanted string, names map[string][]string) []string {
	prefix, _, _ := strings.Cut(wanted, " ")
	matches := []string{}
	for name := range names {
		if candidate, _, _ := strings.Cut(name, " "); strings.EqualFold(candidate, prefix) {
			matches = append(matches, strconv.Quote(name))
		}
	}
	sort.Strings(matches)
	return matches
}

func indentQuoted(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, "  "+strconv.Quote(name))
	}
	sort.Strings(quoted)
	return strings.Join(quoted, "\n")
}
