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
npm test                        # full suite (needs .env)
npx jest smoke.test             # one file
npx jest -t 'second access'     # one test by name
npx tsc --noEmit                # typecheck only (no .env needed)
```

Tests run serially (`maxWorkers: 1` in `jest.config.ts`) because they
share Discord channel state. After each run, `helpers/discord-reporter.js`
posts a per-file pass/fail rollup embed to the test channel (missing env
or Discord errors make it warn, never fail the run).
