const {
  createHash,
  createPrivateKey,
  randomBytes,
  randomUUID,
  sign,
} = require('crypto');
const { createPortalOpener } = require('@layervai/qurl/node');
const { QURLClient, DelegatedBatchOutcomeUnknownError } = require('@layervai/qurl');

const config = require('./config');
const { PRIVATE_SEND_MINT_BUDGET_MS } = require('./constants');
const logger = require('./logger');

const UPLOAD_PATH = '/internal/v1/uploads';
const UPLOAD_DOMAIN = 'LV-QURL-UPLOAD-AUTH-V1';
const P256_ORDER = BigInt('0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551');
const P256_HALF_ORDER = P256_ORDER / 2n;
const MAX_UPLOAD_ATTEMPTS = 3;
const MAX_BATCH_ATTEMPTS = 3;
const MAX_BATCH_POLLS = 900;
const RETRY_BACKOFF_BASE_MS = 250;
const UPLOAD_REQUEST_TIMEOUT_MS = 60000;
const BATCH_REQUEST_TIMEOUT_MS = 30000;

let opener = null;
let recoveryTimer = null;
let signerKey = null;

function u32(value) {
  const out = Buffer.alloc(4);
  out.writeUInt32BE(value);
  return out;
}

function frame(value) {
  const bytes = Buffer.from(String(value), 'utf8');
  return Buffer.concat([u32(bytes.length), bytes]);
}

function canonicalMessage(domain, fields) {
  return Buffer.concat([
    Buffer.from(domain, 'ascii'),
    Buffer.from([0]),
    ...fields.map(frame),
  ]);
}

function canonicalUploadMessage(fields) {
  return canonicalMessage(UPLOAD_DOMAIN, [
    'POST', fields.authority, UPLOAD_PATH, fields.timestamp, fields.nonce,
    fields.clientId, fields.keyId, fields.audienceKeyId, fields.bodySha256,
    fields.bodyLength, fields.contentType, fields.filename, fields.viewerTtlSeconds,
    fields.authorityExpiresAt, fields.requestId,
  ]);
}

function bigintFromBytes(bytes) {
  const hex = bytes.toString('hex');
  return BigInt(`0x${hex || '0'}`);
}

function unsignedInteger(value) {
  let hex = value.toString(16);
  if (hex.length % 2) hex = `0${hex}`;
  let bytes = Buffer.from(hex, 'hex');
  while (bytes.length > 1 && bytes[0] === 0) bytes = bytes.subarray(1);
  if (bytes[0] & 0x80) bytes = Buffer.concat([Buffer.from([0]), bytes]);
  return Buffer.concat([Buffer.from([0x02, bytes.length]), bytes]);
}

function strictDerLowSSign(privateKey, message) {
  const p1363 = sign('sha256', message, { key: privateKey, dsaEncoding: 'ieee-p1363' });
  if (p1363.length !== 64) throw new Error('private upload signer returned an invalid P-256 signature');
  const r = bigintFromBytes(p1363.subarray(0, 32));
  const rawS = bigintFromBytes(p1363.subarray(32));
  const s = rawS > P256_HALF_ORDER ? P256_ORDER - rawS : rawS;
  if (r <= 0n || r >= P256_ORDER || s <= 0n || s > P256_HALF_ORDER) {
    throw new Error('private upload signer returned an out-of-range signature');
  }
  const content = Buffer.concat([unsignedInteger(r), unsignedInteger(s)]);
  return Buffer.concat([Buffer.from([0x30, content.length]), content]).toString('base64url');
}

function loadSigner(pem) {
  if (typeof pem !== 'string' || !pem.includes('-----BEGIN PRIVATE KEY-----')) {
    throw new Error('PRIVATE_UPLOAD_SIGNER_PRIVATE_KEY_PEM must be a PKCS#8 private key');
  }
  const key = createPrivateKey(pem);
  if (key.asymmetricKeyType !== 'ec' || key.asymmetricKeyDetails?.namedCurve !== 'prime256v1') {
    throw new Error('PRIVATE_UPLOAD_SIGNER_PRIVATE_KEY_PEM must be an ECDSA P-256 key');
  }
  for (const [name, value] of [
    ['PRIVATE_UPLOAD_SIGNER_CLIENT_ID', config.PRIVATE_UPLOAD_SIGNER_CLIENT_ID],
    ['PRIVATE_UPLOAD_SIGNER_KEY_ID', config.PRIVATE_UPLOAD_SIGNER_KEY_ID],
  ]) {
    if (typeof value !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value)) {
      throw new Error(`${name} is outside the private-upload v1 contract`);
    }
  }
  return key;
}

function getSigner() {
  if (!signerKey) signerKey = loadSigner(config.PRIVATE_UPLOAD_SIGNER_PRIVATE_KEY_PEM);
  return signerKey;
}

function canonicalContentType(value) {
  const type = String(value || 'application/octet-stream').split(';', 1)[0].trim().toLowerCase();
  if (!/^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(type)) {
    throw new Error('private upload content type must be a concrete parameter-free media type');
  }
  return type;
}

function canonicalViewerTtl(value) {
  if (value === null || value === undefined || Object.is(value, 0)) return '0';
  if (!Number.isFinite(value) || value < 0.5 || value > 3600) {
    throw new Error('private upload viewer TTL must be 0.5 through 3600 seconds');
  }
  const scaled = value * 1000;
  const millis = Math.round(scaled);
  if (Math.abs(scaled - millis) > 1e-9) {
    throw new Error('private upload viewer TTL must have exact millisecond resolution');
  }
  return (millis / 1000).toFixed(3).replace(/0+$/, '').replace(/\.$/, '');
}

function canonicalFilename(value) {
  // TODO(upstream-contract): Connector privateupload accepts NFC display names
  // of 1..180 UTF-8 bytes without these classes. Normalize cosmetic metadata
  // before it enters either the signed message or the transport header.
  const cleaned = String(value ?? '').replace(/[\\/\p{Cc}\p{Cf}\p{Co}\p{Zl}\p{Zp}]/gu, '').normalize('NFC').trim();
  let filename = '';
  let bytes = 0;
  for (const point of cleaned) {
    bytes += Buffer.byteLength(point, 'utf8');
    if (bytes > 180) break;
    filename += point;
  }
  filename = filename.trim();
  return !filename || filename === '.' || filename === '..' ? 'unnamed_file' : filename;
}

function authorityFor(target) {
  const url = target instanceof URL ? target : new URL(target);
  if (url.pathname !== UPLOAD_PATH || url.search || url.hash) {
    const error = new Error('Private upload portal target does not match the signed path');
    error.noRetry = true;
    throw error;
  }
  return url.host.toLowerCase();
}

function signedTransportFields({ authority, body, bodySha256, contentType, filename, viewerTtlSeconds, audienceKeyId, authorityExpiresAt, requestId }) {
  const timestamp = String(Math.floor(Date.now() / 1000));
  const nonce = randomBytes(32).toString('base64url');
  const base = {
    authority,
    timestamp,
    nonce,
    clientId: config.PRIVATE_UPLOAD_SIGNER_CLIENT_ID,
    keyId: config.PRIVATE_UPLOAD_SIGNER_KEY_ID,
    bodySha256,
    bodyLength: String(body.length),
    requestId,
  };
  const message = canonicalUploadMessage({
    ...base, audienceKeyId, contentType, filename, viewerTtlSeconds, authorityExpiresAt,
  });
  return {
    signature: strictDerLowSSign(getSigner(), message),
    timestamp,
    nonce,
  };
}

function errorCode(body) {
  return body?.error?.code || body?.code || null;
}

function isCanonicalUtcSecond(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(value)) return false;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) && new Date(parsed).toISOString().replace('.000Z', 'Z') === value;
}

function isUploadHandle(value) {
  return typeof value === 'string' && /^upl_[A-Za-z0-9_-]{43}$/.test(value);
}

function isUuidV4(value) {
  return typeof value === 'string'
    && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
}

function validateUploadResult(data, { authorityExpiresAt } = {}) {
  if (!isUploadHandle(data?.upload_handle)
      || typeof data?.mint_capability !== 'string'
      || data.mint_capability.length < 1
      || data.mint_capability.length > 8192
      || !isCanonicalUtcSecond(data?.mint_capability_expires_at)
      || !isCanonicalUtcSecond(data?.authority_expires_at)
      || data.authority_expires_at !== authorityExpiresAt) {
    throw new Error('Private upload returned an invalid response');
  }
  return data;
}

async function responseJson(response, label) {
  let body;
  try {
    body = await response.json();
  } catch {
    const err = new Error(`${label} returned invalid JSON (${response.status})`);
    err.status = response.status;
    throw err;
  }
  if (!response.ok) {
    const err = new Error(`${label} failed (${response.status})`);
    err.status = response.status;
    err.apiCode = errorCode(body);
    err.response = response;
    throw err;
  }
  return body;
}

function requirePrivateCredential(credential) {
  if (!credential || typeof credential.apiKey !== 'string' || !credential.apiKey
      || typeof credential.keyId !== 'string' || !/^key_[A-Za-z0-9]{12}$/.test(credential.keyId)) {
    throw new Error('Discord guild needs a current external identity binding; ask an admin to run /qurl setup');
  }
  return credential;
}

function getOpener() {
  if (!config.PRIVATE_UPLOAD_QURL) throw new Error('PRIVATE_UPLOAD_QURL is not configured');
  if (!opener) opener = createPortalOpener({ qurl: config.PRIVATE_UPLOAD_QURL, openTimeoutMs: 30000 });
  return opener;
}

async function startPrivateUploader() {
  if (!config.PRIVATE_UPLOAD_QURL) return;
  getSigner();
  await getOpener().start();
  if (!recoveryTimer) {
    recoveryTimer = setInterval(() => {
      const current = opener;
      if (current?.health().state === 'degraded') {
        current.start().catch(err => logger.warn('Private upload NHP session recovery failed', { error: err.message }));
      }
    }, 5000);
    recoveryTimer.unref();
  }
}

async function closePrivateUploader() {
  if (recoveryTimer) clearInterval(recoveryTimer);
  recoveryTimer = null;
  const current = opener;
  opener = null;
  signerKey = null;
  if (current) await current.close();
}

async function portalRequest(build) {
  return getOpener().fetch(build, { redirects: 'error' });
}

async function uploadPrivate(bodyInput, {
  filename, contentType, viewerTtlSeconds, credential, authorityExpiresAt,
  deadlineMs, requestId = randomUUID(), sleep = delay,
}) {
  // Own immutable bytes across retries; callers may reuse their input buffer.
  const body = Buffer.from(bodyInput);
  const digest = createHash('sha256').update(body).digest();
  const bodySha256 = digest.toString('hex');
  const contentDigestHeader = `sha-256=:${digest.toString('base64')}:`;
  if (body.length < 1) throw new Error('private upload body must not be empty');
  if (!Number.isSafeInteger(deadlineMs) || deadlineMs <= Date.now()) {
    throw new Error('Private upload deadline is invalid or expired');
  }
  if (!isUuidV4(requestId)) throw new Error('private upload request ID must be a lowercase UUIDv4');
  if (!isCanonicalUtcSecond(authorityExpiresAt)) {
    throw new Error('private upload authority expiry must be canonical UTC RFC3339 seconds');
  }
  const safeFilename = canonicalFilename(filename);
  const safeContentType = canonicalContentType(contentType);
  const safeViewerTtl = canonicalViewerTtl(viewerTtlSeconds);
  const guildCredential = requirePrivateCredential(credential);
  let lastError;
  for (let attempt = 1; attempt <= MAX_UPLOAD_ATTEMPTS; attempt++) {
    try {
      const response = await portalRequest((target) => {
        const timeoutMs = requestTimeoutMs(
          deadlineMs,
          UPLOAD_REQUEST_TIMEOUT_MS,
          'Private upload did not complete before the Discord interaction deadline',
        );
        const authority = authorityFor(target);
        const auth = signedTransportFields({
          authority, body, bodySha256, contentType: safeContentType, filename: safeFilename, viewerTtlSeconds: safeViewerTtl,
          audienceKeyId: guildCredential.keyId, authorityExpiresAt, requestId,
        });
        return {
          method: 'POST',
          headers: {
            'Content-Type': safeContentType,
            'Content-Length': String(body.length),
            'Content-Digest': contentDigestHeader,
            'X-LayerV-Client-ID': config.PRIVATE_UPLOAD_SIGNER_CLIENT_ID,
            'X-LayerV-Key-ID': config.PRIVATE_UPLOAD_SIGNER_KEY_ID,
            'X-LayerV-Timestamp': auth.timestamp,
            'X-LayerV-Nonce': auth.nonce,
            'X-LayerV-Audience-Key-ID': guildCredential.keyId,
            'X-LayerV-Authority-Expires-At': authorityExpiresAt,
            'X-LayerV-Upload-Request-ID': requestId,
            'X-LayerV-Filename-B64': Buffer.from(safeFilename, 'utf8').toString('base64url'),
            'X-LayerV-Viewer-TTL-Seconds': safeViewerTtl,
            'X-LayerV-Upload-Signature': auth.signature,
          },
          body,
          signal: AbortSignal.timeout(timeoutMs),
        };
      });
      const result = await responseJson(response, 'Private upload');
      // TODO(upstream-contract): the private-upload v1 service returns 201 for
      // the first completion and 200 for an exact replay after an ambiguous
      // result. Both carry the same validated response shape.
      if (response.status !== 201 && response.status !== 200) {
        const err = new Error(`Private upload returned an unexpected status (${response.status})`);
        err.noRetry = true;
        throw err;
      }
      const data = validateUploadResult(result?.data, { authorityExpiresAt });
      return { ...data, upload_request_id: requestId };
    } catch (err) {
      lastError = err;
      if (err.noRetry || (err.status && !(err.status === 503 && err.apiCode === 'mutation_outcome_unknown'))) throw err;
      if (attempt < MAX_UPLOAD_ATTEMPTS) {
        const waitMs = retryDelayMs(err.response, attempt);
        if (Date.now() + waitMs > deadlineMs) {
          throw new Error('Private upload did not complete before the Discord interaction deadline');
        }
        await sleep(waitMs);
      }
    }
  }
  throw lastError;
}

function retryDelayMs(response, attempt, fallbackMs = Math.min(30_000, RETRY_BACKOFF_BASE_MS * (2 ** (attempt - 1)))) {
  const raw = response?.headers?.get('retry-after')?.trim();
  let milliseconds;
  if (/^\d+$/.test(raw || '')) {
    milliseconds = Number(raw) * 1000;
  } else if (/^[A-Za-z]{3}, \d{2} [A-Za-z]{3} \d{4} \d{2}:\d{2}:\d{2} GMT$/.test(raw || '')) {
    milliseconds = Date.parse(raw) - Date.now();
  }
  // Retry-After is advisory. Missing/damaged values, zero, and elapsed dates
  // retain a positive bounded backoff; callers still enforce the send deadline.
  return Number.isSafeInteger(milliseconds) && milliseconds > 0 ? milliseconds : fallbackMs;
}

function requestTimeoutMs(deadlineMs, maximumMs, deadlineMessage) {
  const remainingMs = deadlineMs - Date.now();
  if (remainingMs <= 0) throw new Error(deadlineMessage);
  return Math.min(maximumMs, remainingMs);
}

function delay(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

async function redeemDelegatedBatch(upload, {
  credential, grants, idempotencyKey = randomUUID(), sleep = delay,
  deadlineMs = Date.now() + PRIVATE_SEND_MINT_BUDGET_MS,
}) {
  if (!Number.isSafeInteger(deadlineMs) || deadlineMs <= Date.now()) {
    throw new Error('Delegated qURL batch deadline is invalid or expired');
  }
  if (!config.QURL_LINK_DOMAIN) {
    throw new Error('QURL_LINK_DOMAIN is required for delegated qURL batches');
  }
  const guildCredential = requirePrivateCredential(credential);
  const client = new QURLClient({
    apiKey: guildCredential.apiKey,
    baseUrl: config.QURL_ENDPOINT,
    maxRetries: 0,
    timeout: BATCH_REQUEST_TIMEOUT_MS,
    fetch: (url, init) => {
      const signal = AbortSignal.timeout(requestTimeoutMs(deadlineMs, BATCH_REQUEST_TIMEOUT_MS,
        'Delegated qURL batch did not complete before the Discord interaction deadline'));
      return fetch(url, { ...init, redirect: 'error',
        signal: init?.signal ? AbortSignal.any([signal, init.signal]) : signal });
    },
  });
  const input = { mint_capability: upload.mint_capability, grants };
  const wait = async milliseconds => {
    if (Date.now() + milliseconds > deadlineMs) {
      throw new Error('Delegated qURL batch did not complete before the Discord interaction deadline');
    }
    await sleep(milliseconds);
  };
  const retryWait = (seconds, fallback) => Number.isFinite(seconds) && seconds > 0
    ? seconds * 1000 : fallback;
  let accepted;
  let uncertainCreate = false;
  let terminal = false;
  try {
    for (let attempt = 1; attempt <= MAX_BATCH_ATTEMPTS; attempt++) {
      try {
        accepted = await client.createDelegatedQurlBatch(input, { idempotencyKey });
        break;
      } catch (error) {
        if (error instanceof DelegatedBatchOutcomeUnknownError) {
          uncertainCreate = true;
          if (error.batchId) {
            accepted = { batch_id: error.batchId };
            break;
          }
        }
        const cause = error.cause || error;
        if (!(error instanceof DelegatedBatchOutcomeUnknownError)
            || (cause.status >= 200 && cause.status < 300 && cause.status !== 202)
            || (cause.status >= 400 && !(cause.status === 503 && cause.code === 'mutation_outcome_unknown'))
            || attempt === MAX_BATCH_ATTEMPTS) throw error;
        await wait(retryWait(cause.retryAfter, RETRY_BACKOFF_BASE_MS * (2 ** (attempt - 1))));
      }
    }
    let etag = accepted.etag;
    let waitMs = retryWait(accepted.retry_after, 1000);
    let transientFailures = 0;
    for (let poll = 0; poll < MAX_BATCH_POLLS; poll++) {
      await wait(waitMs);
      let result;
      try {
        result = await client.getDelegatedQurlBatch(accepted.batch_id, etag ? { etag } : undefined);
      } catch (error) {
        if (error.partialQurlIds) {
          error.partialLinkCount = error.partialQurlIds.length;
          throw error;
        }
        if (!['network_error', 'timeout', 'unexpected_response'].includes(error.code)
            && ![429, 500, 502, 503, 504].includes(error.status)) throw error;
        waitMs = retryWait(error.retryAfter,
          Math.min(30_000, RETRY_BACKOFF_BASE_MS * (2 ** (Math.min(++transientFailures, 8) - 1))));
        continue;
      }
      transientFailures = 0;
      if (result.http_status !== 200) {
        etag = result.etag || etag;
        waitMs = retryWait(result.retry_after, 1000);
        continue;
      }
      const items = result.results;
      const partialQurlIds = [...new Set(items
        .filter(item => item?.status === 'succeeded' && /^q_[0-9a-f]{11}$/.test(item?.qurl?.qurl_id || ''))
        .map(item => item.qurl.qurl_id))];
      if (result.item_count !== grants.length || items.length !== grants.length) {
        const error = new Error('Delegated qURL batch returned an invalid terminal response');
        error.partialQurlIds = partialQurlIds;
        throw error;
      }
      terminal = true;
      const expectedLinkOrigin = `https://${config.QURL_LINK_DOMAIN}`;
      const seenQurlIds = new Set();
      const seenQurlLinks = new Set();
      const links = items.map((item, index) => {
        let qurlLink;
        try {
          qurlLink = new URL(item?.qurl?.qurl_link);
        } catch {
          qurlLink = null;
        }
        if (item?.index !== index || item?.status !== 'succeeded'
            || !/^q_[0-9a-f]{11}$/.test(item?.qurl?.qurl_id || '')
            || !qurlLink || qurlLink.protocol !== 'https:' || qurlLink.username || qurlLink.password
            || qurlLink.origin !== expectedLinkOrigin || qurlLink.pathname !== '/'
            || qurlLink.search || qurlLink.hash.length < 2
            || !isCanonicalUtcSecond(item?.qurl?.expires_at)) {
          const err = new Error(`Delegated qURL batch item ${index} failed`);
          err.apiCode = item?.error?.code || 'delegated_batch_item_failed';
          err.partialLinkCount = partialQurlIds.length;
          err.partialQurlIds = partialQurlIds;
          throw err;
        }
        if (seenQurlIds.has(item.qurl.qurl_id) || seenQurlLinks.has(qurlLink.href)) {
          const err = new Error('Delegated qURL batch returned a duplicate bearer grant');
          err.apiCode = 'delegated_batch_item_failed';
          err.partialLinkCount = partialQurlIds.length;
          err.partialQurlIds = partialQurlIds;
          throw err;
        }
        seenQurlIds.add(item.qurl.qurl_id);
        seenQurlLinks.add(qurlLink.href);
        return { ...item.qurl, resource_id: item.qurl.qurl_id };
      });
      if (result.status !== 'succeeded') {
        const err = new Error('Delegated qURL batch did not fully succeed');
        err.apiCode = 'delegated_batch_item_failed';
        err.partialLinkCount = partialQurlIds.length;
        err.partialQurlIds = partialQurlIds;
        throw err;
      }
      return links;
    }
    throw new Error('Delegated qURL batch did not complete before the poll limit');
  } catch (cause) {
    // The SDK can expose read-only partialQurlIds. Own the cleanup ledger here.
    // Never pass upstream detail (which can echo credentials) to Discord/logs.
    const error = new Error('Delegated qURL batch failed; result or cleanup may be incomplete');
    error.status = cause.status;
    error.apiCode = cause.apiCode || cause.code;
    error.partialQurlIds = [...(cause.partialQurlIds || [])];
    error.partialLinkCount = error.partialQurlIds.length;
    error.batchOutcomeUnknown = !terminal && Boolean(accepted || uncertainCreate);
    if (error.batchOutcomeUnknown) {
      error.batchId = accepted?.batch_id;
      error.batchIdempotencyKey = idempotencyKey;
      error.unknownBatchExpiresAt = isCanonicalUtcSecond(upload.authority_expires_at)
        ? upload.authority_expires_at : undefined;
    }
    throw error;
  }
}

module.exports = {
  startPrivateUploader,
  closePrivateUploader,
  uploadPrivate,
  redeemDelegatedBatch,
};

if (process.env.NODE_ENV === 'test') {
  module.exports.__testExports = {
    canonicalUploadMessage,
    strictDerLowSSign,
    canonicalFilename,
    canonicalContentType,
    canonicalViewerTtl,
    requirePrivateCredential,
  };
}
