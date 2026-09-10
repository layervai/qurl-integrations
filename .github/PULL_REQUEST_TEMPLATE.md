## Summary

Brief description of changes.

## Scope

<!-- Which area(s) does this PR change? Check all that apply. -->

- [ ] `apps/slack/`
- [ ] `apps/teams/`
- [ ] `apps/discord/`
- [ ] `apps/cli/`
- [ ] Browser extension source (`apps/chrome-extension/`)
- [ ] Edge release metadata (`apps/edge-extension/`)
- [ ] `origins/` (connector image — also triggers Slack CI)
- [ ] `e2e/` (end-to-end tests — CI gates the offline subset; the live suite is local-only)
- [ ] `shared/` (triggers tests for ALL apps — coordinate with maintainers)
- [ ] `.github/` (requires maintainer review)

## Changes

- Change 1
- Change 2

## Test Plan

- [ ] `make check` passes locally (Go apps, `shared/`, and repo-wide checks)
- [ ] For Node.js changes — or `shared/`, which triggers Discord CI — the matching
      target passes: `make check-chrome-extension`, `check-edge-extension`,
      `check-discord`, `check-teams`, `check-e2e` (or `check-node` for all five)
- [ ] New tests added for new functionality
- [ ] Manual testing completed
- [ ] For app/shared-impacting changes: branch is current with `main` and the matching `*/ required` aggregate check is green

## Related Issues

Closes #
