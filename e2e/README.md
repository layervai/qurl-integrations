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

Cleanup retries each HTTP 429 once and shares at most ten seconds of retry
waiting across a resource sweep or a message sweep. It honors `Retry-After`
(seconds, fractional seconds, or an HTTP date); a hint above the remaining
budget is reported without an early retry. Missing or invalid hints use a
one-second delay; all retries wait at least 250 ms. Source and message cleanup
do not retry other failures. Child cleanup keeps
its existing three-attempt transient GET/DELETE policy, with at most one
budgeted retry of a 429. Once the budget is spent, further 429s are reported
without a retry. Resource failures
retain their entries and fail the hook after all entries are attempted;
message failures warn and continue. This bounds added waiting, not network
request time; keep suite hook deadlines sized for their fixture count.

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

## File browser ownership receipts

Before running `file-revoke`, build the verifier from the repository root:

```sh
umask 077
receipt_dir=$(mktemp -d)
go build -o "$receipt_dir/verify-ownership" ./e2e/cmd/verify-ownership
export QURL_OWNERSHIP_VERIFIER="$receipt_dir/verify-ownership"
export QURL_OWNERSHIP_RECEIPTS="$receipt_dir/owned-admissions.jsonl"
export QURL_PUBLIC_CONFIG_URL=https://qurl.link.layerv.xyz/
```

The suite checks public issuer configuration and authenticated `/v1/me`
before creating fixtures. Each browser attempt records only public ownership
and a time window. Source deletion does not revoke shared-tunnel children.
Retain the receipts for the operator's service-owner child cleanup and exact
native close/readback; browser shutdown does not prove native CLOSED. Catalog
binding requires independent Control readback by the signed cell public key;
`cell_id` is empty when the optional signed claim is absent. Do not infer a cell
ID from the legacy browser configuration. CI artifacts are accessible to
repository readers for one day. Never put links, private keys or tokens in them.

## Run

Run these package commands from `e2e/` (or use `npm --prefix e2e`); npm then
resolves the optional `.env` file from this package directory.
Use Node 24 as specified by `.nvmrc`; older Node releases can reject the
`--env-file-if-exists` option before Jest starts.

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

Live tests run serially (`maxWorkers: 1` in `jest.config.js`) because they
share Discord channel state. Their config alone loads
`helpers/discord-reporter.js`, so offline unit tests never appear in the
sandbox-health embed. After each live run the reporter posts a per-file
pass/fail rollup to the test channel (missing env or Discord errors warn,
never fail the run).

### Scoped recipient path check

The distinct-recipient file test opens recipient B's view, then recipient A's
view. It keeps A's browser context for three direct requests. The own-path
requests must return HTTP 200 before and after the sibling-path request. The
probe does not follow B's link or issue another knock. All requests share the
viewer's remaining wall-clock deadline. Each request uses at most ten seconds
and has a unique, non-secret `X-Request-ID`.

The router hides an internal authorization refusal with its not-found response
or a connection close. A 404 or reset alone does not prove that the internal
authorizer returned HTTP 403. Resets can also come from an edge or TLS layer.
The receipt marks these observations as pending proof. Unexpected responses,
DNS failures and timeouts fail the probe. A sibling response in the 2xx range
reports an explicit cross-recipient breach with the numeric status only.

One final receipt is written on success or failure. It retains all three request
IDs, known numeric statuses and a fixed failure code. It contains no raw error
or response body. A failed receipt cannot qualify the ledger check.

To complete the [Connector target-path check](https://github.com/layervai/qurl-service/blob/main/docs/runbooks/prod-rollout-ledger/2026-09-05-pr-1474-target-path-cutover.md),
the operator must match the sibling request ID to the exact internal
resource-authorize HTTP 403 access record. Positive own-path requests can hit
the router cache and need not create fresh backend access records. The operator
must also compare the expected path and exact child/public identity with the
matching native admission STATE row using the read-only collector. Pair these
results with the deployed API/router source and shared target-path vectors.
There is no public header or body that identifies a pure scope mismatch.

This probe only allows the reviewed public and LayerV environment domains.
Adding an environment suffix requires source review. The probe alone does not
prove stored scope, production activation, or native CLOSED state.

### Owned target-path input probe (operator opt-in)

`QURL_TARGET_PATH_INPUT_PROBE=1` adds the input-only API1474 matrix to the normal
`tests/file-revoke.test.ts` run. It uses the first successful file case's existing
owned child to read its canonical active Connector parent with the same API key.
It creates no additional upload, identity, browser, or NHP admission. Both API
mint routes receive three valid and three invalid input cases. Six new child
qURLs are revoked through the existing child-only helper; each DELETE requires
204 and each owner read must show revoked. The shared parent is never deleted.

Run through the reviewed normal Sandbox file workflow and shared lock, with the
existing key. Use `--reporters=default`. The wrapper must explicitly pass the
opt-in, retain `e2e/owned-target-path-input-journal.jsonl` on every outcome, and
retain its lock if that file is missing or its status is not
`INPUT_MATRIX_AND_CHILD_CLEANUP_COMPLETE`. This source does not activate the
workflow opt-in or replace that guard. Never enable it before the guard exists.

The append-only JSONL journal is created exclusively with mode0600 and every
snapshot is fsynced before a POST. Earlier known IDs survive an interrupted write.
The receipt gate requires a newline-terminated final complete record; malformed
or truncated records keep the result unresolved.
Returned child IDs are checkpointed before assertions. A lost or ambiguous POST
response is never retried; the pending intent and journal remain for recovery.
Known children are still cleaned up. The journal contains no API key or access
link. Each HTTP call has a ten-second caller abort; the test has a fifteen-minute
outer limit for the fixed request count and existing bounded read/DELETE retries.

This proves only input responses, stored paths, and API child cleanup. Root must
separately bind the deployed API/config, six rejected requests' bounded metric
labels and redacted DEBUG records, and normal flag restoration. It proves no
NHP/native-session cleanup, Connector object lookup, or production result.

### Owned browser native-session cleanup (Sandbox only)

The normal Sandbox workflow can set `QURL_NATIVE_CLEANUP_ENABLED=1` after it
builds the pinned NHP SDK and resolves its public `@layervai/nhp-js-agent/portal`
export into `QURL_NATIVE_CLEANUP_PORTAL_MODULE`. The served, trusted qURL-link
configuration must also enable native cleanup and browser session bootstrap.
Before fixtures, the runner requires the expected built bundle hash and verifies
both the served script name and its actual bytes/SRI.
The ownership verifier keeps its separate source pin.

The Node runner observes the actual relay request/reply before navigation. It
keeps the signed link and original authenticated cleanup handle in memory only.
Before closing the browser it retries all captured handles in paced rounds for
at most five minutes, using the normal Go cleanup budget.
Only `closed` completes a granted session. The existing second one-time arm
requires one strictly authenticated denial with no successful or unknown grant;
its receipt says `no_admission`, never `closed`. Missing, malformed or failed
transport evidence fails that arm instead of passing as an ordinary denial. The private ownership artifact records counts and
owned resource/link identifiers; it never stores keys, packets, or capabilities.
A failed content assertion remains failed. Unresolved cleanup cannot turn a
successful content test green. This does not recover keys from earlier runs.

### URL-target one-use browser check (Sandbox only)

With that verified ownership and native-cleanup configuration, run only
`tests/url-target-one-time.test.ts` with `--reporters=default`. This test needs
the Sandbox API key, not bot/channel credentials, and sends no bot messages.
It adds a nonce to the Sandbox API's public `/health` URL and creates one
one-use URL resource. A real browser must serve exactly `{"status":"healthy"}`
from the isolated `q-` origin. A fresh browser context must then receive an
authenticated denial, and management status must report one use or HTTP 404.

The November 2026 Sandbox observation was a bootstrap redirect through
`/.well-known/qurl/session` to the isolated origin's root, not a redirect to
the target's public hostname. The predicate requires that document response
and its JSON body; an origin change or timeout alone cannot pass.

Mint intent and known IDs are fsynced into the private ownership JSONL before
assertions. There is one POST attempt. Preserve the journal on every outcome;
an ambiguous create or unconfirmed target binding requires owner recovery.
The test revokes only the nonce-bound child/source it verified. Native CLOSED,
authenticated no-admission, and API cleanup remain separate receipts.
