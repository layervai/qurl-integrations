# Branch protection — `main`

`main` currently has no protection. This file is the canonical record
of the rules we want enforced; an admin (Justin) must apply them in
the GitHub UI or via `gh` since neither the bot nor Vikram has admin
on this repo.

## Why this file exists

On 2026-05-10, [PR #212](https://github.com/layervai/qurl-integrations/pull/212)
merged into `main` with `build-and-test` red. The job ran on the PR,
failed, and the merge proceeded anyway — there was no required-check
gate. Result: `main` was broken on the unit suite until [PR #238](https://github.com/layervai/qurl-integrations/pull/238)
landed the fix. Smoke-on-staging passed independently (it doesn't
exercise the jest suite), which obscured the failure.

## Required rules

Apply to: branch `main`.

1. **Require a pull request before merging.**
   - Required approvals: 1.
   - Dismiss stale pull request approvals when new commits are pushed.
2. **Require status checks to pass before merging.**
   - Required: `build-and-test`.
   - Require branches to be up to date before merging — catches the
     "green on branch, red after rebase to main" case.
3. **Require signed commits** — matches `CLAUDE.md`'s existing rule.
4. **Do not allow bypassing the above** for non-admins.

## How to apply (admin only)

### UI

`Settings → Rules → Rulesets → New branch ruleset`:
- Name: `main protection`
- Enforcement status: **Active**
- Target branches → Include default branch (`main`)
- Rules → enable everything in the **Required rules** section above
- Required status checks → add `build-and-test` from the workflow list

### gh CLI

```bash
gh api -X PUT repos/layervai/qurl-integrations/branches/main/protection \
  -H "Accept: application/vnd.github+json" \
  --input - <<'EOF'
{
  "required_status_checks": { "strict": true, "contexts": ["build-and-test"] },
  "enforce_admins": false,
  "required_pull_request_reviews": { "required_approving_review_count": 1, "dismiss_stale_reviews": true },
  "restrictions": null,
  "required_signatures": true
}
EOF
```

## Verification

After applying, this should return a populated object (not a 404):

```bash
gh api repos/layervai/qurl-integrations/branches/main/protection
```

And a follow-up test PR that introduces a deliberate jest failure
should fail to merge until reverted.
