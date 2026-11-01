const {
  SENDER_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmVoiceEveryone,
  makeInteraction,
  makeUser,
  mockDeleteFlow,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmVoiceEveryone', () => {
  const VOICE_CH = 'voice-ch-1';
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const bot1 = '100000000000000099';

  function makeVoiceInteraction({ members = [], channelType = 2, botIds = [] } = {}) {
    const int = makeInteraction();
    int.guild.channels.cache = new Map();
    const chanMembers = new Map();
    for (const mid of members) {
      const isBot = botIds.includes(mid);
      const member = { user: { id: mid, bot: isBot } };
      int.guild.members.cache.set(mid, member);
      chanMembers.set(mid, member);
    }
    int.guild.channels.cache.set(VOICE_CH, {
      id: VOICE_CH, type: channelType, members: chanMembers,
    });
    return int;
  }

  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    voiceChannelId: VOICE_CH,
  };

  test('failed acknowledgement stops before voice resolution or flow mutation', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmVoiceEveryone(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path: voice-connected non-bot members populate recipientIds and advance the flow', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({
        recipients: expect.arrayContaining([u1, u2].map(id => expect.objectContaining({ id }))),
        voiceChannelId: VOICE_CH,
      }),
      terminal: false,
    }));
    const ids = mockTransitionFlow.mock.calls[0][2].payload.recipients.map(recipient => recipient.id);
    expect(ids).toHaveLength(2);
  });

  test('happy path: bots are filtered out of the connected set', async () => {
    const int = makeVoiceInteraction({ members: [u1, bot1, u2], botIds: [bot1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const ids = mockTransitionFlow.mock.calls[0][2].payload.recipients.map(recipient => recipient.id);
    expect(ids.sort()).toEqual([u1, u2].sort());
    expect(ids).not.toContain(bot1);
  });

  test('missing voiceChannelId in payload: re-renders card WITHOUT the voice button (visible feedback)', async () => {
    const int = makeVoiceInteraction({ members: [u1] });
    const payloadWithoutVoice = { ...basePayload, voiceChannelId: null };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: payloadWithoutVoice, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toBeTruthy();
    expect(Array.isArray(lastCall.components)).toBe(true);
    expect(lastCall.components.length).toBeGreaterThan(0);
  });

  test('missing voiceChannelId WITH previously-picked recipients: re-render preserves recipients (no UI/state drift)', async () => {
    const int = makeVoiceInteraction({ members: [u1] });
    int.guild.members.cache.set(u1, { user: makeUser(u1) });
    const payloadWithoutVoice = {
      ...basePayload,
      voiceChannelId: null,
      recipientIds: [u1],
      recipientAliases: { [u1]: 'alice' },
    };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: payloadWithoutVoice, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Sending file/);
    expect(lastCall.content).toMatch(/\*\*To:\*\* 1 user/);
  });

  test('channel deleted between render and click: rejectVoice path runs (warning re-render, no transition)', async () => {
    const int = makeInteraction();
    int.guild.channels.cache = new Map(); // no entry for VOICE_CH
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Couldn't read the voice channel/i);
  });

  test('empty voice channel: rejectVoice path runs (no-one-connected copy, no transition)', async () => {
    const int = makeVoiceInteraction({ members: [] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/No one is connected/i);
  });

  test('bots-only voice channel: surfaces "Cannot send to bots" (NOT the empty-channel copy)', async () => {
    const int = makeVoiceInteraction({ members: [bot1], botIds: [bot1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Cannot send to bots/i);
    expect(lastCall.content).not.toMatch(/No one is connected/i);
  });

  test('sender is voice-connected → excluded from recipientIds + selfIncluded:false', async () => {
    const int = makeVoiceInteraction({ members: [SENDER_ID, u1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipients.map(recipient => recipient.id)).toEqual([u1]);
    expect(payload.recipients.map(recipient => recipient.id)).not.toContain(SENDER_ID);
    expect(payload.selfIncluded).toBe(false);
  });

  test('sender NOT in voice channel → selfIncluded:false on the new payload', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.selfIncluded).toBe(false);
  });

  test('new payload carries recipientMode:"voice" — commits the layout switch', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientMode).toBe('voice');
  });

  test('sender-only voice channel → reject path with "you\'re the only one" copy', async () => {
    const int = makeVoiceInteraction({ members: [SENDER_ID] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/only one in this voice channel/i);
  });

  test('transitionFlow conflict → superseded message (OCC race with sibling interaction)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/superseded/);
    expect(lastCall.components).toEqual([]);
  });

  test('partial-cache row (member missing .user) emits a debug log per send (silent-shrinkage telemetry)', async () => {
    const logger = require('../src/logger');
    logger.debug.mockClear();
    const int = makeVoiceInteraction({ members: [u1] });
    const channel = int.guild.channels.cache.get(VOICE_CH);
    channel.members.set('partial-cache-id', {});  // no .user → drop
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringMatching(/partial-cache/i),
      expect.objectContaining({
        flow_id: 'fid',
        voice_channel_id: VOICE_CH,
        dropped: 1,
      })
    );
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('voice-everyone button succeeds when sender lacks MENTION_EVERYONE (no asymmetric gate)', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    int.memberPermissions = { has: () => false };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipients.map(recipient => recipient.id).sort()).toEqual([u1, u2].sort());
  });

  test('transitionFlow not_found → expired message (row TTL elapsed between click and write)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/expired/);
    expect(lastCall.components).toEqual([]);
  });

  test('cap overshoot (env-overridden small cap): hard-rejects with subset-prompt copy', async () => {
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      const int = makeVoiceInteraction({ members: [u1, u2] });
      await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      expect(mockTransitionFlow).not.toHaveBeenCalled();
      const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
      expect(lastCall.content).toMatch(/Voice channel has 2 eligible recipients \(max 1\)/i);
      expect(lastCall.content).toMatch(/picker or @mentions/i);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('corrupt payload (unknown resourceType): deleteFlow + actionable re-run copy', async () => {
    const int = makeVoiceInteraction({ members: [u1] });
    const corruptPayload = { ...basePayload, resourceType: 'bogus' };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: corruptPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Card data is corrupted/);
  });

  test('success-path emits info-level audit log with counts', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.objectContaining({
        flow_id: 'fid',
        guild_id: int.guildId,
        user_id: int.user.id,
        voice_channel_id: VOICE_CH,
        valid_count: 2,
        dropped_bots: 0,
        partial_cache_drops: 0,
        self_included: false,
        voice_member_count: 2,
      }),
    );
  });

  test('success-log: voice_member_count tracks channel.members.size, NOT valid.length, under partial-cache drops', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    const channel = int.guild.channels.cache.get(VOICE_CH);
    channel.members.set('partial-cache-id', {});
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.objectContaining({
        valid_count: 2,
        partial_cache_drops: 1,
        voice_member_count: 3,
      }),
    );
  });

  test('success-log does NOT fire on transitionFlow conflict / not_found / throw', async () => {
    const logger = require('../src/logger');

    logger.info.mockClear();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );

    logger.info.mockClear();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );

    logger.info.mockClear();
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB unavailable'));
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );
  });
});
