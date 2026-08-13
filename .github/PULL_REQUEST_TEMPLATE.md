## Summary

Brief description of changes.

## Scope

<!-- Which area(s) does this PR change? Check all that apply. -->

- [ ] `apps/slack/`
- [ ] `apps/teams/`
- [ ] `apps/discord/`
- [ ] `apps/cli/`
- [ ] `apps/zapier/`
- [ ] `apps/chrome-extension/`
- [ ] `apps/edge-extension/` (kept in lockstep with `apps/chrome-extension/`)
- [ ] `origins/` (connector image — also triggers Slack CI)
- [ ] `shared/` (triggers tests for ALL apps — coordinate with maintainers)
- [ ] `.github/` (requires maintainer review)

## Changes

- Change 1
- Change 2

## Test Plan

- [ ] `make check` passes locally (Go apps, `shared/`, and repo-wide checks)
- [ ] For Node.js app changes: the matching `make check-<app>` passes —
      `check-chrome-extension`, `check-edge-extension`, `check-discord`,
      `check-teams` (or `make check-node` for all four)
- [ ] New tests added for new functionality
- [ ] Manual testing completed
- [ ] For app/shared-impacting changes: branch is current with `main` and the matching `*/ required` aggregate check is green

## Related Issues

Closes #
