# qURL E2E tests

End-to-end tests for the qURL™ integrations. They drive **live** systems:
the qURL API (mint / access / revoke), the Discord API with a real test
bot + guild, and — optionally — deployed bot HTTP endpoints. TypeScript,
CommonJS, Jest + ts-jest.

> **Warning:** these tests mint real qURL resources and post real Discord
> messages. Point them at test/staging endpoints — never production.
> Suites nonce-tag the target URLs they mint and revoke/delete their
> resources in `afterAll` (see `helpers/cleanup.ts`), so runs clean up
> after themselves.

## Setup

```sh
cp .env.example .env   # then fill in real values
```

Required variables (validated by `helpers/env.ts`; the suite fails fast
if any is missing): `BOT_TOKEN`, `BOT_CLIENT_ID`, `QURL_API_KEY`,
`UPLOAD_API_URL`, `MINT_API_URL`, `GUILD_ID`, `CHANNEL_ID`.

The test bot's application must have the **Server Members privileged
intent enabled** in the Discord developer portal — the guild-members
tests assert it hard, because the production bot load-bears that intent
for `/qurl send` recipient resolution (`apps/discord/src/discord.js`).

Optional variables gate extra suites, which skip themselves
(`describe.skip`) when unset:

- `BOT_HTTP_URL` — enables `qurl-oauth-setup.smoke.test.ts` (deployed
  Discord bot HTTP server).
- `SLACK_BOT_BASE_URL` — enables `slack-liveness.smoke.test.ts`; add
  `SLACK_SIGNING_SECRET` for its signed cases.
- `MAP_COMMAND_ENABLED` — must be explicitly `"true"` or `"false"` when
  running `discord-commands.smoke.test.ts`; that suite fails fast
  otherwise (see the comment there for why).

## Run

```sh
npm ci
npm test                        # unit tests, then live E2E (needs .env)
npm run test:unit               # offline helper tests only (no .env)
npm run test:e2e -- smoke.test  # one live file
npm run test:e2e -- -t 'mint 10 links' # one self-contained live test by name
npx tsc --noEmit                # typecheck only (no .env needed)
```

Everything — including `playwright`, which the tunnel-view tests
load-bear at runtime — lives in `devDependencies` (nothing here is
published). Never install this package with `--omit=dev` /
`--production` in CI: the install goes green but the suite can't run.

Caveat on `-t`: the lifecycle suites (smoke, link-lifecycle) are
deliberately order-dependent (mint → access → re-access → revoke), and
their mid-flow tests carry explicit `toBeDefined` dependency guards. So
running one of those tests by name fails its guard **by design** — run
the whole file instead.

Management-state assertions use the real resource-centric API contract:
`GET /v1/qurls/{id}` accepts either an opaque public `resource_id` or a
`q_…` display ID. Resource lifecycle comes from the returned resource;
per-token `use_count` / status comes from its matching `qurls[]` summary.
Revocation is soft: the resource remains readable with `status=revoked`.
Do not derive public resource-ID syntax from the internal `r_…` routing
labels that can still appear in `qurl.site` hostnames.

Live tests run serially (`maxWorkers: 1` in `jest.config.ts`) because they
share Discord channel state. Their config alone loads
`helpers/discord-reporter.js`, so offline unit tests never appear in the
sandbox-health embed. After each live run the reporter posts a per-file
pass/fail rollup to the test channel (missing env or Discord errors warn,
never fail the run).
