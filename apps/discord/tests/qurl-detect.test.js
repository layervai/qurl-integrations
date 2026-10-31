const {
  DETECT_NO_MATCH_MSG,
  SENDER_ID,
  commands,
  detectCooldowns,
  handleQurlDetect,
  isOnDetectCooldown,
  logger,
  makeInteraction,
  mockDb,
  mockDetectWatermark,
} = require('./helpers/qurl-send-fixtures');

describe('handleQurlDetect', () => {
  const VALID_IMAGE = Object.freeze({
    url: 'https://cdn.discordapp.com/attachments/1/2/shot.png',
    name: 'shot.png',
    contentType: 'image/png',
    size: 2048,
  });

  function makeDetectInteraction({
    guildId = 'guild-1',
    userId = SENDER_ID,
    image = VALID_IMAGE,
    usersFetch,
    omitImage = false,
  } = {}) {
    const base = makeInteraction({ guildId, userId, options: { image } });
    if (omitImage) {
      base.options.getAttachment = jest.fn((name, required) => {
        if (required) throw new Error(`Required option "${name}" not found`);
        return null;
      });
    }
    base.deferReply = jest.fn(async () => { base.deferred = true; });
    base.client = {
      users: {
        fetch: usersFetch || jest.fn(async (id) => ({ id, username: `recip_${id.slice(-3)}` })),
      },
    };
    return base;
  }

  let originalFetch;
  beforeEach(() => {
    originalFetch = global.fetch;
    global.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      arrayBuffer: async () => new Uint8Array([1, 2, 3]).buffer,
    });
    mockDb.getGuildApiKey.mockResolvedValue(null); // → falls back to config.QURL_API_KEY
  });
  afterEach(() => {
    global.fetch = originalFetch;
  });

  test('rejects in DM context (no oracle outside a guild) — no cooldown, no connector call', async () => {
    const int = makeDetectInteraction({ guildId: null });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      flags: 64,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
  });

  test('rejects a non-image attachment (cooldown CLEARED — honest user error)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, contentType: 'application/pdf' } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/needs an image/i),
      flags: 64,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('rejects a non-Discord-CDN attachment url (SSRF gate — cooldown PRESERVED + rejected audit)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, url: 'https://evil.example/x.png' } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/i),
      flags: 64,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(true);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'rejected', guild_id: 'guild-1', requester_id: SENDER_ID,
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect('recipient_discord_id' in auditMeta).toBe(false);
  });

  test('per-(guild,user) cooldown blocks a second detect in the same guild', async () => {
    const int1 = makeDetectInteraction();
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    await handleQurlDetect(int1);
    expect(int1.deferReply).toHaveBeenCalled();

    const int2 = makeDetectInteraction();
    await handleQurlDetect(int2);
    expect(int2.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/wait before running detect/i),
      flags: 64,
    }));
    expect(int2.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).toHaveBeenCalledTimes(1);
  });

  test('cooldown is per-(guild,user): a different guild is NOT throttled', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    await handleQurlDetect(makeDetectInteraction({ guildId: 'guild-A' }));
    const intB = makeDetectInteraction({ guildId: 'guild-B' });
    await handleQurlDetect(intB);
    expect(intB.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).toHaveBeenCalledTimes(2);
  });

  test('(i) detected ⇒ replies (ephemerally) with the recipient handle + % match', async () => {
    const recipientId = '100000000000000007';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_hit1', match_pct: 92, confidence: 0.98,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_hit1', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const usersFetch = jest.fn(async (id) => ({ id, username: 'AliceRecipient' }));
    const int = makeDetectInteraction({ usersFetch });

    int.options.getSubcommand = () => 'detect';
    await commands.commands[0].execute(int);
    expect(int.deferReply).toHaveBeenCalledWith({ flags: 64 });

    expect(mockDetectWatermark).toHaveBeenCalledWith(
      expect.any(Buffer),
      expect.objectContaining({ guildId: 'guild-1', contentType: 'image/png' }),
    );
    expect(mockDb.findSendsByQurlId).toHaveBeenCalledWith('q_hit1');
    expect(usersFetch).toHaveBeenCalledWith(recipientId);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('AliceRecipient'),
    }));
    const matchedReply = int.editReply.mock.calls.at(-1)[0];
    const replyContent = matchedReply.content;
    expect(replyContent).toContain(`<@${recipientId}>`);
    expect(replyContent).toMatch(/92% match/);
    expect(replyContent).not.toMatch(/confidence/i);
    expect(matchedReply.allowedMentions).toEqual({ parse: [] });
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_hit1', match_pct: 92, guild_id: 'guild-1', confidence: 0.98, grant_basis: 'sender',
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect(JSON.stringify(auditMeta)).not.toContain(recipientId);
  });

  test('matched with a non-positive match_pct ⇒ bare attribution, no "0% match" (item 3)', async () => {
    const recipientId = '100000000000000011';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_zero', match_pct: 0, confidence: 0.9,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_zero', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const int = makeDetectInteraction({
      usersFetch: jest.fn(async (id) => ({ id, username: 'BobRecipient' })),
    });

    await handleQurlDetect(int);

    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toContain('BobRecipient');
    expect(replyContent).not.toMatch(/match/i); // no "% match" suffix
    expect(replyContent).not.toContain('0%');
  });

  test('audit keeps the RAW match_pct while the reply rounds it (item 2)', async () => {
    const recipientId = '100000000000000013';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_frac', match_pct: 92.7, confidence: 0.97,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_frac', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const int = makeDetectInteraction({
      usersFetch: jest.fn(async (id) => ({ id, username: 'CarolRecipient' })),
    });

    await handleQurlDetect(int);

    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toMatch(/93% match/);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_frac', match_pct: 92.7,
    }));
  });

  test('(ii) !detected ⇒ "no watermark found" ephemeral + no_match audit', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    const int = makeDetectInteraction();

    await handleQurlDetect(int);

    expect(mockDb.findSendsByQurlId).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: DETECT_NO_MATCH_MSG,
    }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_match', guild_id: 'guild-1',
    }));
  });

  test('(iii) cross-guild guard: a send whose guild_id ≠ interaction.guildId ⇒ defensive "no match"', async () => {
    const recipientId = '100000000000000009';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_otherguild', match_pct: 88, confidence: 0.91,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_otherguild', recipient_discord_id: recipientId, guild_id: 'guild-OTHER' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', usersFetch });

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: DETECT_NO_MATCH_MSG,
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_match', qurl_id: 'q_otherguild',
    }));
  });

  test('no-mark, cross-guild-filtered, and no-standing replies are ALL BYTE-IDENTICAL (no signal)', async () => {
    mockDetectWatermark.mockResolvedValueOnce({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    const noMark = makeDetectInteraction();
    await handleQurlDetect(noMark);
    const noMarkText = noMark.editReply.mock.calls.at(-1)[0].content;

    detectCooldowns.clear(); // second invocation in the same guild/user
    mockDetectWatermark.mockResolvedValueOnce({ detected: true, qurl_id: 'q_x', match_pct: 80, confidence: 0.9 });
    mockDb.findSendsByQurlId.mockResolvedValueOnce([
      { qurl_id: 'q_x', recipient_discord_id: '100000000000000003', guild_id: 'guild-OTHER' },
    ]);
    const crossGuild = makeDetectInteraction({ guildId: 'guild-1' });
    await handleQurlDetect(crossGuild);
    const crossGuildText = crossGuild.editReply.mock.calls.at(-1)[0].content;

    detectCooldowns.clear();
    mockDetectWatermark.mockResolvedValueOnce({ detected: true, qurl_id: 'q_ns', match_pct: 90, confidence: 0.95 });
    mockDb.findSendsByQurlId.mockResolvedValueOnce([
      { qurl_id: 'q_ns', recipient_discord_id: '100000000000000004', guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const noStanding = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000888' });
    await handleQurlDetect(noStanding);
    const noStandingText = noStanding.editReply.mock.calls.at(-1)[0].content;

    expect(noMarkText).toBe(crossGuildText);
    expect(noMarkText).toBe(noStandingText);
    expect(noMarkText).toBe(DETECT_NO_MATCH_MSG);
  });

  test('ambiguity guard: >1 same-guild row for one qurl_id ⇒ refuses to attribute', async () => {
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_dup', match_pct: 90, confidence: 0.95,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_dup', recipient_discord_id: '100000000000000001', guild_id: 'guild-1' },
      { qurl_id: 'q_dup', recipient_discord_id: '100000000000000002', guild_id: 'guild-1' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', usersFetch });
    int.memberPermissions = { has: jest.fn(() => true) };

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/single recipient/i),
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'ambiguous', qurl_id: 'q_dup', guild_id: 'guild-1',
    }));
  });

  test('standing: a STAFF caller (not the sender) gets the reveal (grant_basis staff)', async () => {
    const recipientId = '100000000000000021';
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_staff', match_pct: 95, confidence: 0.97 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_staff', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const usersFetch = jest.fn(async (id) => ({ id, username: 'LeakedRecipient' }));
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000801', usersFetch });
    int.memberPermissions = { has: jest.fn(() => true) };

    await handleQurlDetect(int);

    expect(usersFetch).toHaveBeenCalledWith(recipientId);
    const reply = int.editReply.mock.calls.at(-1)[0].content;
    expect(reply).toContain('LeakedRecipient');
    expect(reply).toMatch(/95% match/);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_staff', grant_basis: 'staff',
    }));
  });

  test('standing: a general member (not sender, not staff) is DENIED — byte-identical no-match + audit no_standing', async () => {
    const recipientId = '100000000000000022';
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_deny', match_pct: 95, confidence: 0.97 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_deny', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000802', usersFetch });

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: DETECT_NO_MATCH_MSG }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_standing', qurl_id: 'q_deny', guild_id: 'guild-1',
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect' && c[1].result === 'no_standing')[1];
    expect(JSON.stringify(auditMeta)).not.toContain(recipientId);
  });

  test('standing+ambiguity: a no-standing caller on an AMBIGUOUS qurl_id gets byte-identical no-match, NOT "couldn\'t determine"', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_dup2', match_pct: 90, confidence: 0.95 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_dup2', recipient_discord_id: '100000000000000001', guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
      { qurl_id: 'q_dup2', recipient_discord_id: '100000000000000002', guild_id: 'guild-1', sender_discord_id: '900000000000000778' },
    ]);
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000803' });

    await handleQurlDetect(int);

    const reply = int.editReply.mock.calls.at(-1)[0].content;
    expect(reply).toBe(DETECT_NO_MATCH_MSG);
    expect(reply).not.toMatch(/single recipient/i);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_standing', qurl_id: 'q_dup2',
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'ambiguous' }));
  });

  test('connector 5xx ⇒ graceful ephemeral error, no throw, cooldown CLEARED (transient)', async () => {
    const err = new Error('Connector detect failed (503)'); err.status = 503;
    mockDetectWatermark.mockRejectedValue(err);
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/unavailable right now/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'rate_limited' }));
  });

  test('connector 429 ⇒ rate-limited ephemeral AND cooldown KEPT (back off)', async () => {
    const err = new Error('Connector detect failed (429)'); err.status = 429;
    mockDetectWatermark.mockRejectedValue(err);
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rate-limited/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(true);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'rate_limited', guild_id: 'guild-1',
    }));
  });

  test('rejects an oversize image (cooldown CLEARED — honest user error)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, size: 999_999_999 } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/i),
      flags: 64,
    }));
    expect(int.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('CDN fetch !ok ⇒ "link may have expired" ephemeral, no connector call, cooldown CLEARED', async () => {
    global.fetch = jest.fn().mockResolvedValue({ ok: false, status: 403 });
    const int = makeDetectInteraction();

    await handleQurlDetect(int);

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/may have expired/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('CDN fetch THROWS (timeout/DNS/network) ⇒ "couldn\'t download" — NOT "detection unavailable" (item 3)', async () => {
    global.fetch = jest.fn().mockRejectedValue(new Error('fetch timed out'));
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toMatch(/could not download|couldn't download|re-upload/i);
    expect(replyContent).not.toMatch(/detection is unavailable/i);
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('realized buffer over cap (TOCTOU: metadata small, body huge) ⇒ "too large", no connector call (item 4)', async () => {
    const MAX = 25 * 1024 * 1024;
    global.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      arrayBuffer: async () => new ArrayBuffer(MAX + 1),
    });
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, size: 1024 } });

    await handleQurlDetect(int);

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('no API key configured ⇒ operator configuration error AND cooldown retained (item 2)', async () => {
    const config = require('../src/config');
    const savedKey = config.QURL_API_KEY;
    config.QURL_API_KEY = '';
    mockDb.getGuildApiKey.mockResolvedValue(null);
    const int = makeDetectInteraction();

    try {
      await handleQurlDetect(int);
    } finally {
      config.QURL_API_KEY = savedKey;
    }

    expect(int.deferReply).toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/bot operator must configure/i),
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(true);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'unconfigured', guild_id: 'guild-1', requester_id: SENDER_ID,
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect('recipient_discord_id' in auditMeta).toBe(false);
  });

  test('required image option missing ⇒ "image option is required" + cooldown CLEARED', async () => {
    const int = makeDetectInteraction({ omitImage: true });

    await handleQurlDetect(int);

    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/`image:` option is required/i),
      flags: 64,
    }));
    expect(int.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });
});
