const fs = require('fs');
const path = require('path');

const metadata = require('../discord-metadata.json');
const {
  assertExpectedApplication,
  dataUri,
  detectImageDimensions,
  errorDetails,
  main,
  PreflightVerificationError,
  PortalActionRequiredError,
  request,
  summarize,
  validateMetadata,
  validateImageRule,
} = require('../scripts/apply-discord-metadata');

function jsonResponse(body, { status = 200, headers = {} } = {}) {
  const normalizedHeaders = Object.fromEntries(
    Object.entries(headers).map(([key, value]) => [key.toLowerCase(), value]),
  );
  return {
    ok: status >= 200 && status < 300,
    status,
    text: jest.fn().mockResolvedValue(JSON.stringify(body)),
    headers: {
      get: (name) => normalizedHeaders[name.toLowerCase()] ?? null,
    },
  };
}

function fetchSequence(...responses) {
  const fetchImpl = jest.fn();
  for (const response of responses) {
    fetchImpl.mockResolvedValueOnce(response);
  }
  return fetchImpl;
}

function appResponse(name = metadata.application.name) {
  return {
    id: metadata.application.id,
    verify_key: metadata.application.public_key,
    name,
    icon: 'icon-hash',
    cover_image: 'cover-hash',
    description: metadata.application.description,
  };
}

function quietLogger() {
  return {
    log: jest.fn(),
    warn: jest.fn(),
  };
}

const tmpMismatchAsset = path.join(__dirname, '.tmp-discord-metadata-mismatch.png');

function cleanupTmpMismatchAsset() {
  if (fs.existsSync(tmpMismatchAsset)) fs.unlinkSync(tmpMismatchAsset);
}

describe('apply-discord-metadata helpers', () => {
  afterEach(cleanupTmpMismatchAsset);
  afterAll(cleanupTmpMismatchAsset);

  test('accepts the LayerV-owned Discord application identity', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      verify_key: metadata.application.public_key,
    }, metadata)).not.toThrow();
  });

  test('accepts Discord application identity with a public_key field', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      public_key: metadata.application.public_key,
    }, metadata)).not.toThrow();
  });

  test('rejects a bot token from the retired personal app before writes', () => {
    expect(() => assertExpectedApplication({
      id: '1495050474414411948',
      verify_key: metadata.application.public_key,
    }, metadata)).toThrow(/belongs to application 1495050474414411948/);
  });

  test('rejects a matching app id with the wrong public key', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      verify_key: '0'.repeat(64),
    }, metadata)).toThrow(/has public key/);
  });

  test('rejects a matching app id when Discord omits the public key', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
    }, metadata)).toThrow(/did not include a public key/);
  });

  test('validates required metadata identity fields', () => {
    expect(() => validateMetadata(metadata)).not.toThrow();
    expect(() => validateMetadata({ ...metadata, bot: { ...metadata.bot, username: '' } })).toThrow(/bot\.username/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, id: '' },
    })).toThrow(/application\.id/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, name: '' },
    })).toThrow(/application\.name/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, description: '' },
    })).toThrow(/application\.description/);
    expect(() => validateMetadata({
      ...metadata,
      bot: { ...metadata.bot, avatar: '' },
    })).toThrow(/bot\.avatar/);
    expect(() => validateMetadata({
      ...metadata,
      bot: { ...metadata.bot, banner: '' },
    })).toThrow(/bot\.banner/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, icon: '' },
    })).toThrow(/application\.icon/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, cover_image: '' },
    })).toThrow(/application\.cover_image/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, public_key: 'not-a-public-key' },
    })).toThrow(/application\.public_key/);
    expect(() => validateMetadata({
      ...metadata,
      application: {
        ...metadata.application,
        install_params: { ...metadata.application.install_params, scopes: ['bot'] },
      },
    })).toThrow(/install_params\.scopes/);
    expect(() => validateMetadata({
      ...metadata,
      application: {
        ...metadata.application,
        install_params: { ...metadata.application.install_params, permissions: 'not-a-number' },
      },
    })).toThrow(/install_params\.permissions/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, tags: [] },
    })).toThrow(/application\.tags/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, tags: ['a', 'b', 'c', 'd', 'e', 'f'] },
    })).toThrow(/application\.tags/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, tags: ['security', ''] },
    })).toThrow(/application\.tags/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, terms_of_service_url: 'http://layerv.ai/terms' },
    })).toThrow(/application\.terms_of_service_url/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, privacy_policy_url: 'not-a-url' },
    })).toThrow(/application\.privacy_policy_url/);
  });

  test('renders referenced PNG assets as data URIs', () => {
    const uri = dataUri(metadata.application.icon, 'application.icon');
    expect(uri).toMatch(/^data:image\/png;base64,/);
  });

  test('reads PNG dimensions only from the leading IHDR chunk', () => {
    const png = Buffer.from([
      0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
      0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
      0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x10,
    ]);
    const nonIhdr = Buffer.from(png);
    nonIhdr.write('tEXt', 12, 'ascii');

    expect(detectImageDimensions(png, 'image/png')).toEqual({ width: 32, height: 16 });
    expect(detectImageDimensions(nonIhdr, 'image/png')).toBeUndefined();
  });

  test('rejects referenced assets with the wrong local dimensions', () => {
    expect(() => dataUri(metadata.application.cover_image, 'application.icon')).toThrow(/expected approximately 1:1/);
    expect(() => dataUri(metadata.application.icon, 'application.cover_image')).toThrow(/expected approximately 16:9/);
  });

  test('reads JPEG dimensions after standalone markers', () => {
    const jpeg = Buffer.from([
      0xff, 0xd8,
      0xff, 0xd0,
      0xff, 0xc0, 0x00, 0x11, 0x08, 0x00, 0x64, 0x00, 0xc8,
      0x03, 0x01, 0x00, 0x02, 0x11, 0x00, 0x03, 0x11, 0x00,
      0x00,
    ]);

    expect(detectImageDimensions(jpeg, 'image/jpeg')).toEqual({ width: 200, height: 100 });
  });

  test('reads JPEG dimensions from a minimal tail SOF segment', () => {
    const jpeg = Buffer.from([
      0xff, 0xd8,
      0xff, 0xc0, 0x00, 0x07, 0x08, 0x00, 0x64, 0x00, 0xc8,
    ]);

    expect(detectImageDimensions(jpeg, 'image/jpeg')).toEqual({ width: 200, height: 100 });
  });

  test('ignores truncated JPEG SOF segments', () => {
    const jpeg = Buffer.from([
      0xff, 0xd8,
      0xff, 0xc0, 0x00, 0x06, 0x08, 0x00, 0x64, 0x00,
      0xff, 0xd9, 0x00,
    ]);

    expect(detectImageDimensions(jpeg, 'image/jpeg')).toBeUndefined();
  });

  test('rejects referenced assets over the local byte limit', () => {
    const bytes = fs.readFileSync(path.join(__dirname, '..', metadata.application.icon));

    expect(() => validateImageRule(metadata.application.icon, bytes, 'image/png', {
      maxBytes: 1,
      minWidth: 1,
      minHeight: 1,
    })).toThrow(/max is 1 bytes/);
  });

  test('fails with a guided error when an asset is missing', () => {
    expect(() => dataUri('assets/does-not-exist.png')).toThrow(/does not exist/);
  });

  test('fails with a guided error for unsupported asset types', () => {
    expect(() => dataUri('discord-metadata.json')).toThrow(/unsupported image extension \.json/);
  });

  test('fails when asset extension does not match image bytes', () => {
    fs.writeFileSync(tmpMismatchAsset, Buffer.from([0xff, 0xd8, 0xff, 0xd9]));

    expect(() => dataUri('tests/.tmp-discord-metadata-mismatch.png'))
      .toThrow(/extension \.png does not match detected image\/jpeg/);
  });

  test('redacts image data in dry-run summaries', () => {
    expect(summarize({
      avatar: 'data:image/png;base64,abc123',
      nested: { keep: 'qURL' },
      array: ['data:image/png;base64,def456'],
    })).toEqual({
      avatar: '<image-data>',
      nested: { keep: 'qURL' },
      array: ['<image-data>'],
    });
  });

  test('surfaces Discord retry-after values in warnings', () => {
    expect(errorDetails({
      status: 429,
      retryAfter: '12.5',
      body: { message: 'You are being rate limited.' },
    })).toContain('retry_after=12.5s');
  });

  test('normalizes Discord retry-after body values', async () => {
    await expect(request('PATCH', '/users/@me', {}, {
      token: 'test-token',
      fetchImpl: jest.fn().mockResolvedValue(jsonResponse({
        message: 'You are being rate limited.',
        retry_after: 12.5,
      }, { status: 429 })),
    })).rejects.toMatchObject({ retryAfter: '12.5' });
  });

  test('preserves zero retry-after headers', async () => {
    await expect(request('PATCH', '/users/@me', {}, {
      token: 'test-token',
      fetchImpl: jest.fn().mockResolvedValue(jsonResponse({
        message: 'You are being rate limited.',
        retry_after: 12.5,
      }, { status: 429, headers: { 'retry-after': '0' } })),
    })).rejects.toMatchObject({ retryAfter: '0' });
  });

  test('falls back to retry-after body values for empty headers', async () => {
    await expect(request('PATCH', '/users/@me', {}, {
      token: 'test-token',
      fetchImpl: jest.fn().mockResolvedValue(jsonResponse({
        message: 'You are being rate limited.',
        retry_after: 12.5,
      }, { status: 429, headers: { 'retry-after': '' } })),
    })).rejects.toMatchObject({ retryAfter: '12.5' });
  });

  test('passes an abort signal to Discord requests', async () => {
    const fetchImpl = jest.fn().mockResolvedValue(jsonResponse({ username: metadata.bot.username }));

    await expect(request('GET', '/users/@me', undefined, {
      token: 'test-token',
      fetchImpl,
    })).resolves.toEqual({ username: metadata.bot.username });

    expect(fetchImpl.mock.calls[0][1].signal).toBeInstanceOf(AbortSignal);
  });

  test('converts aborted Discord requests into timeout errors', async () => {
    const abortErr = new Error('aborted');
    abortErr.name = 'AbortError';

    await expect(request('GET', '/users/@me', undefined, {
      token: 'test-token',
      timeoutMs: 1,
      fetchImpl: jest.fn().mockRejectedValue(abortErr),
    })).rejects.toMatchObject({
      status: 'timeout',
      message: 'GET /users/@me timed out after 1ms',
    });
  });

  test('omits the JSON content-type header on GET requests', async () => {
    const fetchImpl = jest.fn().mockResolvedValue(jsonResponse({ username: metadata.bot.username }));

    await expect(request('GET', '/users/@me', undefined, {
      token: 'test-token',
      fetchImpl,
    })).resolves.toEqual({ username: metadata.bot.username });

    expect(fetchImpl.mock.calls[0][1].headers).toEqual({
      Authorization: 'Bot test-token',
    });
  });

  test('marks portal-only drift with a distinct exit code', () => {
    expect(new PortalActionRequiredError('portal step pending').exitCode).toBe(2);
  });

  test('marks pre-flight verification failures with a distinct exit code', () => {
    expect(new PreflightVerificationError('bad token').exitCode).toBe(4);
  });

  test('main returns the portal-action exit code when only the app name drifts', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse('Qurl Bot')),
      jsonResponse({ username: metadata.bot.username, discriminator: '0' }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse('Qurl Bot')),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({ exitCode: 2 });
    expect(fetchImpl).toHaveBeenCalledTimes(4);
  });

  test('main resolves on a fully applied happy path', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username, discriminator: '0' }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(logger.log).toHaveBeenCalledWith(`Bot username already ${metadata.bot.username}.`);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('main resolves after successfully patching the bot username', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(5);
    expect(JSON.parse(fetchImpl.mock.calls[2][1].body)).toEqual({ username: metadata.bot.username });
    expect(JSON.parse(fetchImpl.mock.calls[3][1].body)).toEqual({ avatar: expect.stringMatching(/^data:image\//), banner: expect.stringMatching(/^data:image\//) });
    expect(JSON.parse(fetchImpl.mock.calls[4][1].body)).toMatchObject({ icon: expect.stringMatching(/^data:image\//), cover_image: expect.stringMatching(/^data:image\//) });
    expect(logger.log).toHaveBeenCalledWith(`Updated bot username: ${metadata.bot.username}`);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('main treats the exact configured bot username as applied', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username, discriminator: '0' }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(logger.log).toHaveBeenCalledWith(expect.stringMatching(/Bot username already/));
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('main accepts an exact bot name without a discriminator', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(logger.log).toHaveBeenCalledWith(expect.stringMatching(/Bot username already/));
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test.each(['qurl', 'QURL', 'Old Bot'])('patches a different bot name %s to the exact configured name', async username => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()), jsonResponse({ username, discriminator: '1234' }),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }), jsonResponse(appResponse()),
    );
    await main({ token: 'test-token', fetchImpl, logger });
    expect(fetchImpl).toHaveBeenCalledTimes(5);
    expect(JSON.parse(fetchImpl.mock.calls[2][1].body)).toEqual({ username: metadata.bot.username });
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('a successful PATCH that does not return the configured name stays a partial failure', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()), jsonResponse({ username: 'Old Bot' }), jsonResponse({ username: 'Still Old' }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }), jsonResponse(appResponse()),
    );
    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() })).rejects.toThrow(/completed with skipped fields/);
  });

  test('main fails clearly when the current bot user omits username', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ id: 'bot-user-id' }),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({
        exitCode: 4,
        message: expect.stringMatching(/did not include username/),
      });
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  test('main marks failed application identity fetches as pre-flight failures', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse({ message: '401: Unauthorized' }, { status: 401 }),
    );

    await expect(main({ token: 'bad-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({
        exitCode: 4,
        status: 401,
        message: expect.stringMatching(/application pre-flight verification failed/),
      });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  test('main marks wrong-app identity responses as pre-flight failures before writes', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse({
        ...appResponse(),
        id: '1495050474414411948',
      }),
    );

    await expect(main({ token: 'wrong-app-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({
        exitCode: 4,
        message: expect.stringMatching(/application pre-flight verification failed/),
      });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  test('main dry-run emits a summary without a token or network calls', async () => {
    const logger = quietLogger();
    const fetchImpl = jest.fn();

    await expect(main({ dryRun: true, token: '', fetchImpl, logger })).resolves.toBeUndefined();

    expect(fetchImpl).not.toHaveBeenCalled();
    expect(logger.log).toHaveBeenCalledTimes(1);
    expect(JSON.parse(logger.log.mock.calls[0][0]).expected_application).toEqual({
      id: metadata.application.id,
      public_key: metadata.application.public_key,
    });
  });

  test('main requires a token for live applies', async () => {
    await expect(main({ dryRun: false, token: '', fetchImpl: jest.fn(), logger: quietLogger() }))
      .rejects.toThrow(/DISCORD_TOKEN is required/);
  });

  test('main treats the application PATCH as fatal after bot identity writes', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({
        exitCode: 3,
        message: expect.stringMatching(/Discord application metadata update failed: PATCH \/applications\/@me failed with 429/),
      });
    expect(fetchImpl).toHaveBeenCalledTimes(5);
  });

  test('main treats a username 429 as a partial apply failure', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toThrow(/completed with skipped fields/);
    expect(fetchImpl).toHaveBeenCalledTimes(5);
  });

  test('main treats a bot image 429 as a partial apply failure', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toThrow(/completed with skipped fields/);
    expect(fetchImpl).toHaveBeenCalledTimes(4);
  });

  test('main treats a missing bot banner response hash as a partial apply failure', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger }))
      .rejects.toThrow(/completed with skipped fields/);
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(logger.warn).toHaveBeenCalledWith(expect.stringMatching(/Bot banner update skipped/));
  });

  test('main preserves bot partial-failure context when the app PATCH also fails', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({
        exitCode: 3,
        message: expect.stringMatching(/bot identity fields were also skipped earlier/),
      });
    expect(fetchImpl).toHaveBeenCalledTimes(5);
  });

  test('main preserves the portal action message when a partial failure also happens', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse('Qurl Bot')),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse('Qurl Bot')),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toThrow(/Developer Portal action is also required/);
    expect(fetchImpl).toHaveBeenCalledTimes(5);
  });
});


describe('metadata apply image upload policy', () => {
  test('skip-images retains application 429 failure and retry-after reporting', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username, discriminator: '0' }),
      jsonResponse({ message: 'rate limited', retry_after: 12.5 }, { status: 429 }),
    );
    await expect(main({ skipImages: true, token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toMatchObject({ exitCode: 3, status: 429, retryAfter: '12.5' });
    expect(fetchImpl).toHaveBeenCalledTimes(3);
  });

  test('explicit skip-images preserves all four images while applying text metadata', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username, discriminator: '0' }),
      jsonResponse(appResponse()),
    );
    await main({ skipImages: true, token: 'test-token', fetchImpl, logger });
    expect(fetchImpl).toHaveBeenCalledTimes(3);
    const writes = fetchImpl.mock.calls.filter(([, options]) => options.method === 'PATCH');
    expect(writes).toHaveLength(1);
    const body = JSON.parse(writes[0][1].body);
    expect(body).toMatchObject({ description: metadata.application.description, tags: metadata.application.tags });
    for (const key of ['avatar', 'banner', 'icon', 'cover_image']) expect(body).not.toHaveProperty(key);
    expect(logger.log).toHaveBeenCalledWith(expect.stringContaining('Images preserved (--skip-images)'));
  });

  test('skip-images dry-run omits image data and clearly reports the operator choice', async () => {
    const logger = quietLogger();
    const fetchImpl = jest.fn();
    await main({ dryRun: true, skipImages: true, fetchImpl, logger });
    const plan = JSON.parse(logger.log.mock.calls[0][0]);
    expect(plan.bot_images).toEqual({});
    expect(plan.application).not.toHaveProperty('icon');
    expect(plan.application).not.toHaveProperty('cover_image');
    expect(plan.skip_images).toBe(true);
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});


describe('metadata apply operator safety', () => {
  test.each(['--skip-image', '--skipImages', '--dryrun', 'unexpected'])('rejects unknown CLI argument %s before token lookup or network', arg => {
    const { spawnSync } = require('node:child_process');
    const result = spawnSync(process.execPath, [path.join(__dirname, '../scripts/apply-discord-metadata.js'), arg], {
      env: { ...process.env, DISCORD_TOKEN: '' }, encoding: 'utf8', timeout: 5000,
    });
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(1);
    expect(result.stderr).toMatch(/Unknown option|Unexpected argument/);
    expect(result.stderr).not.toContain('DISCORD_TOKEN is required');
  });

  test('skip-images does not read any local asset files', async () => {
    await main({ dryRun: true, skipImages: true, logger: quietLogger() });
    const read = jest.spyOn(fs, 'readFileSync');
    const exists = jest.spyOn(fs, 'existsSync');
    try {
      await main({ dryRun: true, skipImages: true, logger: quietLogger() });
      expect(read).not.toHaveBeenCalled();
      expect(exists).not.toHaveBeenCalled();
    } finally { read.mockRestore(); exists.mockRestore(); }
  });

  test('skip-images still updates a changed username without image fields', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()), jsonResponse({ username: 'Old Bot' }),
      jsonResponse({ username: metadata.bot.username }), jsonResponse(appResponse()),
    );
    await main({ skipImages: true, token: 'test-token', fetchImpl, logger: quietLogger() });
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(JSON.parse(fetchImpl.mock.calls[2][1].body)).toEqual({ username: metadata.bot.username });
    const appBody = JSON.parse(fetchImpl.mock.calls[3][1].body);
    expect(appBody).not.toHaveProperty('icon');
    expect(appBody).not.toHaveProperty('cover_image');
  });
});


describe('bot username convergence', () => {
  test.each(['1234', '0', undefined])('an exact configured bot name converges regardless of discriminator %s', async discriminator => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()), jsonResponse({ username: metadata.bot.username, discriminator }), jsonResponse(appResponse()),
    );
    await main({ skipImages: true, token: 'test-token', fetchImpl, logger: quietLogger() });
    expect(fetchImpl).toHaveBeenCalledTimes(3);
    expect(fetchImpl.mock.calls.filter(([, options]) => options.method === 'PATCH')).toHaveLength(1);
  });
});
