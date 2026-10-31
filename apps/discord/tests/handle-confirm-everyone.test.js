const {
  SENDER_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  makeInteraction,
  mockDeleteFlow,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmEveryone', () => {
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const bot1 = '100000000000000099';

  function makeEveryoneInteraction({
    canMentionEveryone = true,
    guildMembers = {
      [u1]: {},
      [u2]: {},
      [bot1]: { bot: true },
      [SENDER_ID]: {},
    },
    memberCount = 4,
    ...rest
  } = {}) {
    const int = makeInteraction({ guildMembers, ...rest });
    int.memberPermissions = { has: jest.fn(() => canMentionEveryone) };
    if (int.guild) int.guild.memberCount = memberCount;
    return int;
  }

  const initialPayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    recipientMode: 'picker',
  };

  const { handleConfirmEveryone } = require('../src/commands');

  test('failed acknowledgement stops before everyone expansion or flow mutation', async () => {
    const int = makeEveryoneInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmEveryone(int, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path → expands to all non-bot members, transitions flow', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
    expect(payload.selfIncluded).toBe(true);
    expect(payload.recipientMode).toBe('everyone');
  });

  test('without MENTION_EVERYONE → reject with permission warning, no transition', async () => {
    const int = makeEveryoneInteraction({ canMentionEveryone: false });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Mention Everyone/);
    const logger = require('../src/logger');
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('without MENTION_EVERYONE'),
      expect.any(Object),
    );
  });

  test('cache stays empty after prewarm despite populated guild → reject with "try again" copy', async () => {
    const int = makeEveryoneInteraction({ guildMembers: {}, memberCount: 5 });
    int.guild.members.fetch = jest.fn().mockResolvedValue(new Map());
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/member cache not ready/i);
  });

  test('sender-only-in-cache (no other non-bots) → reject with "matched only you" copy', async () => {
    const int = makeEveryoneInteraction({
      guildMembers: { [SENDER_ID]: {}, [bot1]: { bot: true } },
      memberCount: 2,
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/matched only you/i);
  });

  test('valid.length === cap (exactly at boundary) → proceeds (cap-reject is strictly >)', async () => {
    const config = require('../src/config');
    const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
    try {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 3, configurable: true, writable: true });
      const int = makeEveryoneInteraction({
        guildMembers: {  // 3 non-bots exactly = cap
          [SENDER_ID]: {},
          '100000000000000010': {},
          '100000000000000011': {},
        },
        memberCount: 3,
      });
      await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
      expect(mockTransitionFlow).toHaveBeenCalled();
      const payload = mockTransitionFlow.mock.calls[0][2].payload;
      expect(payload.recipientIds.length).toBe(3);
    } finally {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
    }
  });

  test('forged-interaction warn log includes guild_id for forensics correlation', async () => {
    const int = makeEveryoneInteraction({ canMentionEveryone: false });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('without MENTION_EVERYONE'),
      expect.objectContaining({ flow_id: 'fid', guild_id: expect.any(String) }),
    );
  });

  test('only bots in cache → reject with bots-dropped copy', async () => {
    const int = makeEveryoneInteraction({
      guildMembers: { [bot1]: { bot: true } },
      memberCount: 1,
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No usable recipients|bot/i);
  });

  test('cache size > QURL_SEND_MAX_RECIPIENTS → hard reject (no truncation)', async () => {
    const config = require('../src/config');
    const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
    try {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
      const int = makeEveryoneInteraction();  // 3 non-bots > cap 2
      await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
      expect(mockTransitionFlow).not.toHaveBeenCalled();
      const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
      expect(reply.content).toMatch(/max 2/);
      expect(reply.content).toMatch(/picker|@mentions/i);
    } finally {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
    }
  });

  test('corrupt resourceType → deleteFlow + actionable error, no transition', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, {
      flow_id: 'fid',
      row: { payload: { ...initialPayload, resourceType: 'mystery' }, version: 1 },
    });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/corrupted/i);
  });

  test('deferUpdate fires before transitionFlow — Discord 3s ack guard', async () => {
    let deferAckedBeforeTransition = false;
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('transitionFlow returns conflict → "Send was superseded" copy, no followup', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/superseded/i);
    expect(reply.components).toEqual([]);
    expect(int.followUp).not.toHaveBeenCalled();
  });

  test('transitionFlow returns not_found → "send expired" copy, no followup', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/expired/i);
    expect(reply.components).toEqual([]);
    expect(int.followUp).not.toHaveBeenCalled();
  });

  test('transitionFlow throws → ephemeral followUp with retry copy', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb blip'));
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save.*Try again/i),
      ephemeral: true,
    }));
    const logger = require('../src/logger');
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('handleConfirmEveryone: transitionFlow threw'),
      expect.any(Object),
    );
  });

  test('sender row missing from cache + other non-bots present → defensively pushed into recipients', async () => {
    const otherUser = '100000000000000077';
    const int = makeEveryoneInteraction({
      guildMembers: { [otherUser]: {} },
      memberCount: 2,  // sender + otherUser
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
  });

  test('sender missing + warm cache with degraded .user-less row + other non-bots → defensive push fires, partial-cache drops counted', async () => {
    const otherUser = '100000000000000088';
    const int = makeEveryoneInteraction({
      guildMembers: { [otherUser]: {} },
      memberCount: 3,  // sender + otherUser + the degraded row below
    });
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('@everyone expansion succeeded'),
      expect.objectContaining({
        valid_count: 2,
        partial_cache_drops: 1,
        cache_size: 2,  // otherUser + degraded-1 = 2 entries
        member_count: 3,
      }),
    );
  });

  test('sender row missing + bots-only cache → still rejects (defensive push gated)', async () => {
    const bot2 = '100000000000000098';
    const int = makeEveryoneInteraction({
      guildMembers: { [bot1]: { bot: true }, [bot2]: { bot: true } },
      memberCount: 3,  // sender + 2 bots
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No usable recipients|bot/i);
  });

  test('forged interaction with no guild → reject without crash + permission warning', async () => {
    const int = makeEveryoneInteraction({ guildId: null });  // → guild = null
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Mention Everyone/);
  });

  test('success-path emits info-level audit log with counts', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('@everyone expansion succeeded'),
      expect.objectContaining({
        flow_id: 'fid',
        valid_count: expect.any(Number),
        dropped_bots: expect.any(Number),
        partial_cache_drops: expect.any(Number),
        self_included: true,
      }),
    );
  });

  test('partial-cache rows (member without .user) → counted in debug log + filtered from selection', async () => {
    const int = makeEveryoneInteraction();
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    int.guild.memberCount = 5;
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('mid-deploy forward: legacy picker-mode row with pre-filled recipientIds → click @everyone → lands in EVERYONE mode', async () => {
    const int = makeEveryoneInteraction();
    const legacyPayload = {
      ...initialPayload,
      recipientMode: 'picker',
      recipientIds: [u1],  // legacy picker-mode auto-fill subset
      recipientAliases: { [u1]: 'alice' },
    };
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientMode).toBe('everyone');
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
  });
});
