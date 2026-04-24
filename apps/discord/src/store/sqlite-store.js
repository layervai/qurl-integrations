// store/sqlite-store — Store backend for the legacy SQLite-on-EFS path.
//
// Thin pass-through over `src/database.js`. Every method this exports is
// the same function object database.js owns — no wrapping, no behavior
// change. The indirection exists so PR 4b can drop in a DdbStore beside
// this one (implementing the same `STORE_METHODS` contract) without
// touching any caller.
//
// Why the separate module (vs. just re-exporting database.js from
// store/index.js): the distinction matters for the contract assertion
// at boot — the shape check validates a concrete backend object, and
// having `sqlite-store.js` as a named backend makes the failure
// message clear ("SqliteStore is missing method X") rather than vague
// ("Store is missing method X").
//
// Lifecycle: `database.js` starts a handful of `setInterval` cleanup
// timers at module load. They `.unref()` themselves so they don't keep
// the process alive past an intentional shutdown. `close()` stops them
// alongside closing the SQLite handle.
//
// Async parity: every method is synchronous today (better-sqlite3 is
// intentionally sync for throughput + transaction correctness). The
// Store contract permits sync or async; PR 4b flips to async when the
// DDB backend lands.

const dbModule = require('../database');

module.exports = dbModule;
