const fs = require('fs');
const path = require('path');

const metadata = require('../discord-metadata.json');
const {
  assertExpectedApplication,
  dataUri,
  errorDetails,
  main,
  PortalActionRequiredError,
  request,
  summarize,
  validateMetadata,
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
      get: (name) => normalizedHeaders[name.toLowerCase()] || null,
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

describe('apply-discord-metadata helpers', () => {
  test('accepts the LayerV-owned Discord application identity', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      verify_key: metadata.application.public_key,
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
  });

  test('renders referenced PNG assets as data URIs', () => {
    const uri = dataUri(metadata.application.icon);
    expect(uri).toMatch(/^data:image\/png;base64,/);
  });

  test('fails with a guided error when an asset is missing', () => {
    expect(() => dataUri('assets/does-not-exist.png')).toThrow(/does not exist/);
  });

  test('fails with a guided error for unsupported asset types', () => {
    expect(() => dataUri('discord-metadata.json')).toThrow(/unsupported image extension \.json/);
  });

  test('fails when asset extension does not match image bytes', () => {
    fs.writeFileSync(tmpMismatchAsset, Buffer.from([0xff, 0xd8, 0xff, 0xd9]));
    try {
      expect(() => dataUri('tests/.tmp-discord-metadata-mismatch.png'))
        .toThrow(/extension \.png does not match detected image\/jpeg/);
    } finally {
      fs.unlinkSync(tmpMismatchAsset);
    }
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

  test('marks portal-only drift with a distinct exit code', () => {
    expect(new PortalActionRequiredError('portal step pending').exitCode).toBe(2);
  });

  test('main returns the portal-action exit code when only the app name drifts', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse('Qurl Bot')),
      jsonResponse({ username: metadata.bot.username }),
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
      jsonResponse({ username: metadata.bot.username }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(4);
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
    expect(logger.log).toHaveBeenCalledWith(`Updated bot username: ${metadata.bot.username}`);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('main skips a case-only username update when Discord normalizes casing', async () => {
    const logger = quietLogger();
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: metadata.bot.username.toLowerCase() }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse(appResponse()),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger })).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(logger.warn).toHaveBeenCalledWith(expect.stringMatching(/normalize username casing/));
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
      .rejects.toThrow(/PATCH \/applications\/@me failed with 429/);
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

  test('main preserves bot partial-failure context when the app PATCH also fails', async () => {
    const fetchImpl = fetchSequence(
      jsonResponse(appResponse()),
      jsonResponse({ username: 'Qurl Bot' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
      jsonResponse({ avatar: 'avatar-hash', banner: 'banner-hash' }),
      jsonResponse({ message: 'rate limited', retry_after: '12.5' }, { status: 429 }),
    );

    await expect(main({ token: 'test-token', fetchImpl, logger: quietLogger() }))
      .rejects.toThrow(/bot identity fields were also skipped earlier/);
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
