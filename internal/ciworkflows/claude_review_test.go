package ciworkflows

import (
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// claude-code-review.yml carries a required context and cannot validate its own
// edits: it runs on `pull_request_target`, so a pull request that changes it is
// reviewed by the default branch's copy of the file. Its own green check is
// therefore not evidence about the change, and this package is where the shape
// of that file gets pinned instead.
//
// What these tests do not cover: they read the job's guards and execute each
// step's script in isolation with hand-supplied env, so they cannot observe
// how a step's conclusion propagates to the job — that a failed review really
// does redden the job and skip the gate, or that `if: success()` really does
// run after skipped steps. That is runner behavior, and the only way to see it
// is to run the chain on a runner. It was confirmed once on a throwaway
// push-triggered probe branch (2026-08-19): the exempt paths concluded
// `success` with the four middle steps `skipped` and the gate still running,
// and a run where nothing failed but nothing published concluded `failure` from
// the gate. Treat a green suite here as "the shape and the scripts are right",
// never as "the job concludes correctly".
const (
	claudeReviewWorkflow = "claude-code-review.yml"
	claudeReviewJobID    = "claude-review"

	claudeReviewEligibilityStepName = "Resolve Claude review eligibility"
	claudeReviewCheckoutStepName    = "Checkout trusted default branch history"
	claudeReviewRunStepName         = "Run Claude Code Review"
	claudeReviewReportStepName      = "Report unfinished Claude review"
	claudeReviewPublishStepName     = "Publish and verify terminal Claude review"
	claudeReviewGateStepName        = "Require a completed review or a declared exemption"

	claudeReviewEligibilityStepID = "eligibility"
	claudeReviewPublishStepID     = "publish_review"

	claudeReviewEligibleGuard = "steps.eligibility.outputs.eligible == 'true'"

	// claudeReviewBudgetEnv names the job-level minute budget. It is the one
	// number the review step's cap, the report step's classification and the
	// report step's message all derive from, which is what makes a single
	// wrong value impossible to spell.
	claudeReviewBudgetEnv = "CLAUDE_REVIEW_BUDGET_MINUTES"
)

// TestClaudeReviewConcludesOnEveryPullRequest is the assertion this whole
// restructuring exists for.
//
// GitHub scores a *skipped* required check as satisfied. While the bot, draft
// and fork conditions were the job's own `if:`, every path that withheld the
// review also silently satisfied the context that was supposed to gate on it —
// so `claude-review` gated the timing of a review that ran, never the existence
// of one, and returning a pull request to draft turned the box green with
// nothing behind it. Dependabot #1156/#1155/#1132 and draft #1158 all reported
// `skipped` against live protection.
//
// The fix is the pattern the `<app> / required` aggregates already use, and
// which workflow_test.go's `if: always()` assertion pins for them: keep the job
// running and move the conditions inside, where a withheld review has to say so
// out loud. This test fails if they migrate back.
func TestClaudeReviewConcludesOnEveryPullRequest(t *testing.T) {
	t.Parallel()

	job := claudeReviewJob(t, readWorkflow(t, claudeReviewWorkflow))

	if got := strings.TrimSpace(job.If); got != "always()" {
		t.Errorf("%s.if = %q, want always() — a conditional job reports `skipped`, which GitHub scores as a satisfied required check", claudeReviewJobID, got)
	}
	// The conditions belong in the eligibility step. Named individually so the
	// failure says which one came back rather than that something did.
	for _, moved := range []string{"user.type", "draft", "head.repo", "base.repo"} {
		if strings.Contains(job.If, moved) {
			t.Errorf("%s.if mentions %q; that condition belongs in the %q step, because withholding the review from the job level also satisfies the required check", claudeReviewJobID, moved, claudeReviewEligibilityStepName)
		}
	}

	if len(job.Steps) == 0 {
		t.Fatalf("%s job has no steps", claudeReviewJobID)
	}

	// Nothing may precede the classification, and nothing may follow the
	// conclusion gate: `success()` and the step outcomes it reads cover only
	// the steps ahead of it.
	if first := job.Steps[0]; first.Name != claudeReviewEligibilityStepName {
		t.Errorf("first step = %q, want %q — eligibility must be decided before any other step runs", first.Name, claudeReviewEligibilityStepName)
	}
	if last := job.Steps[len(job.Steps)-1]; last.Name != claudeReviewGateStepName {
		t.Errorf("last step = %q, want %q — a step after the gate is a conclusion the gate never saw", last.Name, claudeReviewGateStepName)
	}

	eligibility := claudeReviewStep(t, &job, claudeReviewEligibilityStepName)
	if eligibility.ID != claudeReviewEligibilityStepID {
		t.Errorf("%s id = %q, want %q", claudeReviewEligibilityStepName, eligibility.ID, claudeReviewEligibilityStepID)
	}
	if strings.TrimSpace(eligibility.If) != "" {
		t.Errorf("%s if = %q, want unconditional", claudeReviewEligibilityStepName, eligibility.If)
	}

	// The checkout guard is the root of the chain: every later step already
	// keys off `steps.checkout.outcome`, so this is the single place the
	// withheld paths are cut off from the secrets-bearing work.
	checkout := claudeReviewStep(t, &job, claudeReviewCheckoutStepName)
	if strings.TrimSpace(checkout.If) != claudeReviewEligibleGuard {
		t.Errorf("%s if = %q, want %q", claudeReviewCheckoutStepName, checkout.If, claudeReviewEligibleGuard)
	}

	gate := claudeReviewStep(t, &job, claudeReviewGateStepName)
	if strings.TrimSpace(gate.If) != "success()" {
		t.Errorf("%s if = %q, want success() — the gate speaks only on the otherwise-green path; a failed review is already red and annotated, and a `concurrency` cancellation is not its to report", claudeReviewGateStepName, gate.If)
	}

	// The reporting paths exist so a missing review can never read as a passing
	// one. A restructure that drops either is the failure they guard against.
	report := claudeReviewStep(t, &job, claudeReviewReportStepName)
	publish := claudeReviewStep(t, &job, claudeReviewPublishStepName)
	for _, reporter := range []step{report, publish} {
		if reporter.ContinueOnError != nil {
			t.Errorf("%s sets continue-on-error = %v; a swallowed failure here is the silent pass this workflow reports on", reporter.Name, reporter.ContinueOnError)
		}
	}
	if publish.ID != claudeReviewPublishStepID {
		t.Errorf("%s id = %q, want %q — the gate reads this step's outcome to decide whether a review was published", claudeReviewPublishStepName, publish.ID, claudeReviewPublishStepID)
	}
}

// TestClaudeReviewEligibilityClassifiesEveryPullRequest executes the step
// rather than reading it. Its withheld branches run only on bot, fork and draft
// pull requests, so a defect in them ships green and surfaces exactly when the
// classification is load-bearing — the same argument
// TestClaudeReviewReportClassifiesEveryUnfinishedReview makes for its own
// existence.
func TestClaudeReviewEligibilityClassifiesEveryPullRequest(t *testing.T) {
	t.Parallel()
	requireCommand(t, "bash")

	const repo = "layervai/qurl-integrations"
	script := claudeReviewStepScript(t, claudeReviewEligibilityStepName)

	tests := []struct {
		name   string
		author string
		draft  string
		// wantExemption "" means the pull request is reviewable; eligible is
		// derived from it below rather than restated per row, since only one
		// value is ever legal for a given exemption.
		headRepo      string
		baseRepo      string
		wantExemption string
	}{
		{name: "human ready same-repo pull request is reviewed", author: "User", draft: "false", headRepo: repo, baseRepo: repo},
		{name: "organization author is reviewed", author: "Organization", draft: "false", headRepo: repo, baseRepo: repo},
		{name: "bot author is exempt", author: "Bot", draft: "false", headRepo: repo, baseRepo: repo, wantExemption: "bot"},
		{name: "draft is exempt", author: "User", draft: "true", headRepo: repo, baseRepo: repo, wantExemption: "draft"},
		{name: "fork head is exempt", author: "User", draft: "false", headRepo: "outsider/qurl-integrations", baseRepo: repo, wantExemption: "fork"},
		{name: "foreign base is exempt", author: "User", draft: "false", headRepo: repo, baseRepo: "outsider/qurl-integrations", wantExemption: "fork"},
		// Precedence: a bot can never receive this review, while a draft only
		// waits for one, so the most durable reason is the one reported.
		{name: "bot outranks fork and draft", author: "Bot", draft: "true", headRepo: "outsider/qurl-integrations", baseRepo: repo, wantExemption: "bot"},
		{name: "fork outranks draft", author: "User", draft: "true", headRepo: "outsider/qurl-integrations", baseRepo: repo, wantExemption: "fork"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runClaudeReviewStep(t, script, map[string]string{
				"AUTHOR_TYPE": tc.author,
				"IS_DRAFT":    tc.draft,
				"HEAD_REPO":   tc.headRepo,
				"BASE_REPO":   tc.baseRepo,
				"REPO":        repo,
			})
			if run.err != nil {
				t.Fatalf("eligibility step failed: %v\noutput:\n%s", run.err, run.combined)
			}
			wantEligible := "true"
			if tc.wantExemption != "" {
				wantEligible = "false"
			}
			if got := run.outputs["eligible"]; got != wantEligible {
				t.Errorf("eligible = %q, want %q\noutput:\n%s", got, wantEligible, run.combined)
			}
			if got := run.outputs["exemption"]; got != tc.wantExemption {
				t.Errorf("exemption = %q, want %q\noutput:\n%s", got, tc.wantExemption, run.combined)
			}
			// An exemption is a pass, so it has to arrive with something a
			// human can read on the check. The gate refuses a bare reason.
			if tc.wantExemption != "" && strings.TrimSpace(run.outputs["exemption_detail"]) == "" {
				t.Errorf("exemption %q carries no exemption_detail; the conclusion gate rejects a reason with no explanation", tc.wantExemption)
			}
			if tc.wantExemption == "" && run.outputs["exemption_detail"] != "" {
				t.Errorf("reviewed pull request emitted exemption_detail = %q", run.outputs["exemption_detail"])
			}
		})
	}
}

// TestClaudeReviewEligibilityFailsClosedOnAnUnreadablePullRequest covers the
// half that must never become an exemption. An exemption is a pass, so a
// payload this step cannot classify has to redden the check rather than pick a
// branch.
func TestClaudeReviewEligibilityFailsClosedOnAnUnreadablePullRequest(t *testing.T) {
	t.Parallel()
	requireCommand(t, "bash")

	const repo = "layervai/qurl-integrations"
	script := claudeReviewStepScript(t, claudeReviewEligibilityStepName)

	base := map[string]string{
		"AUTHOR_TYPE": "User",
		"IS_DRAFT":    "false",
		"HEAD_REPO":   repo,
		"BASE_REPO":   repo,
		"REPO":        repo,
	}

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "missing author type", key: "AUTHOR_TYPE", value: ""},
		{name: "missing draft state", key: "IS_DRAFT", value: ""},
		// Not a boolean the step recognizes. Treating an unrecognized draft
		// state as "not a draft" would review a candidate the author has not
		// declared ready; treating it as a draft would pass the check empty.
		{name: "unrecognized draft state", key: "IS_DRAFT", value: "TRUE"},
		{name: "missing head repository", key: "HEAD_REPO", value: ""},
		{name: "missing base repository", key: "BASE_REPO", value: ""},
		{name: "missing workflow repository", key: "REPO", value: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := maps.Clone(base)
			env[tc.key] = tc.value

			run := runClaudeReviewStep(t, script, env)
			if run.err == nil {
				t.Fatalf("eligibility step succeeded on %s, want failure\noutput:\n%s", tc.name, run.combined)
			}
			if !strings.Contains(run.combined, "::error title=Claude review eligibility is undecidable::") {
				t.Errorf("output = %q, want the undecidable-eligibility annotation", run.combined)
			}
			if run.outputs["eligible"] != "" {
				t.Errorf("eligible = %q on an undecidable payload, want no verdict at all", run.outputs["eligible"])
			}
		})
	}
}

// TestClaudeReviewReportClassifiesEveryUnfinishedReview executes the report
// step rather than reading it.
//
// That step runs only after the review has already failed, so a defect in it
// ships green and is discovered exactly when it is needed. It is also the only
// thing that tells a maintainer a review is missing, so the assertion with
// teeth is that *every* branch annotates: nothing here may regress to a bare
// echo, which would restore the silent pass this job exists to remove.
//
// The real run: block is extracted and executed rather than pattern-matched,
// so rewording the messages does not require editing this test. Elapsed time
// decides the explanation, never whether the missing review is reported — a
// misclassification costs a wrong cause, not a silent pass — which is why the
// exit code and the annotation count are asserted on every row and the title
// only names which cause was chosen.
func TestClaudeReviewReportClassifiesEveryUnfinishedReview(t *testing.T) {
	t.Parallel()
	requireCommand(t, "bash")

	script := claudeReviewStepScript(t, claudeReviewReportStepName)
	// Read from the workflow rather than restated, so raising the budget moves
	// these rows with it instead of leaving them asserting against a value the
	// workflow no longer uses.
	budgetMinutes := readClaudeReviewBudgetWiring(t).budgetMinutes
	budgetSeconds := budgetMinutes * 60

	// The step reads `date +%s`, so the wall clock decides which branch it
	// takes. Sampling the real clock made the boundary rows racy: a single tick
	// between reading `now` and running a row moves elapsed past the budget and
	// flips the expected classification. `date` is stubbed instead, so `now` is
	// fixed and every boundary below is exact.
	const now = 1000000000
	stubbedPath := stubbedDatePATH(t)

	const (
		timedOut     = "Claude review ran out of time"
		failed       = "Claude review failed"
		unmeasurable = "Claude review did not finish"
	)

	tests := []struct {
		name      string
		startedAt string
		// budget defaults to the workflow's own value; only the row that
		// corrupts it sets this.
		budget    string
		wantTitle string
	}{
		// Overrun: the runner kills the step at the budget, so elapsed is
		// always at or just past it by the time this step measures.
		{name: "over budget is reported as a timeout", startedAt: strconv.Itoa(now - budgetSeconds - 20), wantTitle: timedOut},
		{name: "exactly at budget is reported as a timeout", startedAt: strconv.Itoa(now - budgetSeconds), wantTitle: timedOut},

		// Inside the budget the step cannot have been killed, so this was a
		// real failure and must not be blamed on the clock.
		{name: "one second under budget is reported as a failure", startedAt: strconv.Itoa(now - budgetSeconds + 1), wantTitle: failed},
		{name: "an early failure is reported as a failure", startedAt: strconv.Itoa(now - 5), wantTitle: failed},

		// Unmeasurable: a timeout cannot be ruled out, so these must still
		// annotate rather than pass quietly. Both inputs reach an arithmetic
		// context in the step, so the last two also pin that an expression is
		// rejected as input rather than evaluated as one.
		{name: "an empty clock is reported as unfinished", startedAt: "", wantTitle: unmeasurable},
		{name: "a non-numeric clock is reported as unfinished", startedAt: "not-a-number", wantTitle: unmeasurable},
		{name: "an injected clock expression is reported as unfinished", startedAt: "1+1", wantTitle: unmeasurable},
		{name: "a non-numeric budget is reported as unfinished", startedAt: strconv.Itoa(now), budget: "thirteen", wantTitle: unmeasurable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			budget := tc.budget
			if budget == "" {
				budget = strconv.Itoa(budgetMinutes)
			}

			run := runClaudeReviewStep(t, script, map[string]string{
				"STARTED_AT":     tc.startedAt,
				"BUDGET_MINUTES": budget,
				// PATH is handed over through the env map rather than set on
				// this process: runVerifierScriptWithEnv appends the map after
				// os.Environ(), and os/exec keeps the last duplicate of a key,
				// so the stub wins for the step alone and no other test sees a
				// mutated environment.
				"PATH":          stubbedPath,
				"DATE_NOW_STUB": strconv.Itoa(now),
			})

			// Exit 0 on purpose: the review step's own failure already fails
			// the job, and a second red step would bury the annotation.
			if run.err != nil {
				t.Fatalf("report step exited non-zero (%v), want 0 — a second red step buries the annotation that is the whole point of this step\noutput:\n%s", run.err, run.combined)
			}
			if got := countErrorAnnotations(run.combined); got != 1 {
				t.Fatalf("emitted %d ::error annotations, want exactly 1 — a branch that stops annotating is the silent pass this step exists to remove\noutput:\n%s", got, run.combined)
			}
			if want := "::error title=" + tc.wantTitle + "::"; !strings.Contains(run.combined, want) {
				t.Errorf("output does not contain %q\noutput:\n%s", want, run.combined)
			}
		})
	}
}

// stubbedDatePATH writes a `date` shim into a fresh directory and returns a
// PATH with that directory ahead of the inherited one, the way
// scripts/test-resolve-validated-base.sh stubs `gh`. The shim answers the one
// spelling the report step uses and defers anything else to the real binary,
// so a step that started reading the clock some other way falls through to a
// real `now` and fails its rows loudly rather than reading them as passes.
func stubbedDatePATH(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create stub bin directory: %v", err)
	}

	const shim = `#!/bin/sh
if [ "$1" = "+%s" ]; then
  printf '%s\n' "$DATE_NOW_STUB"
  exit 0
fi
exec /bin/date "$@"
`
	// #nosec G306 -- a shim is reachable through PATH only if it is
	// executable, and this one is written into a t.TempDir() the test owns.
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte(shim), 0o700); err != nil {
		t.Fatalf("write date stub: %v", err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// countErrorAnnotations counts the workflow-command lines GitHub renders as
// check annotations. Counted rather than merely searched for, because two
// branches annotating at once reports two causes for one failure.
func countErrorAnnotations(output string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "::error") {
			count++
		}
	}
	return count
}

// TestClaudeReviewBudgetFitsInsideTheJobCap pins the two numbers the report
// step's message rests on. Neither is inside a run: block, so executing a step
// cannot reach either.
func TestClaudeReviewBudgetFitsInsideTheJobCap(t *testing.T) {
	t.Parallel()

	wiring := readClaudeReviewBudgetWiring(t)

	// The single-source-of-truth property: the cap that stops the review and
	// the budget its annotation names must be the same number, or the message
	// can claim a budget that never fired. Anchored to the review step rather
	// than matched anywhere in the file, so the expression appearing on some
	// other step cannot satisfy it.
	wantCap := "${{ fromJSON(env." + claudeReviewBudgetEnv + ") }}"
	if got, _ := wiring.reviewStepCap.(string); got != wantCap {
		t.Errorf("%s timeout-minutes = %#v, want %q — a cap that does not derive from %s lets the report step name a budget that stopped nothing",
			claudeReviewRunStepName, wiring.reviewStepCap, wantCap, claudeReviewBudgetEnv)
	}

	// The job cap must clear the review budget by more than the steps around
	// the review can cost, or the job is canceled before the review step can
	// fail — and a canceled job runs nothing, so nothing reports. Worst case
	// measured from the workflow, in seconds:
	//
	//   30   job setup + fetch-depth:0 checkout + origin preparation
	//   60   checkout's internal retry, when it fires
	//   92   Publish and verify terminal Claude review: three `timeout 30s` gh
	//        calls — pull request refresh, comment POST, read-back
	//   ---
	//   182  = 3m02s, rounded up to a 4-minute floor
	//
	// Raise this alongside the cap if the surrounding steps grow; it is a
	// floor on the gap, not a prediction of it.
	const minHeadroomMinutes = 4

	if headroom := wiring.jobCapMinutes - wiring.budgetMinutes; headroom < minHeadroomMinutes {
		t.Errorf("job cap %dm leaves %dm over the %dm review budget, under the %dm floor; too thin for setup, checkout retry and verify",
			wiring.jobCapMinutes, headroom, wiring.budgetMinutes, minHeadroomMinutes)
	}
}

// TestClaudeReviewGateRefusesAnEmptyPass drives the conclusion gate. Its
// failing branch is the one that never runs in practice and the only thing
// standing between a future edit and a green `claude-review` with nothing
// behind it, so it is asserted here rather than discovered in production.
func TestClaudeReviewGateRefusesAnEmptyPass(t *testing.T) {
	t.Parallel()
	requireCommand(t, "bash")

	const detail = "A bot-authored pull request cannot receive this secrets-bearing review."
	script := claudeReviewStepScript(t, claudeReviewGateStepName)

	tests := []struct {
		name        string
		env         map[string]string
		wantPass    bool
		wantSummary string
	}{
		{
			name:     "a published review passes",
			env:      map[string]string{"ELIGIBLE": "true", "PUBLISHED": "success"},
			wantPass: true,
		},
		{
			name:        "a declared exemption passes and is recorded",
			env:         map[string]string{"ELIGIBLE": "false", "EXEMPTION": "bot", "EXEMPTION_DETAIL": detail},
			wantPass:    true,
			wantSummary: detail,
		},
		// The hole itself: a reviewable pull request whose review never
		// published. Before the gate this concluded green.
		{
			name: "a reviewable pull request with no published review fails",
			env:  map[string]string{"ELIGIBLE": "true", "PUBLISHED": "skipped"},
		},
		{
			name: "a reviewable pull request with no publish outcome at all fails",
			env:  map[string]string{"ELIGIBLE": "true", "PUBLISHED": ""},
		},
		// The hole from the other side: `eligible=false` is only an exemption
		// when the step above said why.
		{
			name: "an exemption with no reason fails",
			env:  map[string]string{"ELIGIBLE": "false"},
		},
		{
			name: "an exemption with no explanation fails",
			env:  map[string]string{"ELIGIBLE": "false", "EXEMPTION": "bot", "EXEMPTION_DETAIL": ""},
		},
		{
			name: "an eligibility step that never ran fails",
			env:  map[string]string{"ELIGIBLE": "", "PUBLISHED": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{"ELIGIBLE": "", "EXEMPTION": "", "EXEMPTION_DETAIL": "", "PUBLISHED": ""}
			for key, value := range tc.env {
				env[key] = value
			}

			run := runClaudeReviewStep(t, script, env)
			if tc.wantPass {
				if run.err != nil {
					t.Fatalf("gate failed, want pass: %v\noutput:\n%s", run.err, run.combined)
				}
				if tc.wantSummary != "" && !strings.Contains(run.summary, tc.wantSummary) {
					t.Errorf("job summary = %q, want it to record %q — a pass without a review has to be visible on the run", run.summary, tc.wantSummary)
				}
				return
			}

			if run.err == nil {
				t.Fatalf("gate passed, want failure — this is an empty pass on a required check\noutput:\n%s", run.combined)
			}
			if !strings.Contains(run.combined, "::error title=Claude review check has no basis::") {
				t.Errorf("output = %q, want the no-basis annotation", run.combined)
			}
		})
	}
}

// TestClaudeReviewHoldsEverySecretBearingStepBehindTheChain guards the one
// property this restructuring actually traded away.
//
// Before it, a bot or fork pull request was held off by the job never starting
// — the job-level `if:` was itself the defense. The job now always starts, so
// that defense is entirely the step-level guards chaining back to the
// eligibility verdict. The workflow comment claims "every later step already
// chains off its outcome"; this is what makes the claim true rather than
// aspirational.
//
// Written as a rule over anchors rather than a list of step names, so a step
// nobody has written yet is covered too. That matters here more than usual:
// this workflow runs on `pull_request_target`, so a pull request adding an
// unguarded secret-bearing step gets a green `claude-review` from the default
// branch's copy of the file, which says nothing about the step it added.
func TestClaudeReviewHoldsEverySecretBearingStepBehindTheChain(t *testing.T) {
	t.Parallel()

	job := claudeReviewJob(t, readWorkflow(t, claudeReviewWorkflow))
	raw := claudeReviewRawSteps(t)
	if len(raw) != len(job.Steps) {
		t.Fatalf("parsed %d steps but %d raw steps; the two views disagree", len(job.Steps), len(raw))
	}

	// Mentioning an anchor is not enough. On a withheld pull request the
	// eligibility verdict is `false` and every step below it is `skipped`, so
	// `eligible == 'false'` and `outcome == 'skipped'` are *true* there — a
	// step guarded that way names an anchor and still runs on exactly the paths
	// this job must not touch. Each anchor therefore carries the comparisons
	// that are true only when the chain actually ran, and every occurrence has
	// to be one of them. `claude-review` admits 'failure' as well as 'success'
	// because a step can only fail if it ran, which is what the report step
	// keys on.
	//
	// This is a rule about spelling, not an evaluation of the expression: it
	// cannot see through `||`, a negation, or an anchor read inside a `run:`
	// body. It is the shape of the guard that is pinned here.
	anchors := []chainAnchor{
		{"steps.eligibility.outputs.eligible", []string{" == 'true'"}},
		{"steps.checkout.outcome", []string{" == 'success'"}},
		{"steps.review_origin.outputs.ready", []string{" == 'true'"}},
		{"steps.claude-review.outcome", []string{" == 'success'", " == 'failure'"}},
	}
	// GitHub allows a condition to wrap across lines; compare it as one line.
	chained := func(condition string) bool {
		flattened := strings.Join(strings.Fields(condition), " ")
		reached := false
		for _, anchor := range anchors {
			for _, occurrence := range anchorOccurrences(flattened, anchor.expression) {
				if !anchor.allows(occurrence) {
					return false
				}
				reached = true
			}
		}
		return reached
	}

	sawSecret := false
	for i := range job.Steps {
		current := &job.Steps[i]
		// The eligibility step is the root of the chain and the conclusion gate
		// is deliberately outside it; both are pinned by
		// TestClaudeReviewConcludesOnEveryPullRequest.
		isRoot, isGate := i == 0, i == len(job.Steps)-1
		if !isRoot && !isGate && !chained(current.If) {
			t.Errorf("step %q has if: %q, which reaches none of %v — a step that does not chain back to the eligibility verdict runs on bot and fork pull requests, which this job no longer withholds by skipping",
				current.Name, current.If, anchors)
		}
		if !strings.Contains(raw[i], "secrets.") {
			continue
		}
		sawSecret = true
		if isRoot || isGate || !chained(current.If) {
			t.Errorf("step %q reads a secret but its if: %q does not chain back to the eligibility verdict; on a fork or bot pull request that spends the secret on untrusted input",
				current.Name, current.If)
		}
	}

	// Without a secret-bearing step to find, the loop above proves nothing
	// about secrets — and this job exists to run one.
	if !sawSecret {
		t.Error("no step in the claude-review job references secrets.; this assertion matched nothing and would pass however the guards were edited")
	}
}

// chainAnchor is one step output the guards key on, with the comparisons that
// hold only when that step actually ran.
type chainAnchor struct {
	expression string
	reviewable []string
}

func (a *chainAnchor) allows(occurrence string) bool {
	for _, comparison := range a.reviewable {
		if strings.HasPrefix(occurrence, comparison) {
			return true
		}
	}
	return false
}

// anchorOccurrences returns the text following each mention of expression, so
// the caller can check what the guard compares it against.
func anchorOccurrences(condition, expression string) []string {
	following := []string{}
	for rest := condition; ; {
		_, after, found := strings.Cut(rest, expression)
		if !found {
			return following
		}
		following = append(following, after)
		rest = after
	}
}

// claudeReviewRawSteps re-reads the workflow as untyped YAML and renders each
// step of the claude-review job back to text. The typed `step` struct models
// only the fields these tests assert on, so a secret reaching the job through a
// key it does not model would be invisible; this sees every key.
func claudeReviewRawSteps(t *testing.T) []string {
	t.Helper()

	// #nosec G304 -- a checked-in workflow file name, fixed by a constant.
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", claudeReviewWorkflow))
	if err != nil {
		t.Fatalf("read %s: %v", claudeReviewWorkflow, err)
	}

	var raw struct {
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse %s: %v", claudeReviewWorkflow, err)
	}

	steps := raw.Jobs[claudeReviewJobID].Steps
	if len(steps) == 0 {
		t.Fatalf("%s job has no raw steps", claudeReviewJobID)
	}

	rendered := make([]string, 0, len(steps))
	for _, current := range steps {
		text, err := yaml.Marshal(current)
		if err != nil {
			t.Fatalf("render step: %v", err)
		}
		rendered = append(rendered, string(text))
	}
	return rendered
}

// TestClaudeReviewKeepsItsSecurityProperties pins what the restructuring was
// not allowed to cost. This job holds ANTHROPIC_API_KEY and runs on
// `pull_request_target`, where the trigger's own defense is that the workflow
// comes from the default branch; everything below is the rest of that defense.
func TestClaudeReviewKeepsItsSecurityProperties(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, claudeReviewWorkflow)
	triggers := parseWorkflowTriggers(t, workflow.On)
	if _, ok := triggers["pull_request_target"]; !ok {
		t.Errorf("%s must stay on pull_request_target, which loads this file from the trusted default branch", claudeReviewWorkflow)
	}
	if _, ok := triggers["pull_request"]; ok {
		t.Errorf("%s must not run on pull_request; that trigger would load pull-request-authored workflow code into a secrets-bearing run", claudeReviewWorkflow)
	}

	permissions, ok := workflow.Permissions.(map[string]any)
	if !ok || len(permissions) != 0 {
		t.Errorf("%s workflow-level permissions = %#v, want an empty mapping so every job opts in explicitly", claudeReviewWorkflow, workflow.Permissions)
	}

	job := claudeReviewJob(t, workflow)
	assertJobPermissions(t, claudeReviewJobID, job.Permissions, map[string]string{
		"contents":      "read",
		"pull-requests": "write",
		"issues":        "read",
	})

	// PR-authored files never enter this workspace: the checkout takes the
	// default branch, and the review reads the pull request through GitHub MCP.
	checkout := claudeReviewStep(t, &job, claudeReviewCheckoutStepName)
	if !strings.HasPrefix(checkout.Uses, checkoutActionPrefix) {
		t.Errorf("%s uses = %q, want an %s action", claudeReviewCheckoutStepName, checkout.Uses, checkoutActionPrefix)
	}
	if got := checkout.With["ref"]; got != "${{ github.event.repository.default_branch }}" {
		t.Errorf("checkout ref = %v, want the default branch — any pull-request ref puts authored files in a secrets-bearing workspace", got)
	}
	if got := checkout.With["persist-credentials"]; got != false {
		t.Errorf("checkout persist-credentials = %v, want false — the review origin is credential-free and the steps assert it stayed that way", got)
	}

	review := claudeReviewStep(t, &job, claudeReviewRunStepName)
	assertClaudeReviewToolAccess(t, &review)
}

// assertClaudeReviewToolAccess holds the review to read-only GitHub MCP. The
// deny-list is the backstop for local execution and network reach; the
// allow-list is what the review can actually call, and widening either is how
// a prompt-injected review turns into a write.
func assertClaudeReviewToolAccess(t *testing.T, review *step) {
	t.Helper()

	args, ok := review.With["claude_args"].(string)
	if !ok {
		t.Fatalf("%s claude_args = %#v, want a string", claudeReviewRunStepName, review.With["claude_args"])
	}

	wantDenied := []string{
		"Bash", "Read", "Glob", "Grep", "LS", "Task", "Edit", "Write", "MultiEdit",
		"NotebookEdit", "WebFetch", "WebSearch",
		"mcp__github_file_ops__commit_files", "mcp__github_file_ops__delete_files",
		"mcp__github__create_or_update_file", "mcp__github__push_files",
		"mcp__github__delete_file", "mcp__github__add_issue_comment",
	}
	// assertSameContexts reports which tool moved and on which side, rather
	// than leaving a reader to eyeball-diff two 18-element dumps.
	assertSameContexts(t,
		"the deny-list this review is pinned to", wantDenied,
		`claude_args --disallowed-tools`, claudeArgsToolList(t, args, "--disallowed-tools"))

	// Read-only by construction rather than by enumeration: a new reader can be
	// added without touching this test, but a writer cannot.
	for _, tool := range claudeArgsToolList(t, args, "--allowed-tools") {
		name, isGitHubMCP := strings.CutPrefix(tool, "mcp__github__")
		if !isGitHubMCP {
			t.Errorf("--allowed-tools grants %q, which is not a GitHub MCP read tool", tool)
			continue
		}
		if !strings.HasPrefix(name, "get_") && !strings.HasPrefix(name, "list_") && !strings.HasPrefix(name, "search_") {
			t.Errorf("--allowed-tools grants %q, which is not a get_/list_/search_ read", tool)
		}
	}
}

// claudeArgsToolList pulls one `--flag "a,b,c"` value out of the review step's
// claude_args string. It assumes the single-line, double-quoted form the step
// uses today: each flag's value is one quoted token, and no value contains
// another flag's `--name "` spelling. Reflow claude_args across lines and this
// parser needs revisiting — it would fail loudly on the missing flag rather
// than silently accept a widened tool list.
func claudeArgsToolList(t *testing.T, args, flag string) []string {
	t.Helper()

	_, after, ok := strings.Cut(args, flag+` "`)
	if !ok {
		t.Fatalf("%s claude_args has no %s value", claudeReviewRunStepName, flag)
	}
	value, _, ok := strings.Cut(after, `"`)
	if !ok {
		t.Fatalf("%s %s value is unterminated", claudeReviewRunStepName, flag)
	}

	tools := []string{}
	for _, tool := range strings.Split(value, ",") {
		if tool = strings.TrimSpace(tool); tool != "" {
			tools = append(tools, tool)
		}
	}
	if len(tools) == 0 {
		t.Fatalf("%s %s is empty", claudeReviewRunStepName, flag)
	}
	return tools
}

func claudeReviewJob(t *testing.T, workflow githubWorkflow) githubJob {
	t.Helper()

	job, ok := workflow.Jobs[claudeReviewJobID]
	if !ok {
		t.Fatalf("%s is missing the %s job; that job id is the required context's name", claudeReviewWorkflow, claudeReviewJobID)
	}
	// The check renders under the job id only while the job sets no name.
	// TestDocumentedRequiredContextsResolveToWorkflowJobs would also catch a
	// respelling, since `claude-review` is in CONTRIBUTING.md's required-context
	// block; this fatal exists so the lookups below fail with the cause rather
	// than with a missing step.
	if job.Name != "" {
		t.Fatalf("%s job sets name = %q, which respells the required context away from %q", claudeReviewJobID, job.Name, claudeReviewJobID)
	}
	return job
}

// claudeReviewStepScript returns one step's run: block, for the tests that
// execute a step rather than read its shape.
func claudeReviewStepScript(t *testing.T, name string) string {
	t.Helper()

	job := claudeReviewJob(t, readWorkflow(t, claudeReviewWorkflow))
	return claudeReviewStep(t, &job, name).Run
}

func claudeReviewStep(t *testing.T, job *githubJob, name string) step {
	t.Helper()

	for _, candidate := range job.Steps {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("%s job is missing the %q step", claudeReviewJobID, name)
	return step{}
}

// claudeReviewBudgetWiring is the job's declared review budget together with
// the two caps that have to bracket it.
type claudeReviewBudgetWiring struct {
	budgetMinutes int
	jobCapMinutes int
	// reviewStepCap stays `any` so a cap written as a bare number reports as
	// the wrong value rather than as a missing one: timeout-minutes accepts a
	// literal or a `${{ }}` expression, and only one of those is correct here.
	reviewStepCap any
}

// readClaudeReviewBudgetWiring re-reads the workflow as untyped YAML for the
// three values around the budget. None is on the job's or the step's `if:`, so
// none is reachable by executing a step.
//
// Read raw rather than by widening githubJob and step, for the reason
// claudeReviewRawSteps gives one field over: both structs are copied by value
// in the range loops of this package's workflow-wide assertions, and each sits
// just under the size at which that copy becomes a lint finding. Growing them
// for three fields only this file reads charges every one of those loops for it.
func readClaudeReviewBudgetWiring(t *testing.T) claudeReviewBudgetWiring {
	t.Helper()

	// #nosec G304 -- a checked-in workflow file name, fixed by a constant.
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", claudeReviewWorkflow))
	if err != nil {
		t.Fatalf("read %s: %v", claudeReviewWorkflow, err)
	}

	var raw struct {
		Jobs map[string]struct {
			TimeoutMinutes any            `yaml:"timeout-minutes"`
			Env            map[string]any `yaml:"env"`
			Steps          []struct {
				Name           string `yaml:"name"`
				TimeoutMinutes any    `yaml:"timeout-minutes"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse %s: %v", claudeReviewWorkflow, err)
	}

	job, ok := raw.Jobs[claudeReviewJobID]
	if !ok {
		t.Fatalf("%s is missing the %s job", claudeReviewWorkflow, claudeReviewJobID)
	}

	wiring := claudeReviewBudgetWiring{
		budgetMinutes: claudeReviewMinutes(t, claudeReviewBudgetEnv, job.Env[claudeReviewBudgetEnv]),
		jobCapMinutes: claudeReviewMinutes(t, claudeReviewJobID+" job timeout-minutes", job.TimeoutMinutes),
	}

	for _, current := range job.Steps {
		if current.Name == claudeReviewRunStepName {
			wiring.reviewStepCap = current.TimeoutMinutes
			return wiring
		}
	}
	t.Fatalf("%s job is missing the %q step, which is where the budget is spent", claudeReviewJobID, claudeReviewRunStepName)
	return claudeReviewBudgetWiring{}
}

// claudeReviewMinutes reads a minute count that has to be a literal. The report
// step compares the budget arithmetically and the job cap is GitHub's own kill
// timer; neither can take a value computed at run time, and an expression here
// would leave the headroom floor comparing against something it cannot see.
func claudeReviewMinutes(t *testing.T, label string, value any) int {
	t.Helper()

	minutes, ok := value.(int)
	if !ok || minutes <= 0 {
		t.Fatalf("%s = %#v, want a positive integer literal", label, value)
	}
	return minutes
}

// claudeReviewStepRun is one execution of a workflow step's run: block, with
// the two runner-provided files the step writes through.
type claudeReviewStepRun struct {
	combined string
	outputs  map[string]string
	summary  string
	err      error
}

func runClaudeReviewStep(t *testing.T, script string, env map[string]string) claudeReviewStepRun {
	t.Helper()

	if strings.TrimSpace(script) == "" {
		t.Fatal("step has an empty run script")
	}

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "github_output")
	summaryPath := filepath.Join(dir, "github_step_summary")
	for _, path := range []string{outputPath, summaryPath} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create %s: %v", filepath.Base(path), err)
		}
	}

	stepEnv := maps.Clone(env)
	// These beat a real runner's own GITHUB_OUTPUT when the suite runs inside
	// Actions: runVerifierScriptWithEnv appends the whole map after
	// os.Environ(), and os/exec keeps the last duplicate. Assigning them here
	// rather than in the loop only shuts out a caller-supplied override, which
	// no caller passes. Nothing subtle rests on either: a step that wrote to
	// the ambient file would read back empty here and fail loudly.
	stepEnv["GITHUB_OUTPUT"] = outputPath
	stepEnv["GITHUB_STEP_SUMMARY"] = summaryPath

	combined, err := runVerifierScriptWithEnv(t, script, stepEnv)
	return claudeReviewStepRun{
		combined: combined,
		outputs:  readStepOutputs(t, outputPath),
		summary:  readStepFile(t, summaryPath),
		err:      err,
	}
}

func readStepOutputs(t *testing.T, path string) map[string]string {
	t.Helper()

	outputs := map[string]string{}
	for _, line := range strings.Split(readStepFile(t, path), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("GITHUB_OUTPUT line %q is not key=value", line)
		}
		outputs[key] = value
	}
	return outputs
}

func readStepFile(t *testing.T, path string) string {
	t.Helper()

	// #nosec G304 -- path is a file this test created under t.TempDir().
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(data)
}
