# Contributing to qurl-integrations

## Quick Start

```bash
# Clone and install tools
git clone git@github.com:layervai/qurl-integrations.git
cd qurl-integrations

# Install pre-commit hooks (required — CI will catch what these catch, but faster locally)
pip install pre-commit && pre-commit install

# Verify everything works
make check
```

## Your Sandbox

Each integration lives in `apps/{name}/`. You own your app directory:

```
apps/
  slack/          # Slack integration
    cmd/main.go   # Lambda entry point
    internal/     # App-private code — put your logic here
    README.md
  teams/          # Your app here
  discord/        # Your app here
```

**You should primarily be working in `apps/{your-app}/`.**

## Boundaries

| Directory | Who owns it | Can you modify it? |
|-----------|------------|-------------------|
| `apps/{your-app}/` | You | Yes |
| `apps/{other-app}/` | Other contractor | No — open an issue |
| `shared/` | Platform team | PR requires platform review |
| `terraform/` | Platform team | Do not modify |
| `.github/` | Platform team | Do not modify |

Changes to `shared/` trigger tests for ALL apps, so coordinate with other teams.

## Workflow

```bash
# 1. Always start from latest main
git checkout main && git pull

# 2. Create a branch (never commit to main)
git checkout -b feat/slack-thread-replies

# 3. Write code, then verify
make check                    # Full CI parity: fmt + vet + lint + test
make build-slack              # Verify Lambda binary compiles (adjust for your app)

# 4. Push and open a PR
git push -u origin feat/slack-thread-replies
gh pr create --title "feat(slack): add thread replies"
```

## PR Requirements

All of these must pass before merge:

- **PR title** follows [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description`
  - Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`
  - Scopes: `slack`, `teams`, `discord`, `cli`, `zapier`, `shared`
- **Linting** passes (golangci-lint with 28+ linters — see `.golangci.yml`)
- **Tests** pass with `-race`
- **Build** succeeds (Lambda binary)
- **Code review** approved (CODEOWNERS auto-assigns reviewers)
- **No high-severity vulnerabilities** in new dependencies
- **No GPL-3.0 / AGPL-3.0** licensed dependencies

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
5. **Hardcoding secrets** — Use SSM Parameter Store or Secrets Manager. `detect-private-key` hook catches some of this.
6. **Skipping `make check`** — CI runs the same checks. Save yourself the round-trip.

## Getting Help

- **Build/CI issues**: Open an issue tagged `ci`
- **Shared package questions**: Open an issue tagged `shared`
- **Architecture decisions**: Check the app's README first, then ask in your team channel
