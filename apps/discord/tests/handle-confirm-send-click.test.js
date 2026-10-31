const {
  CONFIRM_SEND_CUSTOM_ID,
  SENDER_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  VALID_ATTACHMENT,
  handleConfirmSendClick,
  isOnCooldown,
  logger,
  makeInteraction,
  mockDb,
  mockDeleteFlow,
  mockMintLinks,
  mockSendDM,
  mockTransitionFlow,
  sendCooldowns,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmSendClick', () => {
  const u1 = '100000000000000001';
  const validPayload = {
    resourceType: 'file',
    attachment: VALID_ATTACHMENT,
    locationUrl: null,
    locationName: null,
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    sendNonce: 'nonce-1',
  };

  test.each([
    [10062, 'Unknown interaction'],
    ['ECONNRESET', 'socket reset'],
  ])('failed acknowledgement (%s) stops before flow or send work', async (code, message) => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    int.customId = CONFIRM_SEND_CUSTOM_ID;
    const err = new Error(message);
    err.code = code;
    int.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmSendClick(int, {
      flow_id: 'fid', row: { payload: validPayload, version: 1 },
    });

    expect(mockDb.getGuildApiKey).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('stopping before state change'),
      expect.objectContaining({
        flow_id: 'fid', custom_id: CONFIRM_SEND_CUSTOM_ID, error_code: code,
      }),
    );
  });

  test('happy path → deferUpdate + deleteFlow + editReply "Preparing"', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(int.update).not.toHaveBeenCalled();
  });

  test('deleteFlow dedup loser → version-fenced "Recipients changed" reply, no pipeline call', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Recipients changed|re-click Send/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing/),
    }));
  });

  test('deleteFlow is version-gated to fence the picker-then-Send race', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 7 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 7,
    }));
  });

  test('all recipients have left guild → terminal, deleteFlow called, no pipeline', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { [u1]: 'unknown' },
    });
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/),
    }));
  });

  test('no apiKey resolved → tells user setup is needed, cooldown cleared (admin action recovers)', async () => {
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    const config = require('../src/config');
    jest.replaceProperty(config, 'QURL_API_KEY', null);
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/not configured|setup/i),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('partial-resolve at Send click — Send proceeds with remaining users, drop surfaced via followUp + info log', async () => {
    const gone = '100000000000000099';
    const payloadWithGhost = { ...validPayload, recipientIds: [u1, gone] };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [gone]: 'unknown' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: payloadWithGhost, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 recipient had left the server/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 1, transient: 0 }),
    );
  });

  test('partial transient lookup at Send click — Send proceeds with remaining, transient drop surfaced with retry copy (/qurl send)', async () => {
    const flaky = '100000000000000099';
    const payloadWithFlaky = { ...validPayload, recipientIds: [u1, flaky] };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [flaky]: 'ratelimit' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: payloadWithFlaky, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 couldn't be looked up.*rerun \/qurl send/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 0, transient: 1 }),
    );
  });

  test('partial transient lookup at Send click — /qurl map payload produces /qurl map rerun hint', async () => {
    const flaky = '100000000000000099';
    const mapPayload = {
      ...validPayload,
      resourceType: 'maps',
      attachment: null,
      locationUrl: 'https://google.com/maps/place/x',
      locationName: 'x',
      resourceLabel: 'x',
      recipientIds: [u1, flaky],
    };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [flaky]: 'ratelimit' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: mapPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl map/),
      ephemeral: true,
    }));
    expect(int.followUp).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl send/),
    }));
  });

  test('getGuildApiKey throw at click time → ephemeral retry, NO deleteFlow (row stays alive), cooldown cleared', async () => {
    mockDb.getGuildApiKey.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up the qURL API key/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/getGuildApiKey threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('resolveRecipientUsers throw at click time → ephemeral retry message, NO deleteFlow', async () => {
    const int = makeInteraction({
      guildMembers: {},
    });
    int.guild.members.fetch = jest.fn().mockRejectedValue(new Error('catastrophic'));
    Object.defineProperty(int.guild, 'members', {
      get() { throw new Error('cache exploded'); },
    });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: { ...validPayload, recipientIds: [u1] }, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up recipients/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/resolveRecipientUsers threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('Send click with sender-only recipientIds → legitimate self-send, proceeds to dispatch', async () => {
    const int = makeInteraction({ guildMembers: { [SENDER_ID]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [SENDER_ID] }, version: 1 },
    });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      reason: 'terminal',
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/all left the server/i),
    }));
  });

  test('forged Send click with empty recipientIds → distinct copy + deleteFlow (not the "all left" copy)', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [] }, version: 1 },
    });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/No recipients were selected/i),
      components: [],
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/i),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 1,
    }));
  });

  test('empty recipientIds + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp, no editReply wipe', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [] }, version: 1 },
    });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/No recipients were selected/i),
    }));
  });

  test('all-invalid recipients + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const botId = '100000000000000999';
    const int = makeInteraction({ guildMembers: { [botId]: { bot: true } } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [botId] }, version: 5 },
    });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 5,
    }));
  });

  test('bot kicked between confirm and Send → distinct ephemeral, flow row deleted, cooldown cleared', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    int.guild = null;
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/bot is no longer in this server/i),
      components: [],
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/i),
    }));
  });
});
