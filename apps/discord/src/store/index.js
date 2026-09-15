// TODO(upstream-contract): remove this check after qurl-integrations-infra
// stops emitting STORE_TYPE.
const configured = process.env.STORE_TYPE?.trim();
if (configured && configured !== 'ddb') {
  throw new Error(`Unknown STORE_TYPE: '${configured}'. The only valid value is 'ddb'.`);
}

module.exports = require('./ddb-store');
