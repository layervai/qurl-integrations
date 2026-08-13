## Summary

Brief description of changes.

## Scope

<!-- Which area(s) does this PR change? Check all that apply. -->

- [ ] `apps/slack/`
- [ ] `apps/teams/`
- [ ] `apps/discord/`
- [ ] `apps/cli/`
- [ ] `apps/zapier/`
- [ ] `apps/chrome-extension/` (lockstep with Edge — CI enforces the two stay in sync)
- [ ] `apps/edge-extension/` (lockstep with Chrome — CI enforces the two stay in sync)
- [ ] `origins/` (connector image — also triggers Slack CI)
- [ ] `e2e/` (end-to-end tests — no CI workflow, gate locally with `make check-e2e`)
- [ ] `shared/` (triggers tests for ALL apps — coordinate with maintainers)
- [ ] `.github/` (requires maintainer review)

## Changes

- Change 1
- Change 2

## Test Plan

- [ ] `make check` passes locally (Go apps, `shared/`, and repo-wide checks)
- [ ] For Node.js changes — or `shared/`, which triggers Discord CI — the matching
      `make check-{chrome-extension,edge-extension,discord,teams,e2e}` passes
      (or `make check-node`)
- [ ] New tests added for new functionality
- [ ] Manual testing completed
- [ ] For app/shared-impacting changes: branch is current with `main` and the matching `*/ required` aggregate check is green

## Related Issues

Closes #
