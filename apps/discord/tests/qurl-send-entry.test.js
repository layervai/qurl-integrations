const {
  SENDER_ID,
  SEND_FLOW_TTL_SECONDS,
  SEND_STAGE_AWAITING_CONFIRM,
  VALID_ATTACHMENT,
  getActiveFileSends,
  handleQurlMap,
  handleQurlSend,
  isOnCooldown,
  isPrewarmCall,
  logger,
  makeInteraction,
  makeUser,
  mockDeleteFlow,
  mockSupersedeOrCreate,
  sendCooldowns,
  setActiveFileSends,
} = require('./helpers/qurl-send-fixtures');

describe('handleQurlSend — slash entry', () => {
  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      ephemeral: true,
    }));
  });

  test('rejects when activeFileSends is at cap (UX fast-fail) — cooldown NOT burned (server-side backpressure)', async () => {
    const originalActive = getActiveFileSends();
    try {
      setActiveFileSends(99);  // any value ≥ MAX_CONCURRENT_FILE_SENDS
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      await handleQurlSend(int);
      expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringMatching(/too many file sends/i),
        ephemeral: true,
      }));
      expect(isOnCooldown(SENDER_ID)).toBe(false);
      expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    } finally {
      setActiveFileSends(originalActive);
    }
  });

  test('rejects when attachment.url is not Discord CDN (SSRF gate) — cooldown PRESERVED (probing defense)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, url: 'https://evil.com/x.png' } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/),
      ephemeral: true,
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(true);
  });

  test('rejects disallowed file type — cooldown CLEARED (honest user error, not abuse)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, contentType: 'application/x-evil-macroenabled' } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/File type not allowed/),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('rejects file over size cap — cooldown CLEARED (honest user error)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, size: 999_999_999 } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('happy path with recipients string — supersedeOrCreate called + confirm card rendered', async () => {
    const u1 = '100000000000000001';
    const u2 = '100000000000000002';
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: `<@${u1}> <@${u2}>`,
      },
      guildMembers: { [u1]: {}, [u2]: {} },
    });
    await handleQurlSend(int);
    expect(int.deferReply).toHaveBeenCalled();
    expect(mockSupersedeOrCreate).toHaveBeenCalledWith(expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      ttl_seconds: SEND_FLOW_TTL_SECONDS,
    }));
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('file');
    expect(payload.recipientIds.sort()).toEqual([u1, u2]);
    expect(payload.expiresIn).toBe('24h');
    expect(payload.selfDestructSeconds).toBeNull();
    expect(payload.personalMessage).toBeNull();
    expect(payload.recipientAliases).toEqual(
      expect.objectContaining({ [u1]: expect.any(String), [u2]: expect.any(String) })
    );
    expect(payload).toHaveProperty('warningsBlock');
    expect(int.editReply).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Sending file/);
    expect(reply.content).toMatch(/x\.png/);
    expect(reply.components.length).toBeGreaterThan(0);
  });

  test('/qurl map slash entry persists recipientAliases (parity with /qurl send)', async () => {
    const u1 = '100000000000000001';
    const int = makeInteraction({
      options: {
        _sub: 'map',
        location: 'https://maps.app.goo.gl/abcXYZ',
        recipients: `<@${u1}>`,
      },
      guildMembers: { [u1]: {} },
    });
    await handleQurlMap(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('maps');
    expect(payload.recipientAliases).toEqual(
      expect.objectContaining({ [u1]: expect.any(String) })
    );
  });

  test('warm over-cap server explains disabled everyone button through the slash handler', async () => {
    const guildMembers = Object.fromEntries(Array.from({ length: 26 }, (_, i) => [String(200000000000000000n + BigInt(i)), {}]));
    const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT }, guildMembers });
    int.guild.memberCount = 26;
    int.memberPermissions = { has: permission => permission === 131072n };
    await handleQurlSend(int);
    const reply = int.editReply.mock.calls.at(-1)[0];
    expect(reply.content).toContain('@everyone is unavailable:');
    expect(reply.content).toContain('26 human recipients, above the 25-recipient limit.');
    expect(reply.content).toContain('Pick individual recipients or a smaller role.');
  });

  test('happy path without recipients → confirm card with picker', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Pick recipients/);
  });

  test('all recipients are bots → ephemeral error, no flow row', async () => {
    const u1 = '100000000000000001';
    const u2 = '100000000000000002';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${u1}> <@${u2}>` },
      guildMembers: { [u1]: { bot: true }, [u2]: { bot: true } },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).toMatch(/bots are skipped/);
  });

  test('only sender mentioned → confirm card with self-included notice', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${SENDER_ID}>` },
      guildMembers: { [SENDER_ID]: {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Send includes you/);
    expect(reply.content).not.toMatch(/No valid recipients/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.selfIncluded).toBe(true);
    expect(payload.recipientIds).toEqual([SENDER_ID]);
  });

  test('DM context (no guild) suppresses the @everyone permission warning', async () => {
    const int = makeInteraction({
      guildId: null, // → guild = null in makeInteraction
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${SENDER_ID}>` },
    });
    await handleQurlSend(int);
    const calls = int.editReply.mock.calls;
    for (const [arg] of calls) {
      expect(arg.content || '').not.toMatch(/Mention Everyone permission/);
    }
  });

  test('guild context + no MENTION_EVERYONE → @everyone warning renders + Alice still parses', async () => {
    const aliceId = '400000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/Mention Everyone\b/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
  });

  test('text path: <@&roleId> for non-mentionable role WITHOUT MENTION_EVERYONE → warning with role name, no expansion', async () => {
    const aliceId = '400000000000000010';
    const bobId = '400000000000000011';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}> <@&7000>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.guild.roles.cache.set('7000', {
      id: '7000',
      name: 'admin-team',
      mentionable: false,
      members: new Map([[bobId, { user: { id: bobId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/@admin-team/);
    expect(lastEdit.content).toMatch(/Mention Everyone/);
    expect(lastEdit.content).toMatch(/role\.mentionable: true/);
  });

  test('text path: every recipient denied-role-only → "no valid recipients" but NOT misleading bot-only log nor transient-retry copy', async () => {
    const aliceId = '400000000000000030';
    const logger = require('../src/logger');
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@&7002>' },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7002', {
      id: '7002',
      name: 'private-team',
      mentionable: false,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/@private-team/);
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).not.toMatch(/Could not look up recipients right now/);
    const infoLogCalls = logger.info.mock.calls.filter(
      ([msg]) => typeof msg === 'string' && msg.includes('bot-only-or-self mention list'),
    );
    expect(infoLogCalls).toEqual([]);
  });

  test('text path: <@&roleId> for non-mentionable role WITH MENTION_EVERYONE → expands normally, no warning', async () => {
    const aliceId = '400000000000000020';
    const bobId = '400000000000000021';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@&7001>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.guild.roles.cache.set('7001', {
      id: '7001',
      name: 'admin-team',
      mentionable: false,
      members: new Map([
        [aliceId, { user: { id: aliceId, bot: false } }],
        [bobId, { user: { id: bobId, bot: false } }],
      ]),
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds.sort()).toEqual([aliceId, bobId].sort());
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).not.toMatch(/role\.mentionable/);
  });

  test('guild context + MENTION_EVERYONE permission → @everyone expands, no warning', async () => {
    const aliceId = '400000000000000002';
    const bobId = '400000000000000003';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).not.toMatch(/Mention Everyone permission/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds.sort()).toEqual([aliceId, bobId].sort());
  });

  test('all mentioned recipients hit transient lookup failure → retry copy, not "no valid recipients"', async () => {
    const flaky1 = '100000000000000099';
    const flaky2 = '100000000000000098';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${flaky1}> <@${flaky2}>` },
      guildMembers: {},
      guildFetchByID: { [flaky1]: 'ratelimit', [flaky2]: 'ratelimit' },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Could not look up recipients right now.*Try again/i);
    expect(reply.content).not.toMatch(/No valid recipients to send to/);
  });

  test('unknown-member ID surfaced as warning but valid users still proceed', async () => {
    const known = '100000000000000001';
    const gone = '100000000000000099';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${known}> <@${gone}>` },
      guildMembers: { [known]: {} },
      guildFetchByID: { [gone]: 'unknown' },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([known]);
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/no longer in this server/);
  });

  test('cooldown active rejects', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@100000000000000001>` },
      guildMembers: { '100000000000000001': {} },
    });
    sendCooldowns.set(SENDER_ID, Date.now());
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/wait before sending/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  test('supersedeOrCreate sibling collision → surfaces sibling message', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_revoke_select' },
    });
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/revoke.*menu open/);
  });

  test('forwards expires-in + self-destruct + personal-message into payload', async () => {
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001>',
        'expires-in': '7d',
        'self-destruct': '300',
        'personal-message': 'see you Tuesday',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.expiresIn).toBe('7d');
    expect(payload.selfDestructSeconds).toBe(300);
    expect(payload.personalMessage).toBe('see you Tuesday');
  });

  test('rejects off-set expires-in (defense vs forged interaction)', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>', 'expires-in': '99y' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Unrecognized expiry/);
  });

  test('all-bots cap-skew: cache-miss bots eat cap, real users get squeezed out', async () => {
    const mkId = (n) => '1000000000000' + String(n).padStart(6, '0');
    const realUser = mkId(25);  // beyond the cap
    const bots = Array.from({ length: 25 }, (_, i) => mkId(i));
    const mentions = [...bots, realUser].map((id) => `<@${id}>`).join(' ');
    const fetchByID = {};
    for (const id of bots) fetchByID[id] = makeUser(id, { bot: true });
    fetchByID[realUser] = makeUser(realUser);  // resolves cleanly, but past cap
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: mentions },
      guildMembers: {},  // ALL cache-miss
      guildFetchByID: fetchByID,
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).toMatch(/bot/);
  });

  test('inner catch on supersedeOrCreate throw clears cooldown + surfaces specific error', async () => {
    mockSupersedeOrCreate.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Could not start a send/);
  });

  test('safety-net top-level catch clears cooldown on an unanticipated synchronous throw', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply.mockRejectedValueOnce(new Error('token expired'));
    await handleQurlSend(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ user_id: SENDER_ID }),
    );
  });

  test('safety-net catch deletes orphan flow row when post-supersede throw fires', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.editReply.mockRejectedValueOnce(new Error('Discord 500'));
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    expect(mockDeleteFlow).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({
        stage: 'awaiting_send_confirm',
        reason: 'terminal',
      }),
    );
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ flow_id: expect.any(String) }),
    );
  });

  test('safety-net catch does NOT call deleteFlow when throw fires before supersedeOrCreate', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply.mockRejectedValueOnce(new Error('token expired'));
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  test('safety-net catch does NOT call deleteFlow when supersedeOrCreate returned created:false (sibling flow)', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_confirm', flow_id: 'other_flow' },
    });
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  describe('voice-channel slash entry (auto voice-everyone default)', () => {

    const VOICE_CH = 'voice-ch-slash-1';
    const u1 = '100000000000000011';
    const u2 = '100000000000000012';
    const bot = '100000000000000099';

    function makeVoiceEntryInteraction({ members = [], botIds = [] } = {}) {
      const chanMembers = new Map();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },
      });
      int.channel = { id: VOICE_CH, type: 2 };
      for (const mid of members) {
        const isBot = botIds.includes(mid);
        const member = { user: { id: mid, bot: isBot } };
        int.guild.members.cache.set(mid, member);
        chanMembers.set(mid, member);
      }
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2, name: 'general', members: chanMembers,
      });
      return int;
    }

    test('happy path: voice members minus sender land in payload, recipientMode:"voice"', async () => {
      const int = makeVoiceEntryInteraction({ members: [SENDER_ID, u1, u2] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('voice');
      expect(payload.recipientIds.sort()).toEqual([u1, u2].sort());
      expect(payload.recipientIds).not.toContain(SENDER_ID);
      expect(payload.selfIncluded).toBe(false);
    });

    test('bots in voice are filtered before voice-mode is committed', async () => {
      const int = makeVoiceEntryInteraction({
        members: [u1, bot, u2],
        botIds: [bot],
      });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('voice');
      expect(payload.recipientIds.sort()).toEqual([u1, u2].sort());
      expect(payload.recipientIds).not.toContain(bot);
    });

    test('bots-only voice → picker fallback WITH bot-drop banner (not silent)', async () => {
      const int = makeVoiceEntryInteraction({
        members: [bot],
        botIds: [bot],
      });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
      expect(payload.warningsBlock).toMatch(/bot/i);
    });

    test('sender-only voice → falls back to picker-mode (no auto voice)', async () => {
      const int = makeVoiceEntryInteraction({ members: [SENDER_ID] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
    });

    test('empty voice channel → falls back to picker-mode', async () => {
      const int = makeVoiceEntryInteraction({ members: [] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
    });

    test('over-cap voice → falls back to picker-mode WITH banner explaining why', async () => {
      const config = require('../src/config');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      config.QURL_SEND_MAX_RECIPIENTS = 1;  // force over-cap with 2 members
      try {
        const int = makeVoiceEntryInteraction({ members: [u1, u2] });
        await handleQurlSend(int);
        const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
        expect(payload.recipientMode).toBe('picker');
        expect(payload.recipientIds).toEqual([]);
        expect(payload.warningsBlock).toMatch(/Voice channel has 2 eligible recipients/);
        expect(payload.warningsBlock).toMatch(/max 1/);
      } finally {
        config.QURL_SEND_MAX_RECIPIENTS = originalCap;
      }
    });

    test('voice channel cache miss → picker-mode with "Couldn\'t read voice channel" banner', async () => {
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      int.channel = { id: VOICE_CH, type: 2 };
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.warningsBlock).toMatch(/Couldn't read voice channel members/);
    });

    test('explicit `recipients:` overrides voice-mode default (manual selection wins)', async () => {
      const int = makeVoiceEntryInteraction({ members: [u1, u2] });
      int.options.getString.mockImplementation((key) =>
        (key === 'recipients' ? `<@${u1}>` : null)
      );
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([u1]);
    });

    test('text `@everyone` in recipients → EVERYONE mode (picker hidden, no auto-fill)', async () => {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: {
          [SENDER_ID]: {},
          '100000000000000051': {},
          '100000000000000052': {},
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('everyone');
      expect(payload.selfIncluded).toBe(true);
      expect(payload.recipientIds.length).toBeGreaterThanOrEqual(2);
    });

    test('text `@everyone` with sender MISSING from members.cache post-prewarm → selfIncluded:false (documented divergence from button-click path)', async () => {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: {
          '100000000000000051': {},
          '100000000000000052': {},
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('everyone');
      expect(payload.selfIncluded).toBe(false);
      expect(payload.recipientIds).not.toContain(SENDER_ID);
    });

    test('text `@everyone` WITHOUT MENTION_EVERYONE → stays PICKER (parser denied expansion)', async () => {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: { [SENDER_ID]: {} },
      });
      await handleQurlSend(int);
      const supersedeCalls = mockSupersedeOrCreate.mock.calls;
      expect(supersedeCalls.length).toBe(0);
    });
  });
});

describe('handleQurlSlashSend — guild.members cache pre-warm', () => {
  test('@everyone in recipients string → members.list() pre-warm fires', async () => {
    const aliceId = '500000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('<@&roleId> in recipients string → members.list() pre-warm fires', async () => {
    const aliceId = '500000000000000002';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@&7100>` },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7100', {
      id: '7100', name: 'team', mentionable: true,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('plain <@userId> mentions only → members.list() pre-warm does NOT fire', async () => {
    const aliceId = '500000000000000003';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('empty recipients string → members.list() pre-warm does NOT fire', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT }, // no recipients
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone WITHOUT MENTION_EVERYONE → pre-warm does NOT fire (parser will deny anyway)', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone + <@&roleId> WITHOUT MENTION_EVERYONE → pre-warm STILL fires (role path)', async () => {
    const aliceId = '500000000000000077';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@&7200>` },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7200', {
      id: '7200', name: 'team', mentionable: true,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('@everyonefoo (no word boundary) → pre-warm does NOT fire', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyonefoo' },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('cache already at memberCount → members.list() pre-warm short-circuits', async () => {
    const aliceId = '500000000000000005';
    const bobId = '500000000000000006';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2; // matches cache.size from guildMembers above
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('http-only tier (memberCount undefined) → pre-warm always paginates, NEVER short-circuits on approximateMemberCount', async () => {
    const aliceId = '500000000000000007';
    const bobId = '500000000000000008';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = undefined;
    int.guild.approximateMemberCount = 2;
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall).length).toBeGreaterThan(0);
  });

  test('concurrent invocations in the same guild share one in-flight fetch', async () => {
    const aliceId = '500000000000000010';
    let release;
    const listGate = new Promise((r) => { release = r; });
    const sharedList = jest.fn(() => listGate);
    const sharedGuild = {
      id: 'shared-guild',
      members: { cache: new Map(), list: sharedList },
      roles: { cache: new Map() },
      channels: { cache: new Map() },
      memberCount: 10,
    };
    function makeShared() {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
        guildMembers: { [aliceId]: {} },
      });
      int.guild = sharedGuild;
      int.guildId = sharedGuild.id;
      int.memberPermissions = { has: jest.fn(() => true) };
      return int;
    }
    const p1 = handleQurlSend(makeShared());
    const p2 = handleQurlSend(makeShared());
    await new Promise((r) => setImmediate(r));
    const prewarmCalls = sharedList.mock.calls.filter(isPrewarmCall);
    expect(prewarmCalls.length).toBe(1);
    release(new Map());
    await Promise.all([p1, p2]);
  });

  test('members.list() rejection is swallowed — flow continues in degraded mode', async () => {
    const aliceId = '500000000000000004';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.members.list = jest.fn(async () => {
      const err = new Error('rate limited'); err.code = 429; throw err;
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
  });

  test('pagination: members.list() called with `after` cursor when first page returns full 1000', async () => {
    const aliceId = '500000000000000077';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 1500;

    const lastIdOfPage1 = '999999999999999999';
    const page1 = { size: 1000, lastKey: () => lastIdOfPage1 };
    const page2 = new Map();  // empty → loop breaks
    int.guild.members.list = jest.fn()
      .mockResolvedValueOnce(page1)
      .mockResolvedValueOnce(page2);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(2);
    expect(int.guild.members.list.mock.calls[0][0]).toEqual(expect.objectContaining({ limit: 1000 }));
    expect(int.guild.members.list.mock.calls[0][0].after).toBeUndefined();
    expect(int.guild.members.list.mock.calls[1][0]).toEqual(expect.objectContaining({ limit: 1000, after: lastIdOfPage1 }));
  });

  test('pagination: three full pages advance the cursor each time', async () => {
    const aliceId = '500000000000000078';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2500;

    const lastIdOfPage1 = '111111111111111111';
    const lastIdOfPage2 = '222222222222222222';
    const page1 = { size: 1000, lastKey: () => lastIdOfPage1 };
    const page2 = { size: 1000, lastKey: () => lastIdOfPage2 };
    const page3 = new Map(); // partial → loop breaks
    int.guild.members.list = jest.fn()
      .mockResolvedValueOnce(page1)
      .mockResolvedValueOnce(page2)
      .mockResolvedValueOnce(page3);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(3);
    expect(int.guild.members.list.mock.calls[0][0].after).toBeUndefined();
    expect(int.guild.members.list.mock.calls[1][0].after).toBe(lastIdOfPage1);
    expect(int.guild.members.list.mock.calls[2][0].after).toBe(lastIdOfPage2);
  });

  test('pagination: cursor non-advancement bails with warn', async () => {
    const aliceId = '500000000000000079';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 5000;

    const stuckCursor = '333333333333333333';
    const stuckPage = { size: 1000, lastKey: () => stuckCursor };
    int.guild.members.list = jest.fn().mockResolvedValue(stuckPage);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(2);
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('cursor did not advance'),
    );
    expect(warnCall).toBeTruthy();
    const successCall = logger.info.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('pre-warm complete'),
    );
    expect(successCall).toBeFalsy();
  });

  test('pagination: safety cap fires warn when full pages persist past PREWARM_MAX_PAGES', async () => {
    const aliceId = '500000000000000080';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2000000; // > 1M, defeats the hot-cache short-circuit

    let counter = 0;
    int.guild.members.list = jest.fn(async () => ({
      size: 1000,
      lastKey: () => `cursor-${counter++}`, // unique every page
    }));

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(1000);
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('hit safety cap'),
    );
    expect(warnCall).toBeTruthy();
    expect(warnCall[1]).toEqual(expect.objectContaining({ cache_size: expect.anything() }));
    const successCall = logger.info.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('pre-warm complete'),
    );
    expect(successCall).toBeFalsy();
  });

  test('pagination: each successful list() call merges members into guild.members.cache', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: {},
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2500;

    const page1Ids = ['100', '101', '102'];
    const page2Ids = ['200', '201'];
    int.guild.members.list = jest.fn(async ({ after }) => {
      if (!after) {
        for (const id of page1Ids) int.guild.members.cache.set(id, { user: { id, bot: false } });
        return { size: 1000, lastKey: () => page1Ids[page1Ids.length - 1] };
      }
      for (const id of page2Ids) int.guild.members.cache.set(id, { user: { id, bot: false } });
      return new Map(); // partial → loop breaks
    });

    await handleQurlSend(int);

    for (const id of [...page1Ids, ...page2Ids]) {
      expect(int.guild.members.cache.has(id)).toBe(true);
    }

    for (const call of int.guild.members.list.mock.calls) {
      expect(call[0].cache).not.toBe(false);
    }
  });
});
