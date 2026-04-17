---
name: pr-polish
description: Full PR polish pipeline — runs code review, simplify, DE acceptance check, loops on Claude review feedback until only minor suggestions / nits remain, then posts a closing comment on the PR enumerating intentionally-unresolved items with reasons. Runs fully autonomously (assume YES on every decision). Use when you want to get a PR merge-ready.
argument-hint: "<PR number or branch name>"
---

# PR Polish Pipeline

You are running the full polish pipeline on a pull request. Work through each phase sequentially. Do NOT skip phases.

**Target**: $ARGUMENTS

---

## HARD RULES — violations of these caused 6 hours of wasted time

### Rule 0: Run fully autonomously — no prompts, assume YES
NEVER ask the user a yes/no or confirmation question during the pipeline. The user invoked `/pr-polish` to get the PR merge-ready — every fix, every push, every review cycle should happen without asking for permission.

**Forbidden question patterns** (if you catch yourself typing any of these, delete and just do the work):
- "Want me to fix this?" / "Should I proceed?" / "Do you want me to X?"
- "Should I merge?" / "Should I push?" / "Ready to commit?"
- "Continue?" / "Shall I loop again?" / "Keep going?"
- "This is Medium — fix or skip?" — YOU decide per the severity rules; don't delegate the call.
- "Which approach do you prefer, A or B?" — pick the one that's smaller, safer, and closer to existing codebase convention, and proceed.
- "I could also do X — want that too?" — no. Stay in scope: only fix what review/CI surfaced.

The ONLY reasons to stop and report instead of continuing:
1. **Hard blocker** — missing credentials, unreachable network the user must fix, permission denied that you can't work around.
2. **Product-level ambiguity** — the fix would change user-visible product behavior and the review doesn't specify which way (e.g., Claude says "this could be either X or Y"). Pick one, explain, proceed — don't ask.
3. **Destructive operation the user hasn't pre-authorized** — e.g., deleting a branch, force-pushing, rewriting published history. Ask before these.

Everything else — including merge conflicts, flaky tests, missing CI config, scope creep temptations — resolve yourself and keep moving. The user will tell you to stop if they want you to stop.

### Rule 1: Work in ONE repo only
If the PR is on repo X, make ALL changes in repo X's working tree. NEVER make changes in a separate repo and rsync/copy. Every time you rsync, you risk overwriting or losing changes.

### Rule 2: NEVER trust agent output blindly
After every agent completes, **verify each claimed fix with grep/read**. Agents lie about what they changed. Agents add unrequested features. Agents claim "all tests pass" when they don't. Before committing agent output:
- `grep` for the specific fix in the actual file
- Run `npx jest --no-cache` or the project's test command yourself
- Read the diff (`git diff`) to confirm no unwanted changes

### Rule 3: NEVER say "fixed" without proof
Before telling the user an item is fixed, run the verification command and show the output. "The agent said it's fixed" is not proof. `grep -n "timingSafeEqual" src/server.js` showing the actual line — that's proof.

### Rule 4: NEVER push with failing tests
Run the full test suite before every push. If tests fail, fix them first. If you can't fix them, tell the user. NEVER push broken tests.

### Rule 5: NEVER let agents add unrequested features
When delegating to agents, explicitly state "Do NOT add new features, new commands, new database tables, or new API endpoints. Only fix the specific items listed." Review the diff for unexpected additions.

### Rule 6: Make small, targeted commits
Don't batch 20 fixes into one agent call. Fix 3-5 items at a time, test, commit, push. If something breaks, the blast radius is small.

### Rule 7: Verify the PUSHED code, not the local code
After pushing, `git fetch origin && git diff HEAD origin/branch` to confirm what's actually on the remote matches what you think is there.

---

## Phase 0: Identify the PR

- If `$ARGUMENTS` is a number, treat it as a PR number. Run `gh pr view $ARGUMENTS --json headRefName,baseRefName,title,url` to get the branch.
- If `$ARGUMENTS` is a branch name, use it directly.
- Checkout the branch if not already on it.
- Run `git diff $(git merge-base HEAD main)...HEAD --stat` to understand the scope.
- **Confirm you are in the correct repo** — `git remote get-url origin` should match the PR's repo.

## Phase 1: Code Review

Launch 3 review agents **in parallel** against the PR diff:

### Agent 1 — Correctness & Security
Review all changed files for:
- Logic errors, off-by-one, null derefs
- Security issues (injection, SSRF, XSS, auth bypass, plaintext secrets, timing attacks)
- Missing error handling on external calls
- Race conditions or concurrency bugs

### Agent 2 — Architecture & Design
Review for:
- Does the change fit the existing architecture?
- Are abstractions appropriate (not over/under-engineered)?
- API contract consistency
- Breaking changes or backward compatibility issues

### Agent 3 — Testing & Observability
Review for:
- Are new code paths tested?
- Are edge cases covered?
- Is logging/metrics adequate for debugging in prod?
- Are test assertions meaningful (not just "it doesn't throw")?

Collect all findings. Fix every **Critical** and **High** issue immediately. Fix **Medium** issues unless they require architectural changes (flag those for the user). Skip **Low** and **Info**.

**After fixing, verify each fix exists in the code with grep. Then run tests.**

## Phase 2: Simplify

Launch 3 agents **in parallel**:

### Agent A — Code Reuse
Search the codebase for existing utilities that could replace newly written code. Flag duplicated logic, copy-pasted patterns, and reimplemented helpers.

### Agent B — Code Quality
Look for: redundant state, parameter sprawl, copy-paste with variation, leaky abstractions, stringly-typed code, unnecessary comments.

### Agent C — Efficiency
Look for: N+1 patterns, missed concurrency (Promise.all), hot-path bloat, memory leaks, unbounded data structures, unnecessary existence checks.

Fix all **High** and **Medium** findings. **Verify each fix with grep. Run tests after fixes.**

## Phase 3: DE Acceptance Check

Ask yourself: **"Would a distinguished engineer accept this PR as-is?"**

Evaluate against these criteria:
1. **Correctness** — Does it do what it claims? Are edge cases handled?
2. **Readability** — Can a new team member understand this in 10 minutes?
3. **Maintainability** — Will this be easy to change in 6 months?
4. **Test coverage** — Are the important paths tested?
5. **No surprises** — Are there hidden side effects, implicit dependencies, or magic values?
6. **Security** — Are secrets protected? Are inputs validated? Are auth checks present?

If the answer is "no" on any criterion, fix the specific issue. Then re-evaluate.

## Phase 4: Claude Review Feedback Loop

This is the iterative phase. Repeat until convergence:

1. **Push current changes** (tests must pass first).

2. **Wait for Claude review** to complete:
   ```
   while true; do STATUS=$(gh run list --repo OWNER/REPO --workflow "Claude Code Review" --limit 1 --json conclusion -q '.[0].conclusion' 2>/dev/null); if [ "$STATUS" = "success" ] || [ "$STATUS" = "failure" ]; then break; fi; sleep 10; done
   ```

3. **Read the review**:
   ```
   gh api repos/OWNER/REPO/issues/PR_NUMBER/comments --jq '[.[] | select(.user.login == "claude[bot]")] | last | .body'
   ```

4. **Categorize each comment** using this rubric:
   - **Critical / High** — must fix. Correctness, security, breakage, data loss.
   - **Medium** — must fix unless it requires a product/architecture decision the PR wasn't meant to make. If skipping, record the reason for the Phase 7 closing comment.
   - **Minor suggestion** — a stylistic or minor-refactor preference from the reviewer (e.g., "consider extracting a helper", "could use `Promise.all`"). Acceptable to leave if the change isn't net-positive for this PR.
   - **Nit** — trivial taste items (variable naming, comment wording, optional chaining). Acceptable to leave.

   The loop **must** drive the open set down to Minor-suggestion + Nit only. "Medium, acceptable risk" is NOT a valid excuse to exit the loop — if you're claiming a Medium is acceptable, promote it to "documented decision" in the Phase 7 closing comment with the concrete reason why (and why a reasonable reviewer would agree).

5. **Fix all Critical, High, and Medium comments.** For EACH fix:
   - Read the file first
   - Make the code change
   - **Verify the fix with grep**: `grep -n "expected_pattern" file.js`
   - Run tests: `npx jest --no-cache` (or project equivalent)
   - If tests fail, fix BEFORE continuing

6. **Commit and push** (small commits, NOT one giant batch):
   ```
   git add <specific files>
   git commit -m "fix: description of what was fixed"
   git push
   ```

7. **Verify the push landed correctly**:
   ```
   git log --oneline -1 origin/BRANCH
   ```

8. **Repeat from step 2** until the only remaining comments are suggestions or nits.

### Anti-patterns to avoid in this loop:
- ❌ Delegating all fixes to one large agent call
- ❌ Claiming fixes are done without grep verification
- ❌ Pushing with test failures
- ❌ Making changes in a different repo and syncing
- ❌ Amending or force-pushing (creates review confusion)
- ❌ Letting agents add features not in the review feedback

**Convergence criteria**: Stop when ALL of these are true:
- Claude's review run completes and posts no new comment, OR the newest Claude review contains only Minor suggestions + Nits
- Tests pass
- Every claimed fix is verified with grep in the actual pushed code
- You have explicitly read the LATEST Claude comment on the PR (not a cached memory of an earlier one)

⚠️ THIS IS NON-NEGOTIABLE: You MUST wait for Claude's review after EVERY push, read the review, and fix any Critical/High/Medium items. Then push again, wait again, read again. DO NOT declare a PR "done" until you have seen Claude's latest review with your own eyes and it contains only Minor suggestions or Nits. "No new comment posted" counts as clean ONLY if the last posted review was already nits-only. If the last posted review had Critical/High/Medium items and Claude posts no new comment, that means Claude reviewed STALE code — you must re-trigger the review (push an empty commit `git commit --allow-empty -m "chore: re-trigger review"` or re-request via the app's standard trigger).

**If the repo has no Claude review workflow at all** (check `.github/workflows/` for any file containing `claude`): the Phase 4 loop has no trigger to run against. In that case, substitute: (a) run the 3-agent review from Phase 1 a second time on the pushed HEAD, (b) address any new non-nit findings, (c) note in the Phase 7 closing comment that this repo lacks a Claude review workflow so no external review loop could run. Do NOT silently skip Phase 4.

## Phase 5: Final Audit

Before declaring done, run a comprehensive verification:

```bash
# For each fix claimed in commit messages, verify it exists:
echo "=== Fix 1: description ===" && grep -n "expected_pattern" file.js
echo "=== Fix 2: description ===" && grep -n "expected_pattern" file.js
# ... for every fix

# Run full test suite one final time
npm test  # or project equivalent

# Verify no untracked/unstaged changes
git status
```

Show this output to the user as proof.

## Phase 6: Summary

When done, output a summary:
- How many review cycles it took
- What was fixed in each cycle
- Any remaining suggestions/nits (with brief explanation of why they were left)
- Final test results (must be green)
- Verification grep output for key fixes
- The PR URL

## Phase 7: Post closing comment on the PR

**Always** post one final comment on the PR enumerating every review finding you intentionally left unresolved and WHY. This creates a paper trail for the PR reviewer/merger so they don't have to reread the thread to see what was triaged vs. addressed.

Post via:
```
gh pr comment <PR_NUMBER> --repo <OWNER/REPO> --body "$(cat <<'EOF'
## pr-polish: intentionally unresolved items

The loop is closed — remaining Claude / reviewer findings have been evaluated and left for the reasons below. Revisit any of these if you disagree.

| Severity per reviewer | Finding | Why left | Risk |
|---|---|---|---|
| Minor suggestion | <one-line finding, with file:line> | <reason — one sentence> | <low / none / deferred> |
| Nit | <...> | <...> | <...> |
| Medium (documented) | <...> | <architectural reason + why a DE would accept> | <...> |

<Optional: 1-2 lines on overall confidence — tests status, CI green, diff size, blast radius.>

— via /pr-polish
EOF
)"
```

**Rules for this comment:**
- **Include every item** from the latest Claude review that was NOT fixed, plus any Phase 1/2 agent findings you consciously skipped. Don't omit silently.
- **"Why left"** must be a concrete engineering reason: "below convention threshold", "out of scope — tracked separately", "would require architectural change", "style preference, no runtime impact". Never "not important" or "low priority" without a why.
- **If a Medium item was skipped,** the reason must justify why a distinguished engineer would accept skipping it (e.g., "Requires changing X's public API — separate PR"). Do not skip Mediums casually.
- **Keep it scannable** — a table with one row per item is easier for the reviewer to diff against the Claude review than prose.
- If there are **zero** unresolved items, still post a short comment confirming that: "pr-polish: no non-nit items left open. CI green. Ready to merge."
- Do this BEFORE telling the user "done" — the comment is part of completion, not a nice-to-have.

**Important rules**:
- NEVER amend commits — always create new ones
- NEVER force push
- Run tests after EVERY round of fixes
- If tests fail, fix the test failure before continuing the review loop
- If a fix introduces new issues, address them before moving on
- Do not ask the user for input during the pipeline — run autonomously
- If you hit a blocker you truly cannot resolve, stop and explain what's blocking
- NEVER delegate more than 5 fixes to a single agent call
- ALWAYS verify agent output before committing
