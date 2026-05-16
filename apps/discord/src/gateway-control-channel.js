// In-VPC control channel for Pillar 3 push-handoff. Binds an HTTP
// listener on the task's ENI (default 0.0.0.0 in awsvpc mode) and
// answers a single endpoint:
//
//   POST /control/yours    — peer is handing the lock to us; verify
//                            HMAC, validate routing, invoke onHandoff,
//                            ACK.
//
// Spec reference: docs/zero-downtime-design.md §Pillar 3 "Control-
// channel auth: shared HMAC secret" (lines ~436-472) and "Standby on
// POST /control/yours" (lines ~562-618).
//
// ── Wire envelope ──
// HTTP body = JSON.stringify({
//   body: "<json-string of the signed payload>",
//   signature: "<hex sha256>",
// })
//
// The INNER `body` string is the exact UTF-8 bytes that the sender
// signed (sender: bodyBytes = Buffer.from(JSON.stringify(payload),
// 'utf8')). Wrapping it as a string inside an outer JSON envelope
// preserves the inner bytes verbatim across JSON.parse + .toString:
// JSON-stringified strings round-trip exactly. The alternative —
// putting `body` as an object — would force the receiver to re-
// stringify it, canonicalizing key order, which would break HMAC
// verify on a body whose original stringification used a different
// key order.
//
// ── Verification order ──
// We hash-verify BEFORE we look at any payload field. Reasons:
//   1. DoS: parsing a giant unsigned body before verify lets an
//      attacker burn CPU + memory. The 8 KB cap bounds memory, but
//      verify-first means we don't even JSON.parse on bad inputs.
//   2. nonce-burn safety: gateway-hmac burns the nonce when verify
//      returns ok=true. Burning a nonce on a malformed-payload body
//      (peer_instance_id mismatch, etc.) is OK — legitimate senders
//      don't send mis-addressed bodies, and a captured-then-replayed
//      body addressed to a different peer hits a different replica's
//      LRU anyway.
//
// ── Routing checks (after verify) ──
//   - `payload.peer_instance_id === self.instance_id` — the body is
//     addressed to THIS standby. Defends against intra-cluster cross-
//     shard replay at sharding inflection (PR 15 / 16-ish).
//   - `isKnownPeer(payload.active_instance_id)` — the sender is a
//     replica we've seen in the peer-heartbeat table within the
//     freshness window. Defends against handoff from a stale stopped
//     task whose container exited but whose body was queued upstream.
//
// ── Body cap ──
// 8 KB. Real handoff payload is ~250 B (4 strings + a number + nonce
// + ts). 8 KB is 30× over-provisioned — gives room for future fields
// without re-litigating the cap, but small enough to bound the
// worst-case memory of an attacker spamming maximally-large bodies.
//
// When the cap is hit, `req.pause()` stops further data events and
// the handler returns a 413 with `Connection: close`. Server-level
// `requestTimeout` (default 5 s) is the backstop for a misbehaving
// client that ignores the close and keeps the socket open — the
// paused stream stays in memory at most 5 s before the timeout
// reaps it.
//
// ── 401 reason leak — intentional tradeoff ──
// On HMAC verify failure, the 401 response includes the verifier's
// `reason` field (bad_signature | stale | replay | malformed_body |
// missing_field). On a VPC-internal HMAC-authenticated channel this
// is a small information leak — an attacker without the secret can
// only ever produce `bad_signature`, so the differentiation only
// helps a legitimate operator triage their own misconfig (clock
// skew → stale; replay-protection collision → replay). Worth the
// triage value at this trust boundary.
//
// ── Bind address ──
// Defaults to `0.0.0.0` because in awsvpc mode each Fargate task has
// its own ENI; binding all interfaces means "this ENI" — there are
// no other interfaces in the task's network namespace, so this is
// not actually a broader bind than 127.0.0.1 in the network-namespace
// sense. The task security group is the perimeter. Tests pass
// `127.0.0.1` to avoid binding a routable address during the suite.
//
// IPv4-only assumption: this default works on today's Fargate
// (IPv4 ENI). If the bot ever runs on an IPv6-only ENI, callers
// MUST pass `bindAddr: '::'` (dual-stack) explicitly — `0.0.0.0`
// won't bind to v6. The peer-heartbeat module's `net.isIP` write
// validator and the control-client's read validator both accept
// IPv6 literals, so the wire is v6-ready; only this bind default
// is v4-only. Revisit when ECS rolls out v6-only task networking.

const http = require('node:http');

const { unwrapEnvelope } = require('./gateway-hmac');

const DEFAULT_BODY_BYTE_CAP = 8 * 1024;
const DEFAULT_REQUEST_TIMEOUT_MS = 5_000;

function startControlChannelServer({
  hmac,
  selfInstanceId,
  isKnownPeer,
  onHandoff,
  logger,
  port,
  bindAddr = '0.0.0.0',
  bodyByteCap = DEFAULT_BODY_BYTE_CAP,
  requestTimeoutMs = DEFAULT_REQUEST_TIMEOUT_MS,
  onListenError,
} = {}) {
  if (!hmac || typeof hmac.verify !== 'function') {
    throw new Error('startControlChannelServer: hmac with verify() is required');
  }
  if (!selfInstanceId) {
    throw new Error('startControlChannelServer: selfInstanceId is required');
  }
  if (typeof isKnownPeer !== 'function') {
    throw new Error('startControlChannelServer: isKnownPeer function is required');
  }
  if (typeof onHandoff !== 'function') {
    throw new Error('startControlChannelServer: onHandoff function is required');
  }
  if (!logger) throw new Error('startControlChannelServer: logger is required');
  if (typeof onListenError !== 'function') {
    throw new Error('startControlChannelServer: onListenError function is required');
  }
  if (port == null) {
    throw new Error('startControlChannelServer: port is required');
  }
  // Floor on requestTimeoutMs. The headersTimeout formula below is
  // `min(max(1000, requestTimeout/2), requestTimeout - 100)`. Below
  // ~1100 ms the outer min picks `requestTimeout - 100`, and below
  // 1100 ms entirely it collapses toward 1 ms — effectively
  // disabling header-read protection. Fail loud at boot rather than
  // letting a misconfigured caller silently lose the invariant.
  // 1100 ms is the floor for both clauses to produce ≥ 1000 ms.
  if (!Number.isInteger(requestTimeoutMs) || requestTimeoutMs < 1100) {
    throw new Error('startControlChannelServer: requestTimeoutMs must be an integer >= 1100 (see headersTimeout invariant)');
  }
  // bodyByteCap = 0 would 413 every request; negative / NaN / float
  // would produce surprising int-coercion behavior in the stream
  // length check. Fail loud at boot.
  if (!Number.isInteger(bodyByteCap) || bodyByteCap <= 0) {
    throw new Error('startControlChannelServer: bodyByteCap must be a positive integer');
  }

  const server = http.createServer((req, res) => {
    handleRequest(req, res, {
      hmac, selfInstanceId, isKnownPeer, onHandoff, logger, bodyByteCap,
    }).catch((err) => {
      // Include req.method + req.url so an operator triaging a 500
      // can correlate this log line with what the client was
      // attempting. sendJson is guarded against already-ended
      // responses, so the 500 write is safe even if the failure
      // happened after headers were sent.
      logger.error('control-channel: unhandled handler error', {
        error: err.message, method: req.method, url: req.url,
      });
      sendJson(res, 500, { error: 'internal_error' });
    });
  });

  // Cap idle/header timeouts to bound resource usage. Match the
  // ordering constraint Node enforces: headersTimeout < requestTimeout.
  //
  // Floor at 1 s prevents the `headersTimeout = 0` (= disabled)
  // footgun for tiny caller-provided requestTimeoutMs. The outer
  // Math.min then caps at `requestTimeout - 100` so the
  // headersTimeout < requestTimeout invariant holds even when
  // the floor would otherwise push it over the request timeout.
  // With defaults (5 s request) this yields 2.5 s headersTimeout —
  // the floor only kicks in below requestTimeoutMs ≈ 2 s.
  //
  // The factory-level `requestTimeoutMs < 1100` guard above ensures
  // both clauses can produce a meaningful >= 1000 ms — without it,
  // requestTimeoutMs=50 would yield headersTimeout=1ms (effectively
  // disabling header-read protection).
  server.requestTimeout = requestTimeoutMs;
  server.headersTimeout = Math.min(
    Math.max(1_000, Math.floor(requestTimeoutMs / 2)),
    Math.max(1, requestTimeoutMs - 100),
  );

  server.on('error', (err) => {
    logger.error('control-channel: listener failed', { error: err.message, code: err.code });
    onListenError(err);
  });

  server.listen(port, bindAddr, () => {
    logger.info('control-channel: listening', { addr: bindAddr, port: server.address().port });
  });

  return server;
}

async function handleRequest(req, res, ctx) {
  const path = req.url.split('?')[0];

  // All short-circuit responses below set `Connection: close`. We
  // bail without reading the request body, so any pending body
  // bytes would otherwise confuse a keep-alive client's next
  // request on the same socket. Same shape as the 413 path.
  if (path !== '/control/yours') {
    sendJson(res, 404, { error: 'not_found' }, { Connection: 'close' });
    return;
  }
  // Path matches but wrong method → 405 with Allow header (RFC 9110
  // §9.1). The distinction from a 404 matters for triage: it tells
  // an operator "the endpoint exists but someone is hitting it
  // with the wrong verb" (e.g., a probe misconfigured).
  if (req.method !== 'POST') {
    sendJson(res, 405, { error: 'method_not_allowed' }, { Allow: 'POST', Connection: 'close' });
    return;
  }
  // Content-Type must be application/json (required, not optional).
  // 415 unsupported_media_type is the RFC-correct response — and
  // helps operator triage by distinguishing "probe with wrong /
  // missing content type" (415) from "real client with a bad
  // envelope" (400). Our control-client always sets it; rejecting
  // missing CT also catches `curl` probes without `-H` upfront.
  // We accept charset parameters (`application/json; charset=utf-8`)
  // by matching the prefix, plus RFC 9110-style trailing whitespace
  // after the media type (`application/json `).
  const contentType = req.headers['content-type'];
  if (contentType === undefined || !/^application\/json\s*(?:;|$)/i.test(contentType)) {
    sendJson(res, 415, { error: 'unsupported_media_type' }, { Connection: 'close' });
    return;
  }

  // Pre-check Content-Length against the body cap. The streaming
  // guard in readRequestBody is the load-bearing check (a client can
  // omit or lie about Content-Length), but a declared length over
  // cap is unambiguous — short-circuit before reading any bytes so
  // an obvious bad request doesn't get a per-chunk dance.
  // `parseInt` tolerates leading whitespace; we additionally require
  // a digits-only value to ignore garbage like `0xFF` or `1KB`.
  const declaredLengthRaw = req.headers['content-length'];
  if (declaredLengthRaw !== undefined && /^\d+$/.test(declaredLengthRaw)) {
    const declaredLength = parseInt(declaredLengthRaw, 10);
    if (Number.isFinite(declaredLength) && declaredLength > ctx.bodyByteCap) {
      ctx.logger.warn('control-channel: Content-Length exceeds cap', {
        declared: declaredLength, cap: ctx.bodyByteCap,
      });
      sendJson(res, 413, { error: 'body_too_large' }, { Connection: 'close' });
      return;
    }
  }

  let bodyBuf;
  try {
    bodyBuf = await readRequestBody(req, ctx.bodyByteCap);
  } catch (err) {
    if (err.code === 'BODY_TOO_LARGE') {
      ctx.logger.warn('control-channel: body exceeded cap', { cap: ctx.bodyByteCap });
      // `Connection: close` because we never read the rest of the
      // (paused) request body. Without forcing close, a keep-alive
      // client could attempt another request on the same socket
      // and the leftover unread bytes would confuse the HTTP
      // parser. Forcing close after this response makes the
      // unread body harmless.
      sendJson(res, 413, { error: 'body_too_large' }, { Connection: 'close' });
      return;
    }
    ctx.logger.warn('control-channel: body read failed', { error: err.message });
    sendJson(res, 400, { error: 'body_read_failed' });
    return;
  }

  const unwrapped = unwrapEnvelope(bodyBuf);
  if (!unwrapped) {
    sendJson(res, 400, { error: 'invalid_envelope' });
    return;
  }

  const verifyResult = ctx.hmac.verify(unwrapped);
  if (!verifyResult.ok) {
    ctx.logger.warn('control-channel: hmac verify failed', { reason: verifyResult.reason });
    sendJson(res, 401, { error: 'unauthorized', reason: verifyResult.reason });
    return;
  }

  const payload = verifyResult.payload;
  const invalidField = findInvalidHandoffField(payload);
  if (invalidField) {
    ctx.logger.warn('control-channel: payload shape invalid', { field: invalidField });
    sendJson(res, 400, { error: 'invalid_payload' });
    return;
  }

  if (payload.peer_instance_id !== ctx.selfInstanceId) {
    ctx.logger.warn('control-channel: body addressed to wrong peer', {
      addressedTo: payload.peer_instance_id, self: ctx.selfInstanceId,
    });
    sendJson(res, 400, { error: 'wrong_peer' }, { Connection: 'close' });
    return;
  }

  if (!ctx.isKnownPeer(payload.active_instance_id)) {
    ctx.logger.warn('control-channel: handoff from unknown peer', {
      activeInstanceId: payload.active_instance_id,
    });
    sendJson(res, 400, { error: 'unknown_peer' }, { Connection: 'close' });
    return;
  }

  try {
    await ctx.onHandoff({
      activeInstanceId: payload.active_instance_id,
      expectedVersion: payload.expected_version,
    });
  } catch (err) {
    ctx.logger.error('control-channel: onHandoff threw', { error: err.message });
    sendJson(res, 500, { error: 'handoff_failed' });
    return;
  }

  ctx.logger.info('control-channel: handoff accepted', {
    activeInstanceId: payload.active_instance_id,
    expectedVersion: payload.expected_version,
  });
  sendJson(res, 200, { status: 'ok' });
}

// Per-field validation for the handoff payload. Returns the name of
// the first invalid field, or null if everything checks out. Named-
// field-in-log makes a 400 invalid_payload easier to root-cause than
// a 5-clause boolean expression with a `hasActive: 'string'` log
// entry that doesn't say which field bad.
function findInvalidHandoffField(payload) {
  if (typeof payload.active_instance_id !== 'string' || payload.active_instance_id.length === 0) {
    return 'active_instance_id';
  }
  if (typeof payload.peer_instance_id !== 'string' || payload.peer_instance_id.length === 0) {
    return 'peer_instance_id';
  }
  // Upper bound is a sanity ceiling, not a security primitive — the
  // HMAC has already authenticated the sender. A peer adopting
  // Number.MAX_SAFE_INTEGER as the version cursor would burn
  // ~2^53 CAS values before the row could ever match again; reject
  // anything that's clearly not a real lock-version number.
  if (typeof payload.expected_version !== 'number'
      || !Number.isInteger(payload.expected_version)
      || payload.expected_version <= 0
      || payload.expected_version > Number.MAX_SAFE_INTEGER) {
    return 'expected_version';
  }
  return null;
}

function readRequestBody(req, byteCap) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let totalLength = 0;
    let settled = false;

    function settle(fn, arg) {
      if (settled) return;
      settled = true;
      fn(arg);
    }

    req.on('data', (chunk) => {
      if (settled) return;
      totalLength += chunk.length;
      if (totalLength > byteCap) {
        // `req.pause()` (NOT `req.destroy()`) stops further data
        // events without tearing down the socket. Destroying here
        // would race the 413 response: the handler's
        // sendJson(res, 413, ...) runs in the next microtask, by
        // which point the destroyed socket may not be writable,
        // and legitimate over-cap clients wouldn't see the 413.
        // Pause keeps the response path alive; TCP backpressure
        // bounds the attacker's per-connection cost.
        req.pause();
        const err = new Error('body-too-large');
        err.code = 'BODY_TOO_LARGE';
        settle(reject, err);
        return;
      }
      chunks.push(chunk);
    });

    req.on('end', () => { settle(resolve, Buffer.concat(chunks)); });
    req.on('error', (err) => { settle(reject, err); });
    // 'close' fires after the request stream is fully done — either
    // by 'end' (we already settled resolve) or by abort/transport
    // error (we may not have settled yet). Use `req.destroyed`
    // post-close as the abort signal. The older 'aborted' event was
    // deprecated in Node 17 in favor of this pattern; settling here
    // makes the abort path forward-compatible.
    req.on('close', () => {
      if (req.destroyed) {
        settle(reject, new Error('request_aborted'));
      }
    });
  });
}

function sendJson(res, status, body, extraHeaders = {}) {
  // `res.destroyed` covers the client-abort path: the request was
  // aborted mid-stream, the socket is gone, and a writeHead/end
  // pair would emit 'error' on the response. Without this guard
  // that 'error' would propagate to the server-level error handler
  // (which is scoped to listen errors, not per-request) and
  // surface a benign client abort as a listener fault.
  if (res.headersSent || res.writableEnded || res.destroyed) return;
  const buf = Buffer.from(JSON.stringify(body), 'utf8');
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': buf.length,
    ...extraHeaders,
  });
  res.end(buf);
}

module.exports = {
  startControlChannelServer,
  // Exported for tests that want to drive the handler without
  // actually binding a socket.
  _handleRequestForTest: handleRequest,
  _readRequestBodyForTest: readRequestBody,
  DEFAULT_BODY_BYTE_CAP,
  DEFAULT_REQUEST_TIMEOUT_MS,
};
