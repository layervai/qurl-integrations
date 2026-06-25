# CLAUDE.md — qurl-integrations

## Constraints

- **Never push directly to `main`.** Branch protection enforces PRs.
- **All commits must be GPG/SSH signed.** Unsigned commits are rejected.
- **`golangci-lint` must pass clean.** Config is strict by design (see `.golangci.yml`); fix the code, not the rules.
- **GitHub Actions refs must be pinned.** Follow the source-of-truth policy in
  [CONTRIBUTING.md](CONTRIBUTING.md#pr-requirements): full commit SHA, exact
  upstream version comment, no `docker://`, and human tag/SHA verification.

## Layout

Polyglot monorepo for qURL integrations. SDKs live in separate repos: [qurl-python](https://github.com/layervai/qurl-python), [qurl-typescript](https://github.com/layervai/qurl-typescript).

- `apps/slack/`, `apps/cli/` — Go (`cmd/` + `internal/`)
- `apps/discord/` — Node.js (CommonJS, `src/*.js`)
- `apps/chrome-extension/` — Chrome MV3 extension (JavaScript)
- `apps/edge-extension/` — Edge MV3 extension (JavaScript)
- `apps/teams/`, `apps/zapier/` — placeholder dirs, no implementation yet
- `origins/s3-static-connector/` — reusable private S3 static origin image
- `shared/` — Go packages consumed by every Go app; changes here affect all of them
- `e2e/` — TypeScript end-to-end tests (Jest)
- Per-app release tracks via Release Please monorepo mode (`release-please-config.json`)

## Commit format

```
<type>(<scope>): <description>

type:  feat | fix | docs | style | refactor | perf | test | build | ci | chore | revert
scope: slack | teams | discord | cli | zapier | chrome-extension | edge-extension | origins | shared | ci
```

> Keep this type list aligned with CONTRIBUTING.md and `.github/workflows/pr-title.yml`'s `types:` block.
>
> - When adding a new type: touch CLAUDE.md, CONTRIBUTING.md, and
>   `.github/workflows/pr-title.yml`.
>
> Keep this scope list aligned with the Component dropdown in `.github/ISSUE_TEMPLATE/bug_report.yml`. `.github/workflows/pr-title.yml`'s `scopes:` block is the CI-enforced superset:
>
> - It currently lists two extra scopes (`infra`, `deps`) that aren't in this list or the issue template — tracked in #463 for sync.
> - `requireScope: false`, so a scope is optional in PR titles; but when one is present, `amannn/action-semantic-pull-request` validates it against the workflow's list.
> - When adding a new scope: touch CLAUDE.md, CONTRIBUTING.md, and
>   `bug_report.yml`, plus `pr-title.yml` if it isn't already in its superset.
> - The dropdown's `other` option is a reporter-UX escape hatch — do NOT add it here (not a valid commit scope).

## Brand spelling

The product brand is **`qURL`** (case-sensitive: lowercase `q`, uppercase `URL`). Use `qURL` in user-visible prose, log/error messages, doc comments, README content, and anything a human reads.

**Trademark:** mark the first singular mention in a human-readable document (README intro, package description, etc.) as `qURL™`, then use plain `qURL` for the rest. Don't put `™` on a heading or on the plural `qURLs`. This matches the SDKs, the MCP server, and the root README.

**Never "firewall":** LayerV is not a firewall company — don't describe qURL's mechanism in firewall terms. Resolving a token's NHP knock **grants network access** to the caller's IP; use "grant network access" / "grants access", never "open(s) firewall". (Applies to prose, doc comments, and user-visible strings, not wire-protocol identifiers.)

The following stay literal — don't "finish" the rebrand:
- Go identifiers: types/structs/funcs (`QURL`, `QURLClient`, `Qurl`, `CreateQurlRequest`, `QURLLink`)
- Env vars (`QURL_API_KEY`, `QURL_ENDPOINT`, `QURL_BASE_URL`, `QURL_TIMEOUT`)
- DDB table/column names and JSON keys (`qurl_sends`, `qurl_send_configs`, `qurl_link`, `qurl_id`)
- Wire-protocol HTTP headers (`QURL-Signature`, `X-QURL-*`) and User-Agent strings (`qurl-cli/...`, `qurl-go-client/...`, `qurl-discord-bot/1.0`)
- Slash command names (`/qurl file`, `/qurl map`, `/qurl help`) and the CLI binary `qurl`
- OAuth scope identifiers (`qurl:read`, `qurl:write`, `qurl:resolve`)
- Domain literals (`qurl.link`, `qurl.site`)
- Man-page section titles (`QURL(1)` — system-reference convention)

When upstream qurl-service rebrands its API error strings, the test fixtures in this repo that mirror them (`"QURL not found"`, `"QURL API error (...)"`, `"token limit per QURL reached"` etc.) need to update in lockstep — `git grep TODO(upstream-rebrand)` finds the doc-comment markers.

For non-error external or cross-repo contracts mirrored locally (for example qurl-service TTLs or infra log filters), use `TODO(upstream-contract)` so `git grep TODO(upstream-contract)` finds those lockstep sites.
