const crypto = require('crypto');

const mockOpener = {
  start: jest.fn(),
  close: jest.fn(),
  fetch: jest.fn(),
  health: jest.fn(() => ({ state: 'running' })),
};
const mockCreatePortalOpener = jest.fn(() => mockOpener);
const mockKeyPair = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
const mockConfig = {
  PRIVATE_UPLOAD_QURL: 'qurl://private-upload',
  PRIVATE_UPLOAD_SIGNER_PRIVATE_KEY_PEM: mockKeyPair.privateKey.export({ type: 'pkcs8', format: 'pem' }),
  PRIVATE_UPLOAD_SIGNER_CLIENT_ID: 'discord-sandbox',
  PRIVATE_UPLOAD_SIGNER_KEY_ID: 'discord-signing-v1',
  QURL_ENDPOINT: 'https://api.test.local',
};

jest.mock('@layervai/qurl/node', () => ({ createPortalOpener: mockCreatePortalOpener }));
jest.mock('../src/config', () => mockConfig);
jest.mock('../src/logger', () => ({ warn: jest.fn() }));

const privateUpload = require('../src/private-upload');
const {
  canonicalUploadMessage,
  stableUploadRequestDigest,
  canonicalRefreshMessage,
  strictDerLowSSign,
  canonicalViewerTtl,
} = privateUpload.__testExports;

const UPLOAD_VECTOR = {
  authority: '127.0.0.1:48123',
  timestamp: '1788645600',
  nonce: 'WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlo',
  clientId: 'discord-sandbox',
  keyId: 'discord-signing-v1',
  audienceKeyId: 'key_A1b2C3d4E5f6',
  bodySha256: '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824',
  bodyLength: '5',
  contentType: 'text/plain',
  filename: 'report.txt',
  viewerTtlSeconds: '30',
  authorityExpiresAt: '2026-09-06T22:00:00Z',
  requestId: '123e4567-e89b-42d3-a456-426614174000',
};

const UPLOAD_CANONICAL_HEX = '4c562d5155524c2d55504c4f41442d415554482d56310000000004504f53540000000f3132372e302e302e313a3438313233000000142f696e7465726e616c2f76312f75706c6f6164730000000a313738383634353630300000002b576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c6f0000000f646973636f72642d73616e64626f7800000012646973636f72642d7369676e696e672d7631000000106b65795f413162324333643445356636000000403263663234646261356662306133306532366538336232616335623965323965316231363165356331666137343235653733303433333632393338623938323400000001350000000a746578742f706c61696e0000000a7265706f72742e74787400000002333000000014323032362d30392d30365432323a30303a30305a0000002431323365343536372d653839622d343264332d613435362d343236363134313734303030';
const UPLOAD_REQUEST_DIGEST = '5573b5e08e820de835e3c6929de7f4602c403f5068a6b636653cae02a1fe0c9d';
const REFRESH_CANONICAL_HEX = '4c562d5155524c2d55504c4f41442d524546524553482d415554482d5631000000000550415443480000000f3132372e302e302e313a3438313233000000142f696e7465726e616c2f76312f75706c6f6164730000000a313738383634353630300000002b576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c7061576c6f0000000f646973636f72642d73616e64626f7800000012646973636f72642d7369676e696e672d76310000004034393437623464323461343230663363376136633566633530306232306233373561653834646639303166353362353166623864323738356566623437346663000000033231390000002431323365343536372d653839622d343264332d613435362d343236363134313734303030';

function jsonResponse(status, body, headers = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

function readDerS(signature) {
  let offset = 2;
  const rLength = signature[offset + 1];
  offset += 2 + rLength;
  const sLength = signature[offset + 1];
  const s = signature.subarray(offset + 2, offset + 2 + sLength);
  return BigInt(`0x${s.toString('hex')}`);
}

beforeEach(() => {
  jest.clearAllMocks();
  mockOpener.health.mockReturnValue({ state: 'running' });
});

afterEach(async () => {
  await privateUpload.closePrivateUploader();
});

test('upload and refresh canonical bytes match the private-upload v1 vectors', () => {
  expect(canonicalUploadMessage(UPLOAD_VECTOR).toString('hex')).toBe(UPLOAD_CANONICAL_HEX);
  expect(stableUploadRequestDigest(UPLOAD_VECTOR)).toBe(UPLOAD_REQUEST_DIGEST);
  const refresh = {
    authority: UPLOAD_VECTOR.authority,
    timestamp: UPLOAD_VECTOR.timestamp,
    nonce: UPLOAD_VECTOR.nonce,
    clientId: UPLOAD_VECTOR.clientId,
    keyId: UPLOAD_VECTOR.keyId,
    bodySha256: '4947b4d24a420f3c7a6c5fc500b20b375ae84df901f53b51fb8d2785efb474fc',
    bodyLength: '219',
    requestId: UPLOAD_VECTOR.requestId,
  };
  expect(canonicalRefreshMessage(refresh).toString('hex')).toBe(REFRESH_CANONICAL_HEX);
});

test('viewer TTL uses the exact signed canonical decimal contract', () => {
  expect([
    canonicalViewerTtl(null), canonicalViewerTtl(0), canonicalViewerTtl(0.5),
    canonicalViewerTtl(1), canonicalViewerTtl(1.234), canonicalViewerTtl(30),
    canonicalViewerTtl(3600),
  ]).toEqual(['0', '0', '0.5', '1', '1.234', '30', '3600']);
  for (const value of [-0, 0.499, 0.5001, 3600.001, Infinity, '0.5']) {
    expect(() => canonicalViewerTtl(value)).toThrow();
  }
});

test('P-256 signatures are strict DER, valid, and always low-S', () => {
  const message = canonicalUploadMessage(UPLOAD_VECTOR);
  const signature = Buffer.from(strictDerLowSSign(mockKeyPair.privateKey, message), 'base64url');
  expect(signature[0]).toBe(0x30);
  expect(signature.length).toBe(signature[1] + 2);
  expect(crypto.verify('sha256', message, mockKeyPair.publicKey, signature)).toBe(true);
  const halfOrder = BigInt('0x7fffffff800000007fffffffffffffffd E737d56d38bcf4279dce5617e3192a8'.replace(/ /g, ''));
  expect(readDerS(signature)).toBeLessThanOrEqual(halfOrder);
});

test('refresh signs the exact canonical JSON without upload-only headers', async () => {
  let request;
  const upload = {
    upload_handle: `upl_${'r'.repeat(43)}`,
    upload_request_id: '123e4567-e89b-42d3-a456-426614174000',
  };
  mockOpener.fetch.mockImplementation(async builder => {
    request = builder(new URL('https://private.test/internal/v1/uploads'));
    return jsonResponse(200, { data: {
      upload_handle: upload.upload_handle,
      mint_capability: 'qmc1.refreshed',
      mint_capability_expires_at: '2026-09-06T21:15:00Z',
      authority_expires_at: '2026-09-06T22:00:00Z',
    } });
  });

  const result = await privateUpload.refreshPrivateUpload(upload, {
    maxBatchSize: 1,
    maxLinkTtlSeconds: 3600,
    authorityExpiresAt: '2026-09-06T22:00:00Z',
    requestId: '018f3f5a-7b6c-4d2e-8a10-112233445566',
  });

  expect(request.method).toBe('PATCH');
  expect(request.headers['Content-Type']).toBe('application/json');
  expect(request.headers['X-LayerV-Upload-Request-ID']).toBe('018f3f5a-7b6c-4d2e-8a10-112233445566');
  expect(request.headers['X-LayerV-Audience-Key-ID']).toBeUndefined();
  expect(request.headers['X-LayerV-Filename-B64']).toBeUndefined();
  expect(request.headers['X-LayerV-Viewer-TTL-Seconds']).toBeUndefined();
  expect(request.body.toString()).toBe(JSON.stringify({
    upload_handle: upload.upload_handle,
    upload_request_id: '018f3f5a-7b6c-4d2e-8a10-112233445566',
    max_batch_size: 1,
    max_link_ttl_seconds: 3600,
    authority_expires_at: '2026-09-06T22:00:00Z',
  }));
  expect(result).toEqual(expect.objectContaining({
    upload_handle: upload.upload_handle,
    mint_capability: 'qmc1.refreshed',
    upload_request_id: upload.upload_request_id,
  }));
});

test('upload ambiguity retries the same request ID with fresh transport authentication', async () => {
  const seen = [];
  mockOpener.fetch
    .mockImplementationOnce(async builder => {
      const request = builder(new URL('https://private.test:48123/internal/v1/uploads'));
      seen.push(request.headers);
      return jsonResponse(503, { error: { code: 'mutation_outcome_unknown', retryable: true } });
    })
    .mockImplementationOnce(async builder => {
      const request = builder(new URL('https://private.test:48123/internal/v1/uploads'));
      seen.push(request.headers);
      return jsonResponse(201, { data: {
        upload_handle: `upl_${'a'.repeat(43)}`,
        mint_capability: 'qmc1.test',
        mint_capability_expires_at: '2026-09-06T21:15:00Z',
        authority_expires_at: '2026-09-06T22:00:00Z',
      } });
    });

  const result = await privateUpload.uploadPrivate(Buffer.from('hello'), {
    filename: 'report.txt',
    contentType: 'text/plain',
    credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
    viewerTtlSeconds: 30,
    authorityExpiresAt: '2026-09-06T22:00:00Z',
    requestId: UPLOAD_VECTOR.requestId,
  });

  expect(result.upload_request_id).toBe(UPLOAD_VECTOR.requestId);
  expect(seen).toHaveLength(2);
  expect(seen[0]['X-LayerV-Upload-Request-ID']).toBe(seen[1]['X-LayerV-Upload-Request-ID']);
  expect(seen[0]['X-LayerV-Nonce']).not.toBe(seen[1]['X-LayerV-Nonce']);
  expect(seen[0]['X-LayerV-Viewer-TTL-Seconds']).toBe('30');
});

test('upload to delegated batch yields input-ordered links ready for Discord DMs', async () => {
  mockOpener.fetch.mockImplementation(async builder => {
    builder(new URL('https://private.test/internal/v1/uploads'));
    return jsonResponse(201, { data: {
      upload_handle: `upl_${'b'.repeat(43)}`,
      mint_capability: 'qmc1.test',
      mint_capability_expires_at: '2026-09-06T21:15:00Z',
      authority_expires_at: '2026-09-06T22:00:00Z',
    } });
  });
  const realFetch = global.fetch;
  const postBodies = [];
  global.fetch = jest.fn()
    .mockImplementationOnce(async (_url, init) => {
      postBodies.push(init.body);
      return jsonResponse(503, { error: { code: 'mutation_outcome_unknown' } });
    })
    .mockImplementationOnce(async (_url, init) => {
      postBodies.push(init.body);
      return jsonResponse(202, { data: {
        batch_id: `dqb_${'a'.repeat(22)}`,
        status: 'queued',
        item_count: 2,
        submitted_at: '2026-09-06T21:00:00Z',
      } }, {
        Location: `https://api.test.local/v1/delegated-qurl-batches/dqb_${'a'.repeat(22)}`,
        ETag: '"queued"',
        'Retry-After': '2',
      });
    })
    .mockImplementationOnce(async () => jsonResponse(200, { data: {
      batch_id: `dqb_${'a'.repeat(22)}`,
      status: 'succeeded',
      item_count: 2,
      submitted_at: '2026-09-06T21:00:00Z',
      results: [
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/a', expires_at: '2026-09-06T22:00:00Z' } },
        { index: 1, status: 'succeeded', qurl: { qurl_id: 'q_00000000002', qurl_link: 'https://qurl.site/b', expires_at: '2026-09-06T22:00:00Z' } },
      ],
    } }, { ETag: '"complete"' }));
  try {
    const credential = { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' };
    const upload = await privateUpload.uploadPrivate(Buffer.from('hello'), {
      filename: 'report.txt', contentType: 'text/plain', credential,
      viewerTtlSeconds: 30,
      authorityExpiresAt: '2026-09-06T22:00:00Z',
    });
    const links = await privateUpload.redeemDelegatedBatch(upload, {
      credential,
      grants: [{ expires_in: '1h', one_time_use: true }, { expires_in: '1h', one_time_use: true }],
      idempotencyKey: '123e4567-e89b-42d3-a456-426614174000',
      sleep: jest.fn(),
    });
    const sendDM = jest.fn(async (recipientId, qurlLink) => ({ recipientId, qurlLink, ok: true }));
    const recipients = ['discord-user-1', 'discord-user-2'];
    const deliveries = await Promise.all(links.map((link, index) => sendDM(recipients[index], link.qurl_link)));

    expect(postBodies[0]).toBe(postBodies[1]);
    expect(links.map(link => link.resource_id)).toEqual(['q_00000000001', 'q_00000000002']);
    expect(deliveries).toEqual([
      { recipientId: 'discord-user-1', qurlLink: 'https://qurl.site/a', ok: true },
      { recipientId: 'discord-user-2', qurlLink: 'https://qurl.site/b', ok: true },
    ]);
  } finally {
    global.fetch = realFetch;
  }
});
