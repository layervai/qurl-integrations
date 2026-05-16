// Outbound control-channel client for Pillar 3 push-handoff. The
// active replica calls `pushHandoff` from its SIGTERM handler to
// signal a chosen peer "you're up." The peer's gateway-control-
// channel server (see gateway-control-channel.js) verifies the HMAC,
// runs `transferLock` (atomic active→standby), connects its WS to
// Discord, and ACKs.
//
// Spec reference: docs/zero-downtime-design.md §Pillar 3
// "SIGTERM handoff sequence" (lines ~519-537) and "Standby on POST
// /control/yours" (lines ~562-618).
//
// ── Timeout ──
// Default 200 ms. The active is shutting down; it cannot block on
// SIGTERM's ECS-imposed deadline (10 s) waiting for an unresponsive
// peer. If the standby doesn't ACK in 200 ms, the active gives up
// and exits anyway — the standby's connection watchdog will catch
// "lock held but WS disconnected" within ~1 s of the next watchdog
// tick and bring up the gateway from the cold-fallback path. The
// ~7 s cold-floor applies but is still better than a stuck active
// preventing handoff.
//
// ── Body shape ──
// `{active_instance_id, peer_instance_id, expected_version, ts, nonce}`
// signed with HMAC. `ts` and `nonce` are added by this module
// (callers don't need to think about them); `expected_version` is
// the POST-transfer version returned by the active's
// `transferLock` call (DDB version after the CAS-bump). The
// standby seeds its own `gateway-lock` cursor with this value
// directly via `adoptLockFromHandoff(expected_version)` — no
// arithmetic on the receive side. Sender contract: pass the
// `version` field of a `{transferred: true}` transferLock result.
//
// ── Return contract ──
// Returns a result object — never throws. Distinguishes:
//   { ok: true,  status: 200 }                        — peer ACKed; we're done
//   { ok: false, reason: 'invalid_arg', arg }         — argument validation rejected (also caught at the heartbeat-side validator; this is defense-in-depth)
//   { ok: false, reason: 'timeout' }                  — peer didn't reply in time
//   { ok: false, reason: 'http_error', error }        — connection error
//   { ok: false, reason: 'rejected', status, body }   — peer returned non-2xx
//
// Callers (the leader's SIGTERM path) ignore the difference between
// failure modes — any non-ok result means "exit anyway." The
// distinction is for observability only.

const http = require('node:http');
const net = require('node:net');

const { wrapEnvelope } = require('./gateway-hmac');

const DEFAULT_TIMEOUT_MS = 200;
// Symmetric to the server's bodyByteCap (gateway-control-channel.js
// DEFAULT_BODY_BYTE_CAP = 8 KB). A correctly-behaving peer will reply
// with a small ACK or a JSON error envelope — kilobytes at most.
// A misrouted POST hitting an HTML error page (or a hostile in-VPC
// actor) could otherwise return multi-MB and OOM the active during
// the SIGTERM critical path. Buffer cap is the bytes we'll keep; on
// exceeded the request is destroyed and we settle with `http_error`.
const DEFAULT_RESPONSE_BYTE_CAP = 8 * 1024;

function createControlClient({
  hmac,
  logger,
  timeoutMs = DEFAULT_TIMEOUT_MS,
  responseByteCap = DEFAULT_RESPONSE_BYTE_CAP,
  // Injected for tests. Production uses node:http.
  httpRequest = http.request,
} = {}) {
  if (!hmac || typeof hmac.sign !== 'function' || typeof hmac.generateNonce !== 'function') {
    throw new Error('createControlClient: hmac with sign() and generateNonce() is required');
  }
  if (!logger) throw new Error('createControlClient: logger is required');

  async function pushHandoff({
    peerIp,
    peerPort,
    peerInstanceId,
    selfInstanceId,
    expectedVersion,
  }) {
    // Validators return a result object rather than throw so the
    // "never throws" contract documented in the module header holds
    // unconditionally. Defense-in-depth: if a heartbeat row is ever
    // corrupted or mis-written to carry a hostname (or the literal
    // "undefined" from env-stringification), we don't want to do DNS
    // resolution + POST to wherever it resolves. The heartbeat-side
    // validator (gateway-peer-heartbeat.js) DOES throw (it's a write-
    // time config check, not a hot-path runtime check) — this side
    // mirrors the same shape but at runtime returns a result so the
    // SIGTERM caller stays exception-free.
    const invalidArg = (
      (typeof peerIp !== 'string' || net.isIP(peerIp) === 0) ? 'peerIp'
      : (!Number.isInteger(peerPort) || peerPort <= 0 || peerPort > 65535) ? 'peerPort'
      : (typeof peerInstanceId !== 'string' || peerInstanceId.length === 0) ? 'peerInstanceId'
      : (typeof selfInstanceId !== 'string' || selfInstanceId.length === 0) ? 'selfInstanceId'
      : (!Number.isInteger(expectedVersion) || expectedVersion <= 0) ? 'expectedVersion'
      : null
    );
    if (invalidArg) {
      logger.warn('control-client: invalid pushHandoff arg', {
        arg: invalidArg, peerInstanceId: typeof peerInstanceId === 'string' ? peerInstanceId : null,
      });
      return { ok: false, reason: 'invalid_arg', arg: invalidArg };
    }

    const payload = {
      active_instance_id: selfInstanceId,
      peer_instance_id: peerInstanceId,
      expected_version: expectedVersion,
      ts: Date.now(),
      nonce: hmac.generateNonce(),
    };
    const signed = hmac.sign(payload);
    const wire = wrapEnvelope(signed);

    return new Promise((resolve) => {
      let settled = false;
      function settle(result) {
        if (settled) return;
        settled = true;
        resolve(result);
      }

      // Wrap httpRequest + req.end(wire) in try/catch so a future
      // Node version tightening validation on hostname/port (or
      // any other synchronous throw from the http module) doesn't
      // break the "pushHandoff never throws" contract. Inputs are
      // pre-validated above, so this is belt-and-braces — but the
      // contract is load-bearing for the SIGTERM caller, which
      // cannot handle exceptions cleanly.
      try {
        const req = httpRequest({
          hostname: peerIp,
          port: peerPort,
          path: '/control/yours',
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'Content-Length': wire.length,
          },
          // Per-call timeout — covers both connect and response phases.
          timeout: timeoutMs,
        }, (res) => {
          const chunks = [];
          let bytesRead = 0;
          let bodyCapExceeded = false;
          res.on('data', (chunk) => {
            if (bodyCapExceeded) return;
            bytesRead += chunk.length;
            if (bytesRead > responseByteCap) {
              // A misrouted POST hitting an HTML error page or a
              // hostile in-VPC actor could otherwise return multi-MB
              // and OOM the active during SIGTERM. Destroy the
              // response so we settle now instead of buffering
              // forever; the destroy emits an error which the 'error'
              // handler below maps to http_error. Mark a flag so
              // subsequent 'data' chunks (already in the pipeline)
              // are dropped without growing the buffer.
              bodyCapExceeded = true;
              logger.warn('control-client: response body exceeded cap', {
                peerInstanceId, cap: responseByteCap,
              });
              settle({ ok: false, reason: 'http_error', error: 'response_body_too_large' });
              res.destroy();
              return;
            }
            chunks.push(chunk);
          });
          res.on('end', () => {
            if (bodyCapExceeded) return;
            const body = Buffer.concat(chunks).toString('utf8');
            if (res.statusCode >= 200 && res.statusCode < 300) {
              logger.info('control-client: handoff ACKed', {
                peerInstanceId, status: res.statusCode,
              });
              settle({ ok: true, status: res.statusCode });
            } else {
              // Cap the logged body so an unexpectedly large peer
              // response (e.g., a stray HTML error page from a
              // misrouted request) doesn't flood the log line. The
              // returned `body` field is already cap-bounded by the
              // streaming guard above.
              logger.warn('control-client: handoff rejected by peer', {
                peerInstanceId, status: res.statusCode, body: body.slice(0, 512),
              });
              settle({ ok: false, reason: 'rejected', status: res.statusCode, body });
            }
          });
          res.on('error', (err) => {
            logger.warn('control-client: response error', {
              peerInstanceId, error: err.message,
            });
            settle({ ok: false, reason: 'http_error', error: err.message });
          });
          // Peer crashes mid-response (after headers, before end).
          // The deprecated `res.on('aborted')` was replaced in Node 17
          // by `res.on('close')` combined with checking
          // `res.destroyed` post-close. Matches the receive-side
          // idiom in gateway-control-channel.js. Without this
          // handler we'd wait the full timeout before settling.
          res.on('close', () => {
            if (res.destroyed && res.statusCode !== undefined) {
              // Aborted after headers but before 'end' fired. The
              // 'end' arm already settled if the body completed
              // normally, so settle is idempotent here.
              logger.warn('control-client: response aborted', { peerInstanceId });
              settle({ ok: false, reason: 'http_error', error: 'response_aborted' });
            }
          });
        });

        req.on('timeout', () => {
          // `timeout` event fires but doesn't close the socket. We
          // MUST settle BEFORE destroy: `req.destroy(err)` can emit
          // 'error' synchronously in some code paths, and the 'error'
          // handler below would otherwise race ahead and settle with
          // reason:'http_error' instead of 'timeout'. Settle is
          // idempotent, so the destroy-induced 'error' becomes a
          // no-op after this.
          logger.warn('control-client: handoff timed out', {
            peerInstanceId, timeoutMs,
          });
          settle({ ok: false, reason: 'timeout' });
          req.destroy(new Error('handoff_timeout'));
        });

        req.on('error', (err) => {
          // After `destroy(err)` for the timeout path, 'error' will
          // fire — settle is already done, so this is a no-op. Other
          // error paths (connection refused, DNS fail, etc.) settle
          // here.
          logger.warn('control-client: request error', {
            peerInstanceId, error: err.message,
          });
          settle({ ok: false, reason: 'http_error', error: err.message });
        });

        req.end(wire);
      } catch (err) {
        logger.warn('control-client: synchronous http.request failure', {
          peerInstanceId, error: err.message,
        });
        settle({ ok: false, reason: 'http_error', error: err.message });
      }
    });
  }

  return { pushHandoff };
}

module.exports = {
  createControlClient,
  DEFAULT_TIMEOUT_MS,
};
