// Named backend alias over `database.js`. Two reasons the separate
// module exists instead of inlining in store/index.js: (a) the shape
// assertion names a concrete backend in its error message
// (`SqliteStore is missing method X`) instead of the generic "Store";
// (b) additional backends (DynamoDB, etc.) slot in symmetrically as
// `ddb-store.js` / etc. rather than asymmetrically against an
// inlined default.
//
// Async wrapping: the Store contract is Promise-returning so
// async-native backends (DynamoDB, etc.) slot in without callers
// needing to know the backend. better-sqlite3 is intentionally sync,
// so this module wraps each exported function to return a Promise.
// Internal database.js calls (e.g. `dbModule.updateStreak` from
// inside `recordContribution`) still use the unwrapped sync
// functions — the wrap only applies to the module's public
// exports consumed through `require('./store')`.
//
// Non-function exports (constants like `BADGE_TYPES`, `BADGE_INFO`)
// pass through unchanged — the contract's `STORE_CONSTANTS` list
// carries non-callable values that shouldn't be Promise-wrapped.
//
// Why programmatic wrap (vs. method-by-method enumeration): a
// one-line-per-method enumeration would drift out of sync with
// `database.js`'s exports over time. The programmatic form auto-
// picks up new methods and the contract assertion (STORE_METHODS
// list) remains the authoritative source for "what MUST exist."

const dbModule = require('../database');

const wrapped = {};
for (const key of Object.keys(dbModule)) {
  const value = dbModule[key];
  if (typeof value === 'function') {
    // `.apply(dbModule, args)` preserves `this` for methods that
    // reference it. Today database.js exports method-shorthand
    // properties on the `dbModule` object literal, so `this` isn't
    // referenced internally — but a future hand-rolled `function()`
    // export would break under a naive `(...args) => value(...args)`
    // wrap. Cheap defense.
    wrapped[key] = async (...args) => dbModule[key].apply(dbModule, args);
  } else {
    wrapped[key] = value;
  }
}

module.exports = wrapped;
