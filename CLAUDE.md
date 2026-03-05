# CLAUDE.md — qurl-integrations

## Project Overview

Monorepo for QURL integrations (Slack, Teams, Discord, CLI, Zapier, etc.).
Each integration lives in `apps/{name}/` with independent release tracks.
Shared code lives in `shared/`.

## Repository Rules

1. **Never push directly to main.** Always create a branch and PR.
2. **Conventional Commits required.** Scope to the app: `feat(slack):`, `fix(teams):`, `chore(shared):`.
3. **Path-filtered CI.** Each app has its own workflow. Changes to `shared/` trigger all app tests.
4. **Independent releases.** Release Please monorepo mode. Each app has its own version and CHANGELOG.

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

## Code Conventions

- **Language:** Go (matches nhp and qurl-service repos)
- **Module:** `github.com/layervai/qurl-integrations`
- **App entry points:** `apps/{name}/cmd/main.go`
- **App-private code:** `apps/{name}/internal/`
- **Shared code:** `shared/{package}/` — changes here affect ALL apps
- **Per-app infra:** `apps/{name}/deploy/terraform/`

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
- **Terraform:** Per-app in `apps/{name}/deploy/terraform/`. Isolated blast radius.

## Related Repos

- `layervai/nhp` — NHP server, AC, infrastructure
- `layervai/qurl-service` — QURL API backend
- `layervai/website` — Marketing site + QURL Playground
