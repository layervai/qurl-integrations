// store — Store backend factory
//
// Every bot module reaches for the data layer via `require('./store')`
// rather than `require('./database')` so swapping backends is a one-
// location change, not a dozen-file sweep. Today the only supported
// backend is `sqlite`; PR 4b adds `ddb`, and PR 4c (greenfield bot
// module, infra side) switches prod's `STORE_TYPE` env var to select
// the new backend.
//
// Selection precedence:
//   1. `process.env.STORE_TYPE` — explicit override, wins.
//   2. Default: `sqlite` — preserves pre-PR-4a behavior for every env
//      that hasn't opted in.
//
// Boot-time invariants:
//   - Unknown `STORE_TYPE` → throw with an explicit list of valid
//     values. A typo like `STORE_TYPE=sqlitte` falls through the
//     switch AND is a silent "fall back to sqlite" footgun; the throw
//     makes the typo fail loud.
//   - Backend missing a method → `assertStoreShape` throws with the
//     offending method name.
// Both throws happen at module-load time so a mis-configured bot
// refuses to start rather than erroring deep in a request path.

const logger = require('../logger');
const { assertStoreShape } = require('./contract');

const VALID_BACKENDS = Object.freeze(['sqlite']);
const BACKEND = process.env.STORE_TYPE || 'sqlite';

if (!VALID_BACKENDS.includes(BACKEND)) {
  throw new Error(`Unknown STORE_TYPE: '${BACKEND}'. Valid backends: ${VALID_BACKENDS.join(', ')}. Set STORE_TYPE to one of these (or leave unset to default to sqlite).`);
}

let store;
switch (BACKEND) {
  case 'sqlite':
    store = require('./sqlite-store');
    break;
  default:
    // Unreachable given VALID_BACKENDS check above; defense-in-depth
    // so a future contributor adding a backend to VALID_BACKENDS but
    // forgetting the switch arm gets a loud "not wired" error
    // instead of a silent null-store.
    throw new Error(`STORE_TYPE '${BACKEND}' is listed in VALID_BACKENDS but has no switch arm in src/store/index.js. Add the require call for the backend implementation.`);
}

// Skip the contract assertion when running under Jest. Jest tests
// routinely `jest.mock('../src/database', () => ({ ...partial... }))`
// to isolate the path under test; those partial mocks are legitimate
// (the test isn't exercising the omitted methods) and should not
// trip a boot-time shape check. `JEST_WORKER_ID` is injected into
// every jest worker process (including `--runInBand`) and is the
// canonical "am I running inside jest?" marker. `typeof jest` is a
// belt-and-suspenders fallback for a hypothetical jest version that
// stops setting the env var — not a defense against a vitest/ava
// migration, which would require `typeof vi !== 'undefined'` or
// similar added at migration time. Keep the explicit escape hatch
// so prod's strict-assert invariant stays intact while tests keep
// their minimal mocks. The separate contract-coverage test
// (`tests/store-contract.test.js`) runs assertStoreShape against a
// complete fixture AND the real default backend AND a
// child-process spawn of the real boot path, so the invariant is
// still enforced at PR time — the prod-boot assertion is a
// runtime belt on top.
const isUnderTestRunner = !!process.env.JEST_WORKER_ID || typeof jest !== 'undefined';
if (!isUnderTestRunner) {
  assertStoreShape(store, BACKEND);
  logger.info('Store backend initialized', {
    backend: BACKEND,
    // `source` flags whether STORE_TYPE was explicitly set vs. fell
    // through to the sqlite default. Useful for confirming flag-day
    // PRs (PR 4c will set STORE_TYPE=ddb on prod) actually landed —
    // "default" in prod logs after the flag-day means the env var
    // didn't propagate to the container.
    source: process.env.STORE_TYPE ? 'env' : 'default',
  });
}

module.exports = store;
