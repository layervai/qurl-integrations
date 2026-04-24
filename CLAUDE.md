# CLAUDE.md — qurl-integrations

## CRITICAL RULES - NEVER VIOLATE

> **NEVER push directly to `main` branch.** All changes MUST go through a Pull Request, no exceptions. Create a branch, open a PR, and let CI run.

> **All commits must be GPG/SSH signed.** Unsigned commits will be rejected by GitHub branch protection rules.

> **All code must pass `golangci-lint` with zero issues.** The linter config is strict by design — fix the code, don't weaken the rules.

## Code Change Workflow

1. `git checkout main && git pull origin main`
2. `git checkout -b <type>/<short-description>`
3. Make changes
4. `make check` (runs fmt, vet, lint, tests)
5. `git push -u origin <branch>`
6. `gh pr create --title "<type>(scope): description" --body "..."`
7. Address code review feedback, then push fixes

## Project Overview

Go monorepo for QURL integrations (Slack, Teams, Discord, CLI, Zapier, etc.).
Each integration lives in `apps/{name}/` with independent release tracks.
Shared code lives in `shared/`.

SDKs live in separate repos: [qurl-python](https://github.com/layervai/qurl-python), [qurl-typescript](https://github.com/layervai/qurl-typescript).

## Commit Format

```
<type>(<scope>): <description>

type:  feat | fix | chore | docs | test | refactor | ci
scope: slack | teams | discord | cli | zapier | shared
```

Examples:
- `feat(slack): add slash command handler`
- `fix(shared): retry on 429 in API client`
- `ci(slack): add deploy step to workflow`

> Keep this scope list aligned with the Component dropdown in
> `.github/ISSUE_TEMPLATE/bug_report.yml`. Convention only (not CI-
> enforced in this repo); add a new scope to both places in the same
> PR. The dropdown's `other` option is a reporter-UX escape hatch —
> do NOT add it here (it's not a valid commit scope).

## Code Conventions

- **Language:** Go
- **Module:** `github.com/layervai/qurl-integrations`
- **App entry points:** `apps/{name}/cmd/main.go`
- **App-private code:** `apps/{name}/internal/`
- **Shared code:** `shared/{package}/` — changes here affect ALL apps

## Linting

This repo uses `golangci-lint` v2.10.1 with 28+ linters enabled (see `.golangci.yml`). Key rules:

- All errors must be checked (`errcheck`)
- Use `errors.Is`/`errors.As`, not type assertions (`errorlint`)
- No naked returns (`nakedret`)
- Max cognitive complexity 30 / cyclomatic complexity 20
- No code duplication over 150 tokens (`dupl`)
- Repeated strings (3+ occurrences) must be constants (`goconst`)
- All exported types/functions need comments (`revive`)
- `nolint` directives require explanation and specific linter name (`nolintlint`)
- Security scanning via `gosec`
- Performance-aware formatting (`perfsprint`)

## Pre-commit Hooks

```bash
# Install (one-time)
pip install pre-commit && pre-commit install

# Run manually
pre-commit run --all-files
```

Hooks: trailing whitespace, EOF fixer, YAML/JSON validation, large file check, private key detection, merge conflict check, `gofmt`, `go mod tidy`, `golangci-lint`.

## Common Commands

```bash
make check          # Full CI parity: fmt + vet + lint + test
make lint           # golangci-lint only
make test           # go test (no race)
make test-race      # go test -race
make build-slack    # Build Slack Lambda binary
make security       # govulncheck
make fmt            # gofmt + goimports
```

## Testing

```bash
go test ./...                      # All tests
go test ./apps/slack/...           # Single app
go test ./shared/...               # Shared only
go test -count=1 ./...             # Skip cache
```

## Build

Lambda apps use `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`:
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bootstrap ./apps/slack/cmd/
```

## Key Architecture Decisions

- **Auth:** Start with workspace API keys (qurl-api-keys table exists). Per-user OAuth later.
- **Runtime (Slack):** AWS Lambda behind API Gateway. Event-driven, scales to zero.
- **Shared client:** `shared/client/` wraps the QURL API. Not a standalone module yet.
- **Release:** Release Please monorepo mode with per-app version tracks.
