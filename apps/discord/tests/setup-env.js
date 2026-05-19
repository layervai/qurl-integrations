// Global Jest setup — runs ONCE before any test file loads, via
// jest.config.js `setupFiles`. Sets process.env vars that source
// modules read at require-time so their fail-fast module-load guards
// don't throw mid-import in tests.
//
// Why module-level setup (not per-file): once commands.js started
// requiring flow-state in PR 5, every test file that imports
// commands.js (or any of its peers) indirectly loads flow-state.js,
// which throws at the top level when `DDB_TABLE_PREFIX` or
// `AWS_REGION` are absent. Forcing each test file to set those two
// vars by hand would be ~9 copy-pasted lines per file plus a real
// risk of new test files forgetting. A single setupFiles entry
// instead.
//
// These vars stay set for the whole worker process. Individual test
// files can still override via `process.env.X = ...` if they need a
// different value (and re-require the module to re-read it). Tests
// that mock flow-state entirely via `jest.mock('../src/flow-state',
// ...)` don't observe these vars at all — the mock replaces the real
// module before its top-level code runs.

// Test prefix has the same shape as the real one
// (e.g. `qurl-bot-discord-sandbox-`) — must end with `-` per the
// flow-state guard. Value is a sentinel that won't collide with any
// real environment; if a test accidentally lets a real DDB call
// through, the error message names this prefix so the breakage is
// obvious.
process.env.DDB_TABLE_PREFIX = process.env.DDB_TABLE_PREFIX || 'jest-test-';
process.env.AWS_REGION = process.env.AWS_REGION || 'us-east-1';

// Spawn-test caveat: `tests/store-contract.test.js`'s `spawnStoreBoot`
// helper pins DDB_TABLE_PREFIX + AWS_REGION explicitly in the child
// env (insulating those specific values from sentinel changes here),
// but every OTHER module-load guard a child inherits (e.g.
// KEY_ENCRYPTION_KEY from individual specs that set it before
// `require('../src/store')`) flows through `{...process.env}`. If
// you ever trim this file's sentinel set, audit the spawn tests
// first — a silent regression where the child suddenly fails to
// boot would show up as a `result.status !== 0` mismatch with no
// hint at the culprit env var.
