// Process-local view-update registry for the sub-second view counter
// (feat #60). The webhook receiver (qurl-webhook.js) publishes view
// events to SQS; the consumer (view-update-consumer.js) drains the
// queue and calls `dispatch(qurl_id, update)` on this registry. Live
// monitorLinkStatus instances register a callback per qurl_id when
// they start and unregister when they stop.
//
// SILENT DROP IS LOAD-BEARING: with N worker replicas, the SQS-Standard
// fan-out gives any one replica a (1/N) probability of receiving the
// message for a given monitor. The dispatch path returns false on
// lookup miss; the consumer deletes the message regardless. The polling
// path in monitorLinkStatus (unchanged by this PR) is the correctness
// primitive — SQS push only shaves latency on the hit half.
//
// GC discipline: the registry holds callbacks that close over monitor
// state (linkStatus Map, baseMsg, etc.). A long-running monitor that
// never unregisters would pin all that closure state until process
// restart. `unregister(qurlId, cb)` MUST be called from
// monitorLinkStatus.stop() and addRecipients(). Mirrors the
// closure-mutable-rebind discipline already in commands.js's
// monitorLinkStatus stop() path.

const logger = require('./logger');

const registry = new Map(); // qurl_id → Set<callback>

function register(qurlId, callback) {
  if (typeof qurlId !== 'string' || !qurlId) {
    throw new Error('view-update-registry.register: qurlId must be a non-empty string');
  }
  if (typeof callback !== 'function') {
    throw new Error('view-update-registry.register: callback must be a function');
  }
  let set = registry.get(qurlId);
  if (!set) {
    set = new Set();
    registry.set(qurlId, set);
  }
  set.add(callback);
}

function unregister(qurlId, callback) {
  const set = registry.get(qurlId);
  if (!set) return;
  set.delete(callback);
  if (set.size === 0) registry.delete(qurlId);
}

// Returns true if at least one callback was invoked; false on silent
// drop (no monitor on this replica). Caller (consumer) deletes the
// message regardless — see SILENT DROP IS LOAD-BEARING above.
//
// Callback signature: (update, qurlId). qurlId is passed in so a
// single shared closure can register against all of a monitor's
// tracked qurl_ids without paying per-id closure-creation cost — a
// 50-recipient send registers the SAME callback against 50 keys,
// each Set holds one element, callback distinguishes inside.
function dispatch(qurlId, update) {
  const set = registry.get(qurlId);
  if (!set || set.size === 0) return false;
  // Snapshot to defensively allow callbacks to unregister themselves
  // mid-iteration without mutating the live set we're iterating.
  const snapshot = Array.from(set);
  for (const cb of snapshot) {
    try {
      cb(update, qurlId);
    } catch (err) {
      logger.error('view-update-registry: callback threw', {
        qurl_id: qurlId,
        error: err.message,
      });
    }
  }
  return true;
}

function _sizeForTest() {
  return registry.size;
}

function _entryCountForTest(qurlId) {
  return (registry.get(qurlId) || new Set()).size;
}

function _resetForTest() {
  registry.clear();
}

module.exports = {
  register,
  unregister,
  dispatch,
  _test: {
    _sizeForTest,
    _entryCountForTest,
    _resetForTest,
  },
};
