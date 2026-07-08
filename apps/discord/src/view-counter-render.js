// Pure render of the sender's live "👀 N viewed / M pending" counter
// line. Extracted from monitorLinkStatus's buildStatusMsg() closure (and
// re-exported from commands.js, where it originally lived) into its own
// tiny module so BOTH render sites — the in-memory monitor AND the
// cross-replica webhook fast-path (routes/qurl-webhook.js) — import the
// SAME function and can never drift byte-for-byte.
//
// Why a standalone module rather than the commands.js `_test` export:
// `_test` is gated on NODE_ENV !== 'production', so the fast-path can't
// reach it in prod; and importing commands.js into the webhook route
// would pull discord.js + the whole slash-command surface into the HTTP
// receiver's require graph. A leaf module with no deps keeps the
// fast-path's import cheap and the byte-identity guarantee real (one
// definition, one unit test, two callers).
//
// PURE by contract — plain args in, string out, no closure refs and no
// I/O — which is what lets any replica rebuild the confirmation body from
// the persisted render state (baseMsg + last_rendered_count +
// expected_count) without the monitor's in-memory linkStatus map.
//
// `degraded` mirrors viewCounterDegraded: when ANY tracked link is
// missing a qurl_id the counter would mis-attribute, so we render the
// baseMsg alone rather than a partial count. pending floors at 0 so a
// race where viewed transiently exceeds expectedCount (e.g. a webhook
// double-fire landing before expectedCount is bumped by /qurl add)
// never renders a negative "pending".
function renderViewCounter({ baseMsg, viewed, expectedCount, degraded }) {
  if (degraded) return baseMsg;
  const pending = Math.max(0, expectedCount - viewed);
  return `${baseMsg}\n👀 ${viewed} viewed / ${pending} pending`;
}

module.exports = { renderViewCounter };
