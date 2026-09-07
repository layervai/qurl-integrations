const {
  createHash,
  createPrivateKey,
  randomBytes,
  randomUUID,
  sign,
} = require('crypto');
const { createPortalOpener } = require('@layervai/qurl/node');

const config = require('./config');
const { PRIVATE_SEND_MINT_BUDGET_MS } = require('./constants');
const logger = require('./logger');

const UPLOAD_PATH = '/internal/v1/uploads';
const UPLOAD_DOMAIN = 'LV-QURL-UPLOAD-AUTH-V1';
const UPLOAD_REQUEST_DOMAIN = 'LV-QURL-UPLOAD-REQUEST-V1';
const P256_ORDER = BigInt('0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551');
const P256_HALF_ORDER = P256_ORDER / 2n;
const MAX_UPLOAD_ATTEMPTS = 3;
const MAX_BATCH_ATTEMPTS = 3;
const MAX_BATCH_POLLS = 900;
const RETRY_BACKOFF_BASE_MS = 250;

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

function stableUploadRequestDigest(fields) {
  const canonical = canonicalMessage(UPLOAD_REQUEST_DOMAIN, [
    fields.clientId, fields.audienceKeyId, fields.bodySha256,
    fields.bodyLength, fields.contentType, fields.filename,
    fields.viewerTtlSeconds, fields.authorityExpiresAt, fields.requestId,
  ]);
  return sha256Hex(canonical);
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
  const filename = String(value ?? '').normalize('NFC');
  const bytes = Buffer.byteLength(filename, 'utf8');
  if (bytes < 1 || bytes > 180 || filename === '.' || filename === '..'
      || /[\\/\p{Cc}\p{Cf}\p{Co}\p{Zl}\p{Zp}]/u.test(filename)
      || /^\s|\s$/u.test(filename)) {
    throw new Error('private upload filename is outside the v1 contract');
  }
  return filename;
}

function authorityFor(target) {
  const url = target instanceof URL ? target : new URL(target);
  return url.host.toLowerCase();
}

function sha256Hex(body) {
  return createHash('sha256').update(body).digest('hex');
}

function contentDigest(body) {
  return `sha-256=:${createHash('sha256').update(body).digest('base64')}:`;
}

function signedTransportFields({ authority, body, contentType, filename, viewerTtlSeconds, audienceKeyId, authorityExpiresAt, requestId }) {
  const timestamp = String(Math.floor(Date.now() / 1000));
  const nonce = randomBytes(32).toString('base64url');
  const bodySha256 = sha256Hex(body);
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

function validateUploadResult(data, { authorityExpiresAt, uploadHandle } = {}) {
  if (!isUploadHandle(data?.upload_handle)
      || (uploadHandle && data.upload_handle !== uploadHandle)
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
  requestId = randomUUID(), sleep = delay,
}) {
  const body = Buffer.from(bodyInput);
  if (body.length < 1) throw new Error('private upload body must not be empty');
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
        const authority = authorityFor(target);
        const auth = signedTransportFields({
          authority, body, contentType: safeContentType, filename: safeFilename, viewerTtlSeconds: safeViewerTtl,
          audienceKeyId: guildCredential.keyId, authorityExpiresAt, requestId,
        });
        return {
          method: 'POST',
          headers: {
            'Content-Type': safeContentType,
            'Content-Length': String(body.length),
            'Content-Digest': contentDigest(body),
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
          signal: AbortSignal.timeout(60000),
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
        await sleep(retryDelayMs(err.response, attempt));
      }
    }
  }
  throw lastError;
}

function requiredRetryAfterMs(response) {
  const raw = response.headers.get('retry-after');
  if (!/^[1-9][0-9]*$/.test(raw || '')) {
    throw new Error('Delegated qURL batch returned an invalid Retry-After header');
  }
  const seconds = Number(raw);
  if (!Number.isSafeInteger(seconds)) {
    throw new Error('Delegated qURL batch returned an invalid Retry-After header');
  }
  return seconds * 1000;
}

function retryDelayMs(response, attempt) {
  const raw = response?.headers?.get('retry-after');
  if (raw != null) {
    return requiredRetryAfterMs(response);
  }
  return RETRY_BACKOFF_BASE_MS * (2 ** (attempt - 1));
}

function requiredEtag(response) {
  const value = response.headers.get('etag');
  if (!value || value.length > 96) throw new Error('Delegated qURL batch returned an invalid ETag header');
  return value;
}

function validatedBatchLocation(value, batchId) {
  let location;
  let endpoint;
  try {
    location = new URL(value);
    endpoint = new URL(config.QURL_ENDPOINT);
  } catch {
    throw new Error('Delegated qURL batch returned an invalid Location header');
  }
  if (location.origin !== endpoint.origin || location.username || location.password
      || location.search || location.hash
      || location.pathname !== `/v1/delegated-qurl-batches/${batchId}`) {
    throw new Error('Delegated qURL batch returned an invalid Location header');
  }
  return location.toString();
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
  const guildCredential = requirePrivateCredential(credential);
  const body = JSON.stringify({ mint_capability: upload.mint_capability, grants });
  const headers = {
    'Authorization': `Bearer ${guildCredential.apiKey}`,
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'Idempotency-Key': idempotencyKey,
  };
  let accepted;
  let lastError;
  for (let attempt = 1; attempt <= MAX_BATCH_ATTEMPTS; attempt++) {
    try {
      const response = await fetch(`${config.QURL_ENDPOINT}/v1/delegated-qurl-batches`, {
        method: 'POST', headers, body, redirect: 'error', signal: AbortSignal.timeout(30000),
      });
      if (response.status === 202) {
        const result = await responseJson(response, 'Delegated qURL batch');
        const data = result?.data;
        if (!/^dqb_[A-Za-z0-9_-]{22}$/.test(data?.batch_id || '')
            || data?.status !== 'queued' || data?.item_count !== grants.length
            || !isCanonicalUtcSecond(data?.submitted_at)) {
          throw new Error('Delegated qURL batch returned an invalid acceptance response');
        }
        accepted = { response, batchId: data.batch_id };
        break;
      }
      if (response.ok) {
        const err = new Error(`Delegated qURL batch returned an unexpected success status (${response.status})`);
        err.noRetry = true;
        throw err;
      }
      await responseJson(response, 'Delegated qURL batch');
    } catch (err) {
      lastError = err;
      if (err.noRetry || (err.status && !(err.status === 503 && err.apiCode === 'mutation_outcome_unknown'))) throw err;
      if (attempt < MAX_BATCH_ATTEMPTS) {
        const waitMs = err.status === 503
          ? requiredRetryAfterMs(err.response)
          : retryDelayMs(err.response, attempt);
        if (Date.now() + waitMs > deadlineMs) {
          throw new Error('Delegated qURL batch did not complete before the Discord interaction deadline');
        }
        await sleep(waitMs);
      }
    }
  }
  if (!accepted) throw lastError || new Error('Delegated qURL batch was not accepted');
  const location = validatedBatchLocation(accepted.response.headers.get('location'), accepted.batchId);
  let etag = requiredEtag(accepted.response);
  let waitMs = requiredRetryAfterMs(accepted.response);
  for (let poll = 0; poll < MAX_BATCH_POLLS; poll++) {
    if (Date.now() + waitMs > deadlineMs) {
      throw new Error('Delegated qURL batch did not complete before the Discord interaction deadline');
    }
    await sleep(waitMs);
    const pollHeaders = { 'Authorization': `Bearer ${guildCredential.apiKey}`, 'Accept': 'application/json' };
    if (etag) pollHeaders['If-None-Match'] = etag;
    const response = await fetch(location, {
      headers: pollHeaders, redirect: 'error', signal: AbortSignal.timeout(30000),
    });
    if (response.status === 304) {
      etag = requiredEtag(response);
      waitMs = requiredRetryAfterMs(response);
      continue;
    }
    if (response.status === 202) {
      etag = requiredEtag(response);
      waitMs = requiredRetryAfterMs(response);
      const pending = await responseJson(response, 'Delegated qURL batch status');
      if (pending?.data?.batch_id !== accepted.batchId
          || !['queued', 'running'].includes(pending?.data?.status)) {
        throw new Error('Delegated qURL batch returned an invalid pending response');
      }
      continue;
    }
    if (response.status !== 200) {
      await responseJson(response, 'Delegated qURL batch status');
      throw new Error(`Delegated qURL batch returned an unexpected status (${response.status})`);
    }
    const result = await responseJson(response, 'Delegated qURL batch status');
    if (result?.data?.batch_id !== accepted.batchId
        || !['succeeded', 'partially_failed', 'failed'].includes(result?.data?.status)
        || result?.data?.item_count !== grants.length
        || !isCanonicalUtcSecond(result?.data?.submitted_at)) {
      throw new Error('Delegated qURL batch returned an invalid terminal response');
    }
    const items = result?.data?.results;
    if (!Array.isArray(items) || items.length !== grants.length) {
      throw new Error('Delegated qURL batch returned an invalid terminal response');
    }
    return items.map((item, index) => {
      let qurlLink;
      try {
        qurlLink = new URL(item?.qurl?.qurl_link);
      } catch {
        qurlLink = null;
      }
      if (item?.index !== index || item?.status !== 'succeeded'
          || !/^q_[0-9a-f]{11}$/.test(item?.qurl?.qurl_id || '')
          || !qurlLink || qurlLink.protocol !== 'https:' || qurlLink.username || qurlLink.password
          || !isCanonicalUtcSecond(item?.qurl?.expires_at)) {
        const err = new Error(`Delegated qURL batch item ${index} failed`);
        err.apiCode = item?.error?.code || 'delegated_batch_item_failed';
        throw err;
      }
      return { ...item.qurl, resource_id: item.qurl.qurl_id };
    });
  }
  throw new Error('Delegated qURL batch did not complete before the poll limit');
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
    stableUploadRequestDigest,
    strictDerLowSSign,
    canonicalFilename,
    canonicalContentType,
    canonicalViewerTtl,
    requirePrivateCredential,
  };
}
