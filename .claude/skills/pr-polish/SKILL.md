---
name: pr-polish
description: Full PR polish pipeline — runs code review, simplify, DE acceptance check, then iterates on Claude review feedback until only nits remain. Use when you want to get a PR merge-ready.
argument-hint: "<PR number or branch name>"
---

# PR Polish Pipeline

You are running the full polish pipeline on a pull request. Work through each phase sequentially. Do NOT skip phases.

**Target**: $ARGUMENTS

---

## HARD RULES — violations of these caused 6 hours of wasted time

### Rule 0: Run fully autonomously — no prompts
NEVER ask the user "Want me to fix this?", "Should I proceed?", "Do you want me to X?". The answer is always YES. Just do it. The user invoked /pr-polish to get the PR merge-ready — every fix, every push, every review cycle should happen without asking for permission. The only time to stop and ask is if you hit a genuine blocker you cannot resolve (e.g., missing credentials, merge conflict you can't resolve, architectural decision that changes the product).

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

4. **Categorize each comment** as: Critical, High, Medium, Suggestion, or Nit.

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
- Claude's review run completes and posts no new comment (or only nits)
- Tests pass
- Every claimed fix is verified with grep in the actual pushed code

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
