# Contributing to qurl-integrations

Thank you for your interest in contributing! This guide will help you get started.

## Quick Start

```bash
# Clone and install tools
git clone https://github.com/layervai/qurl-integrations.git
cd qurl-integrations

# Install pre-commit hooks (required — CI will catch what these catch, but faster locally)
pip install pre-commit && pre-commit install

# Verify everything works
make check
```

`make check` covers the Go apps, `shared/`, and the repo-wide checks. It does
**not** run the Node.js suites — those are opt-in, see
[Node.js apps](#nodejs-apps) below.

`make check` shells out to `python3` for the repo-consistency checks
(`scripts/check-release-please-sync.sh`, `scripts/check-extension-lockstep.sh`).
Both fail with an explicit install message rather than silently passing if it is
missing.

## Project Structure

Each integration lives in `apps/{name}/`. Shared libraries live in `shared/`.

```
apps/
  slack/             # Slack integration
    cmd/main.go      # Lambda entry point
    internal/        # App-private code — put your logic here
    README.md
  discord/           # Discord bot
  chrome-extension/  # Chrome MV3 extension for Gmail
  edge-extension/    # Edge MV3 extension for Gmail (fork of chrome-extension, kept in lockstep)
  cli/               # CLI tool
  teams/             # Microsoft Teams OAuth core (TypeScript, not yet shipped)
  zapier/            # Zapier integration (placeholder, no implementation yet)
origins/
  s3-static-connector/ # Reusable private S3 static origin image
shared/              # Shared Go libraries used by the Go apps
e2e/                 # TypeScript end-to-end tests (Jest)
```

## Boundaries

| Directory | Who owns it | Can you modify it? |
|-----------|------------|-------------------|
| `apps/{app}/` | Maintainers | Yes — open a PR |
| `origins/{origin}/` | Maintainers | Yes — open a PR |
| `shared/` | Maintainers | PR requires maintainer review |
| `.github/` | Maintainers | PR requires maintainer review |

Changes to `shared/` trigger tests for ALL apps, so coordinate with maintainers.

## Workflow

```bash
# 1. Always start from latest main
git checkout main && git pull

# 2. Create a branch (never commit to main)
git checkout -b feat/slack-thread-replies

# 3. Write code, then verify
make check                    # Go apps, shared/, and repo-wide checks
make check-<app>              # only if you touched a Node.js suite (see below)
make build-slack              # Verify Lambda binary compiles (adjust for your app)

# 4. Push and open a PR
git push -u origin feat/slack-thread-replies
gh pr create --title "feat(slack): add thread replies"
```

### Node.js apps

`make check` stops at the Go boundary. Each Node.js app installs from its own
lockfile, so the suites are opt-in targets rather than prerequisites of
`make check`:

- `make check-chrome-extension`, `check-edge-extension`, `check-discord`,
  `check-teams` — each mirrors that app's `<app> / build and test` job.
- `make check-e2e` — mirrors `e2e / build and test`: `e2e/`'s offline subset
  (typecheck plus `test:unit`). The live suite is excluded from both this
  target and CI, deliberately — it mints real qURL resources, posts real
  Discord messages, and needs credentials in `e2e/.env` that CI does not have.
- `make check-node` — all five.

Go by what your change *triggers*, not just which directory it sits in:
`discord.yml`'s filter includes `shared/**`, so a Go-only change under
`shared/` runs Discord CI and is worth a `make check-discord`.

A green run predicts that app's `*/ required` aggregate, but CI stays the gate.
Each CI job carries a comment pointing back at its target, and each target
records what it omits and why — a step added to one belongs in the other.

## PR Requirements

All of these must pass before merge:

- **PR title** follows [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description`
  - Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
    `build`, `ci`, `chore`, `revert`
  - Scopes: `slack`, `teams`, `discord`, `cli`, `zapier`,
    `chrome-extension`, `edge-extension`, `origins`, `shared`, `ci`
    (`.github/workflows/pr-title.yml` also accepts repository-maintenance
    scopes `infra` and `deps`, tracked in #463.)
- **Linting** passes (golangci-lint with 28+ linters — see `.golangci.yml`)
- **Tests** pass with `-race`
- **Build** succeeds (Lambda binary)
- **Code review** approved (CODEOWNERS auto-assigns reviewers)
- **No high-severity vulnerabilities** in new dependencies
- **No GPL-3.0 / AGPL-3.0** licensed dependencies
- **GitHub Actions refs** use full 40-character commit SHAs plus the exact
  upstream version tag as the first trailing comment, for example
  `owner/action@<sha> # v1.2.3`. `docker://` actions are not used in this repo.
  The validator enforces format only; reviewers own tag/SHA correctness. Before
  changing a tag comment, confirm it matches the pinned SHA with `git ls-remote
  https://github.com/OWNER/REPO.git refs/tags/v1.2.3`.
  - **Dependabot:** The `github-actions` updater updates pinned SHAs and exact
    trailing tag comments together. This behavior was first observed on PR
    #639, where Dependabot bumped `anthropics/claude-code-action` from
    `# v1.0.133` to `# v1.0.148` in both workflow comments. Future Dependabot
    PRs should not need manual comment edits; if one leaves them out of sync,
    update the SHA and first trailing comment together before merge.

### Merge-result checks

`main` protection requires strict status checks, so required checks must be
green for the current `main` merge result before a PR can merge. If `main`
moves after checks turn green, update the PR branch or rerun checks against the
new merge result before merging. When required contexts change in GitHub
settings, update this section in the same operational change.

App- and shared-impacting PRs report always-present aggregate checks, and
branch protection requires all eight: `slack / required`, `discord / required`,
`chrome-extension / required`, `edge-extension / required`, `teams / required`,
`s3-static-connector / required`, `e2e / required`, and `shared / required`.
The connector aggregate arrived in #1042 (refs #1022), which moved
`s3-static-connector.yml` off `on.push.paths` onto the `changes`-job pattern;
it and `e2e / required` were the last two still awaiting the settings change,
which landed 2026-08-17 with the repair described below. Each workflow's
`changes` filter is the source of truth for which paths need validation. When
that filter matches, the aggregate validates every quality gate listed in its
workflow `needs:` set. Branch protection requires only these aggregate checks,
never the internally skipped expensive jobs.

Every PR is gated by the always-present `Validate GitHub Actions pins` check.
The job re-scans all workflow and composite-action files, checks external
`uses:` refs against the GitHub Actions refs rule above, skips local `./` refs,
and also gates on the validator self-test. The validator reports violations for
missing `@` refs, non-SHA refs, missing or malformed version comments, and
`docker://` actions. A defensive guard exits non-zero if no files are scanned.
Any job failure blocks the PR until fixed, even when the PR did not introduce
it. Branch protection requires the exact `Validate GitHub Actions pins`
context. It is separate from the existing
`age-check / Check GitHub Actions pin ages` context, even though both contexts
are produced by the same workflow file.

The full required set is those eight aggregates plus `Workflow Contract`,
`Validate GitHub Actions pins`, and the four `age-check / *` contexts —
fourteen in all. **Required contexts match case-sensitively.** From 2026-08-14
to 2026-08-17 the required list was a single context spelled `Workflow
contract` (lowercase `c`), which no job ever reports, so nothing gated and
every merge went through an admin override. The API call that adds a context
*replaces* the list rather than appending to it, so the same edit also dropped
the six contexts required until then. When changing required contexts, send the
complete desired set in one `PATCH`, spell each one exactly as the job's
`name:` renders it, and verify with `gh pr checks <open-PR> --required` — it
prints `no required checks reported` when a context matches nothing, which is
the only cheap way to catch a typo. Note that PRs opened by release-please
carry no checks at all, because GitHub does not fire `pull_request` workflows
for events created by `GITHUB_TOKEN`; that PR needs an admin override to merge
regardless of this set.

## Code Conventions

- **Error handling**: Always check errors. Use `errors.Is`/`errors.As`, not type assertions.
- **Context**: Pass `context.Context` as the first parameter.
- **Logging**: Use the shared observability package, not `fmt.Println` or `log`.
- **HTTP clients**: Always pass context (`req.WithContext(ctx)`).
- **Constants**: If you use the same string 3+ times, make it a constant.
- **Complexity**: Max cognitive complexity 30, cyclomatic complexity 20.
- **Tests**: Use `t.Parallel()` where possible. Table-driven tests preferred.

## Using Shared Packages

```go
import (
    "github.com/layervai/qurl-integrations/shared/client"
    "github.com/layervai/qurl-integrations/shared/auth"
    "github.com/layervai/qurl-integrations/shared/formatting"
)
```

If you need something that doesn't exist in `shared/`, start by putting it in your app's `internal/` package. If multiple apps need it, open an issue to discuss promoting it to `shared/`.

## Common Mistakes

1. **Pushing to main** — Branch protection will reject it, but don't try.
2. **Modifying `shared/` without coordination** — Your change runs tests for ALL apps.
3. **Ignoring linter errors** — `//nolint` requires an explanation AND a specific linter name.
4. **Adding large dependencies** — Dependency review will flag high-severity or GPL deps.
5. **Hardcoding secrets** — Use environment variables. `detect-private-key` hook catches some of this.
6. **Skipping `make check`** — CI runs the same checks. Save yourself the round-trip.
7. **Assuming `make check` covered a Node.js app** — it doesn't. Run the
   matching `make check-<app>` too (see [Node.js apps](#nodejs-apps)).

## Getting Help

- **Bug reports**: [Open an issue](https://github.com/layervai/qurl-integrations/issues/new?template=bug_report.md)
- **Feature requests**: [Open an issue](https://github.com/layervai/qurl-integrations/issues/new?template=feature_request.md)
- **Questions**: [Open an issue](https://github.com/layervai/qurl-integrations/issues) with the `question` label
