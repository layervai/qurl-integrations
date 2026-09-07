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
  QURL_LINK_DOMAIN: 'qurl.site',
};

jest.mock('@layervai/qurl/node', () => ({ createPortalOpener: mockCreatePortalOpener }));
jest.mock('../src/config', () => mockConfig);
jest.mock('../src/logger', () => ({ warn: jest.fn() }));

const privateUpload = require('../src/private-upload');
const {
  canonicalUploadMessage,
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

test('upload canonical bytes match the private-upload v1 vector', () => {
  expect(canonicalUploadMessage(UPLOAD_VECTOR).toString('hex')).toBe(UPLOAD_CANONICAL_HEX);
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

test('upload ambiguity respects Retry-After and accepts an exact 200 replay', async () => {
  const seen = [];
  const sleep = jest.fn();
  mockOpener.fetch
    .mockImplementationOnce(async builder => {
      const request = builder(new URL('https://private.test:48123/internal/v1/uploads'));
      seen.push(request.headers);
      return jsonResponse(503, { error: { code: 'mutation_outcome_unknown', retryable: true } }, {
        'Retry-After': '1',
      });
    })
    .mockImplementationOnce(async builder => {
      const request = builder(new URL('https://private.test:48123/internal/v1/uploads'));
      seen.push(request.headers);
      return jsonResponse(200, { data: {
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
    deadlineMs: Date.now() + 60_000,
    requestId: UPLOAD_VECTOR.requestId,
    sleep,
  });

  expect(result.upload_request_id).toBe(UPLOAD_VECTOR.requestId);
  expect(seen).toHaveLength(2);
  expect(seen[0]['X-LayerV-Upload-Request-ID']).toBe(seen[1]['X-LayerV-Upload-Request-ID']);
  expect(seen[0]['X-LayerV-Nonce']).not.toBe(seen[1]['X-LayerV-Nonce']);
  expect(seen[0]['X-LayerV-Viewer-TTL-Seconds']).toBe('30');
  expect(sleep).toHaveBeenCalledTimes(1);
  expect(sleep).toHaveBeenCalledWith(1000);
});

test('upload rejects Retry-After beyond the shared send deadline without retaining the body', async () => {
  const sleep = jest.fn();
  mockOpener.fetch.mockResolvedValue(jsonResponse(
    503,
    { error: { code: 'mutation_outcome_unknown', retryable: true } },
    { 'Retry-After': '100000' },
  ));

  await expect(privateUpload.uploadPrivate(Buffer.alloc(25 * 1024 * 1024), {
    filename: 'report.bin',
    contentType: 'application/octet-stream',
    credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
    viewerTtlSeconds: 30,
    authorityExpiresAt: '2026-09-06T22:00:00Z',
    deadlineMs: Date.now() + 60_000,
    requestId: UPLOAD_VECTOR.requestId,
    sleep,
  })).rejects.toThrow(/interaction deadline/);

  expect(mockOpener.fetch).toHaveBeenCalledTimes(1);
  expect(sleep).not.toHaveBeenCalled();
});

test('upload to delegated batch honors retry timing and accepts terminal 200 without ETag', async () => {
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
      return jsonResponse(503, { error: { code: 'mutation_outcome_unknown' } }, {
        'Retry-After': '1',
      });
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
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/#at_a', expires_at: '2026-09-06T22:00:00Z' } },
        { index: 1, status: 'succeeded', qurl: { qurl_id: 'q_00000000002', qurl_link: 'https://qurl.site/#at_b', expires_at: '2026-09-06T22:00:00Z' } },
      ],
    } }));
  try {
    const credential = { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' };
    const upload = await privateUpload.uploadPrivate(Buffer.from('hello'), {
      filename: 'report.txt', contentType: 'text/plain', credential,
      viewerTtlSeconds: 30,
      authorityExpiresAt: '2026-09-06T22:00:00Z',
      deadlineMs: Date.now() + 60_000,
    });
    const sleep = jest.fn();
    const links = await privateUpload.redeemDelegatedBatch(upload, {
      credential,
      grants: [{ expires_in: '1h', one_time_use: true }, { expires_in: '1h', one_time_use: true }],
      idempotencyKey: '123e4567-e89b-42d3-a456-426614174000',
      sleep,
    });
    const sendDM = jest.fn(async (recipientId, qurlLink) => ({ recipientId, qurlLink, ok: true }));
    const recipients = ['discord-user-1', 'discord-user-2'];
    const deliveries = await Promise.all(links.map((link, index) => sendDM(recipients[index], link.qurl_link)));

    expect(postBodies[0]).toBe(postBodies[1]);
    expect(sleep.mock.calls).toEqual([[1000], [2000]]);
    expect(links.map(link => link.resource_id)).toEqual(['q_00000000001', 'q_00000000002']);
    expect(deliveries).toEqual([
      { recipientId: 'discord-user-1', qurlLink: 'https://qurl.site/#at_a', ok: true },
      { recipientId: 'discord-user-2', qurlLink: 'https://qurl.site/#at_b', ok: true },
    ]);
  } finally {
    global.fetch = realFetch;
  }
});

test('delegated batch POST and poll requests cannot outlive the shared send deadline', async () => {
  const batchId = `dqb_${'e'.repeat(22)}`;
  const realFetch = global.fetch;
  let now = 1_000;
  const dateNow = jest.spyOn(Date, 'now').mockImplementation(() => now);
  const timeout = jest.spyOn(AbortSignal, 'timeout');
  global.fetch = jest.fn()
    .mockResolvedValueOnce(jsonResponse(202, { data: {
      batch_id: batchId,
      status: 'queued',
      item_count: 1,
      submitted_at: '2026-09-06T21:00:00Z',
    } }, {
      Location: `https://api.test.local/v1/delegated-qurl-batches/${batchId}`,
      ETag: '"queued"',
      'Retry-After': '1',
    }))
    .mockResolvedValueOnce(jsonResponse(200, { data: {
      batch_id: batchId,
      status: 'succeeded',
      item_count: 1,
      submitted_at: '2026-09-06T21:00:00Z',
      results: [
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/#at_a', expires_at: '2026-09-06T22:00:00Z' } },
      ],
    } }));
  try {
    await privateUpload.redeemDelegatedBatch(
      { mint_capability: 'qmc1.test' },
      {
        credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
        grants: [{ one_time_use: true }],
        deadlineMs: 3_500,
        sleep: jest.fn(async (ms) => { now += ms; }),
      },
    );

    expect(timeout.mock.calls.map(([ms]) => ms)).toEqual([2_500, 1_500]);
  } finally {
    timeout.mockRestore();
    dateNow.mockRestore();
    global.fetch = realFetch;
  }
});

test('delegated batch rejects an unexpected successful POST without retrying', async () => {
  const realFetch = global.fetch;
  const sleep = jest.fn();
  global.fetch = jest.fn(async () => jsonResponse(200, { data: { status: 'succeeded' } }));
  try {
    await expect(privateUpload.redeemDelegatedBatch(
      { mint_capability: 'qmc1.test' },
      {
        credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
        grants: [{ expires_in: '1h', one_time_use: true }],
        idempotencyKey: '123e4567-e89b-42d3-a456-426614174000',
        sleep,
      },
    )).rejects.toThrow(/unexpected success status \(200\)/);
    expect(global.fetch).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  } finally {
    global.fetch = realFetch;
  }
});

test('delegated batch preserves successful qURL IDs when a terminal item fails', async () => {
  const batchId = `dqb_${'b'.repeat(22)}`;
  const realFetch = global.fetch;
  global.fetch = jest.fn()
    .mockResolvedValueOnce(jsonResponse(202, { data: {
      batch_id: batchId,
      status: 'queued',
      item_count: 2,
      submitted_at: '2026-09-06T21:00:00Z',
    } }, {
      Location: `https://api.test.local/v1/delegated-qurl-batches/${batchId}`,
      ETag: '"queued"',
      'Retry-After': '1',
    }))
    .mockResolvedValueOnce(jsonResponse(200, { data: {
      batch_id: batchId,
      status: 'partially_failed',
      item_count: 2,
      submitted_at: '2026-09-06T21:00:00Z',
      results: [
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/#at_a', expires_at: '2026-09-06T22:00:00Z' } },
        { index: 1, status: 'failed', error: { code: 'creation_failed', message: 'failed' } },
      ],
    } }));
  try {
    const error = await privateUpload.redeemDelegatedBatch(
      { mint_capability: 'qmc1.test' },
      {
        credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
        grants: [{ one_time_use: true }, { one_time_use: true }],
        deadlineMs: Date.now() + 60_000,
        sleep: jest.fn(),
      },
    ).then(() => null, err => err);

    expect(error).toEqual(expect.any(Error));
    expect(error.partialQurlIds).toEqual(['q_00000000001']);
    expect(error.partialLinkCount).toBe(1);
  } finally {
    global.fetch = realFetch;
  }
});

test.each([
  ['a foreign origin', 'https://phishing.example/#at_secret'],
  ['a non-root path', 'https://qurl.site/share#at_secret'],
  ['a query', 'https://qurl.site/?next=bad#at_secret'],
  ['no bearer fragment', 'https://qurl.site/'],
])('delegated batch rejects a share link with %s', async (_case, qurlLink) => {
  const batchId = `dqb_${'c'.repeat(22)}`;
  const realFetch = global.fetch;
  global.fetch = jest.fn()
    .mockResolvedValueOnce(jsonResponse(202, { data: {
      batch_id: batchId,
      status: 'queued',
      item_count: 1,
      submitted_at: '2026-09-06T21:00:00Z',
    } }, {
      Location: `https://api.test.local/v1/delegated-qurl-batches/${batchId}`,
      ETag: '"queued"',
      'Retry-After': '1',
    }))
    .mockResolvedValueOnce(jsonResponse(200, { data: {
      batch_id: batchId,
      status: 'succeeded',
      item_count: 1,
      submitted_at: '2026-09-06T21:00:00Z',
      results: [
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: qurlLink, expires_at: '2026-09-06T22:00:00Z' } },
      ],
    } }));
  try {
    const error = await privateUpload.redeemDelegatedBatch(
      { mint_capability: 'qmc1.test' },
      {
        credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
        grants: [{ one_time_use: true }],
        deadlineMs: Date.now() + 60_000,
        sleep: jest.fn(),
      },
    ).then(() => null, err => err);

    expect(error).toEqual(expect.any(Error));
    expect(error.partialQurlIds).toEqual(['q_00000000001']);
  } finally {
    global.fetch = realFetch;
  }
});

test.each([
  {
    name: 'qurl_id',
    second: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/#at_b' },
    expectedIds: ['q_00000000001'],
  },
  {
    name: 'qurl_link',
    second: { qurl_id: 'q_00000000002', qurl_link: 'https://qurl.site/#at_a' },
    expectedIds: ['q_00000000001', 'q_00000000002'],
  },
])('delegated batch rejects a duplicate $name bearer grant', async ({ second, expectedIds }) => {
  const batchId = `dqb_${'d'.repeat(22)}`;
  const expiresAt = '2026-09-06T22:00:00Z';
  const realFetch = global.fetch;
  global.fetch = jest.fn()
    .mockResolvedValueOnce(jsonResponse(202, { data: {
      batch_id: batchId,
      status: 'queued',
      item_count: 2,
      submitted_at: '2026-09-06T21:00:00Z',
    } }, {
      Location: `https://api.test.local/v1/delegated-qurl-batches/${batchId}`,
      ETag: '"queued"',
      'Retry-After': '1',
    }))
    .mockResolvedValueOnce(jsonResponse(200, { data: {
      batch_id: batchId,
      status: 'succeeded',
      item_count: 2,
      submitted_at: '2026-09-06T21:00:00Z',
      results: [
        { index: 0, status: 'succeeded', qurl: { qurl_id: 'q_00000000001', qurl_link: 'https://qurl.site/#at_a', expires_at: expiresAt } },
        { index: 1, status: 'succeeded', qurl: { ...second, expires_at: expiresAt } },
      ],
    } }));
  try {
    const error = await privateUpload.redeemDelegatedBatch(
      { mint_capability: 'qmc1.test' },
      {
        credential: { apiKey: 'lv_test_example', keyId: 'key_A1b2C3d4E5f6' },
        grants: [{ one_time_use: true }, { one_time_use: true }],
        deadlineMs: Date.now() + 60_000,
        sleep: jest.fn(),
      },
    ).then(() => null, err => err);

    expect(error).toEqual(expect.any(Error));
    expect(error.message).toMatch(/duplicate bearer grant/);
    expect(error.partialQurlIds).toEqual(expectedIds);
  } finally {
    global.fetch = realFetch;
  }
});
