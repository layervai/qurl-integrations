// Named backend alias over `database.js`. Two reasons the separate
// module exists instead of inlining in store/index.js: (a) the shape
// assertion names a concrete backend in its error message
// (`SqliteStore is missing method X`) instead of the generic "Store";
// (b) additional backends (DynamoDB, etc.) slot in symmetrically as
// `ddb-store.js` / etc. rather than asymmetrically against an
// inlined default.

module.exports = require('../database');
