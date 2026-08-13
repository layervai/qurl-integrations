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
- `apps/edge-extension/` — Edge MV3 extension (JavaScript); see **Chrome↔Edge lockstep** below
- `apps/teams/` — Node.js (TypeScript ESM, `src/*.ts`); OAuth security core only — no HTTP routes, Teams SDK, or deploy yet
- `apps/zapier/` — placeholder dir, no implementation yet
- `origins/s3-static-connector/` — reusable private S3 static origin image
- `shared/` — Go packages consumed by every Go app; changes here affect all of them
- `e2e/` — TypeScript end-to-end tests (Jest)
- Per-app release tracks via Release Please monorepo mode (`release-please-config.json`); tags are `<component>-v*` except the CLI, which intentionally tags bare `v*` for OSS GoReleaser — see the `.github/workflows/release-please.yml` header before "normalizing" it. A track is earned by cutting a semver version stream that something downstream pins to (Lambda/container deploy, Chrome Web Store, GoReleaser + `install.sh`) — not by having code, nor even by publishing an artifact: `shared/` and `apps/teams/` publish nothing, and `origins/s3-static-connector/` publishes an image tagged only `:main`/`:<sha>`, so none of them have a track. Adding one means editing `release-please-config.json` **and** `.release-please-manifest.json` together — `scripts/check-release-please-sync.sh` fails the build if their keys drift

### Chrome↔Edge lockstep

`apps/edge-extension/` is a platform adaptation of `apps/chrome-extension/`. The two share security-sensitive logic (multipart sanitization, HTTPS-only normalization, permission handling) that **must be kept in sync**. When editing any of the following files in one extension, apply the same change to the counterpart:

| Group | Files | Why it must match |
|-------|-------|-------------------|
| Runtime | `lib/qurl-api.js`, `lib/qurl-compose-format.js`, `lib/qurl-config.js`, `lib/qurl-i18n.js`, `content/gmail-compose.js`, `popup/popup.js`, `popup/popup.css`, `background.js` | Upload sanitizers, HTTPS-only normalization, permission handling, Gmail DOM insertion, per-file size cap; `popup.css` because `popup.js` toggles classes defined only there |
| Build/release | `scripts/build-release.js`, `scripts/bump-version.js`, `scripts/generate-icons.js`, `scripts/package-release.js`, `scripts/package-all.sh` | `build-release.js` re-implements the runtime's https-only and credential-stripping normalization — it can't `require()` the runtime module — and decides the bundled default origin and host permission |
| Tests | `test/*.test.js` (all eleven) | They are the guard on everything above; a browser-specific assertion should be a documented divergence, not a drift |

`scripts/check-extension-lockstep.sh` enforces this on every PR (via the Scripts workflow), so drift fails CI instead of relying on a reviewer noticing. It masks three token classes on both sides and then requires an exact match: store names (`Chrome Web Store` / `Microsoft Edge Add-ons`, masked first because they contain a browser name), the capitalized prose words `Chrome`/`Edge`, and each copy's own app directory (`apps/chrome-extension` / `apps/edge-extension`, which appears in doc comments pointing at sibling files). The lowercase `chrome.*` extension API namespace is spelled the same in both browsers and is deliberately **not** masked, so a real change to an API call still trips the check. When a deliberate divergence is added, document it below **and** update that script.

Accepted blind spot: because browser names are masked symmetrically, a comment naming the **wrong** browser (Edge's copy still saying "Chrome") reads as a match. That is prose-only by construction — every browser name a user actually sees lives in `_locales/en/messages.json`, which is outside this check and covered by `scripts/check-i18n-parity.sh` instead. That check has to be a *separate script* rather than an assertion inside a lockstep test file, because the masking above applies to those files too: a browser name asserted there is erased on both sides before the comparison, so the assertion cannot pin it.

`_locales/en/messages.json` is deliberately **not** in the table — it carries two sanctioned wording deltas (below). `scripts/check-i18n-parity.sh` is what covers it instead, on the same Scripts workflow, and it enforces six rules: identical key sets; byte-identical entries, where an allowlisted key is exempt on its `message` **only** (its `description`, placeholders and everything else must still match); every allowlisted key's `message` actually differing — otherwise the allowlist is a hole, since Edge taking Chrome's `ext_name` is exempt from the equality rule and names no browser, so nothing else would catch it; neither catalog naming the other browser, in any string at any depth; every `__MSG_*__` reference in `manifest.json` resolving in that app's catalog; and each `popup.html`'s two pre-i18n `ext_name` mirrors matching its own catalog. The `message`-only exemption is load-bearing: exempting the whole entry would let a copied message hide behind a diverging `description`, which is unequal and so passes a whole-entry check. Adding a sanctioned delta means editing `SANCTIONED_DELTAS` in that script **and** the list below.

Each app's `test/i18n-coverage.test.js` is a *different* guard and does not overlap: it checks only that keys referenced in that app's own source and markup exist in that app's own catalog and are non-empty. It never inspects message content, never compares the two apps, and does not scan `manifest.json`. A key used by a lockstep file but missing from `messages.json` silently falls back to the hard-coded English literal in `getMessage`'s second argument rather than failing, which is what that test exists to catch.

Intentional differences (do **not** sync these):
- `manifest.json` / `package.json` `version` — separate release-please tracks (`chrome-extension-v*` vs `edge-extension-v*`), so the two versions move independently. This is the *only* manifest delta; Edge Add-ons hosts updates itself, so the Edge manifest carries no `update_url` (that key is the self-hosted Chrome mechanism — don't add it).
- `_locales/en/messages.json` — `ext_name` ("qURL Agent" vs "qURL File Upload for Edge"), and `permission_request_confirm`, which names the host browser showing the prompt ("Chrome will show…" vs "Edge will show…").
- `popup/popup.html` — the two hard-coded mirrors of `ext_name` (the static `<title>` and the header `div`), which is why it is not a lockstep file. Each copy's pair must match its own `ext_name`, or the popup renders one name and swaps to the other once `chrome.i18n` resolves. `check-i18n-parity.sh` enforces that pairing.
- Inside lockstep files, the masked tokens above: `lib/qurl-api.js` names the host browser whose minimum version guarantees `crypto.getRandomValues`; `popup/popup.js` names the browser that will show the permission prompt; `lib/qurl-config.js` points at its own app directory for the packaging `.env`. All prose; the code is identical.
- Store-facing docs and assets: `docs/chrome-web-store-review.md` vs `docs/edge-add-ons-review.md` / `docs/edge-add-ons-submission-guide.md`. The `icons/` are *not* a delta: both apps generate them from the same `icons/logo.png` with `scripts/generate-icons.js`, so all four PNGs are byte-identical and must stay that way.

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

For non-error external or cross-repo contracts mirrored locally (for example qurl-service TTLs or infra log filters), use `TODO(upstream-contract)` so `git grep TODO(upstream-contract)` finds those lockstep sites. This covers third-party platform behavior we depend on but do not control, not just our own services — a Slack event shape the code treats as guaranteed belongs here, because the failure mode is the same: the contract changes upstream and nothing local fails loudly.
