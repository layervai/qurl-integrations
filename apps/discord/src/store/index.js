// store — Store backend factory
//
// Every bot module reaches for the data layer via `require('./store')`
// rather than `require('./database')` so swapping backends is a one-
// location change, not a dozen-file sweep. Today the only supported
// backend is `sqlite`; additional backends (e.g. DynamoDB) slot in
// beside it by adding an entry to `VALID_BACKENDS` + the switch arm
// below + a matching backend module that passes `assertStoreShape`.
//
// Selection precedence:
//   1. `process.env.STORE_TYPE` — explicit override, wins.
//   2. Default: `sqlite` — preserves pre-indirection behavior for any
//      env that hasn't opted in.
//
// Boot-time invariants (all throw at module-load time so a mis-
// configured bot refuses to start rather than erroring deep in a
// request path):
//   - Unset OR empty-string STORE_TYPE → default to `sqlite`
//     silently. `STORE_TYPE=` (empty) is rare but happens under
//     buggy container templating; treating it as "unset" matches
//     operator intent. An explicit whitespace-only value is
//     treated the same.
//   - Non-empty unknown STORE_TYPE → throw, naming the bad value +
//     the valid options. A typo like `STORE_TYPE=sqlitte` fails
//     loud rather than falling back.
//   - Backend missing a method → `assertStoreShape` throws with the
//     offending method name (non-Jest boot only — see comment on
//     the assertion call below for the Jest escape hatch).

const logger = require('../logger');
const { assertStoreShape } = require('./contract');

const VALID_BACKENDS = Object.freeze(['sqlite']);
const rawStoreType = process.env.STORE_TYPE;
// Treat unset, empty, and whitespace-only STORE_TYPE as "not
// configured" — falls back to the sqlite default. Any non-empty
// value is taken literally and must match VALID_BACKENDS.
const BACKEND = (rawStoreType && rawStoreType.trim()) || 'sqlite';

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
// canonical "am I running inside jest?" marker. The separate
// contract-coverage test (`tests/store-contract.test.js`) runs
// assertStoreShape against a complete fixture AND the real default
// backend AND a child-process spawn of the real boot path, so the
// invariant is still enforced at PR time — the prod-boot assertion
// is a runtime belt on top.
if (!process.env.JEST_WORKER_ID) {
  assertStoreShape(store, BACKEND);
  logger.info('Store backend initialized', {
    backend: BACKEND,
    // `source` flags whether STORE_TYPE was explicitly set vs. fell
    // through to the sqlite default. Useful for confirming a
    // backend-change rollout actually landed — "default" in prod
    // logs after an env-var switch means the env var didn't
    // propagate to the container.
    source: rawStoreType ? 'env' : 'default',
  });
}

module.exports = store;
