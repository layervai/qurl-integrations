const { claimFlowWarning } = require('./flow-state');

const recent = new Map();
let active = 0;

// The bounded cache saves reads; the row claim preserves once-per-lifecycle
// logging across eviction, multiple workers and restarts. A crash after the
// claim can omit a warning, but cannot log it twice.
function warnFlowCorruptionOnce({ flow_id, sendNonce, kind, value }, emit) {
  if (typeof flow_id !== 'string' || !flow_id) return;
  const key = JSON.stringify([flow_id, sendNonce, kind]);
  if (recent.has(key)) {
    const entry = recent.get(key);
    recent.delete(key);
    recent.set(key, entry);
    return;
  }
  if (active >= 4) return; // Best-effort diagnostics must not queue work behind user requests.
  const entry = {};
  recent.set(key, entry);
  if (recent.size > 64) recent.delete(recent.keys().next().value);
  const forget = () => { if (recent.get(key) === entry) recent.delete(key); };
  active += 1;
  void Promise.resolve().then(() => claimFlowWarning(flow_id, sendNonce, kind, value)).then(result => {
    if (result === 'claimed') emit();
    // Legacy rows have no nonce: do not cache across a later legacy lifecycle.
    if (result === 'retry' || typeof sendNonce !== 'string' || !sendNonce) forget();
  }).catch(forget).finally(() => { active -= 1; });
}

module.exports = { warnFlowCorruptionOnce };
