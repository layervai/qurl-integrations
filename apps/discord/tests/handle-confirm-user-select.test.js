const {
  SENDER_ID,
  SEND_FLOW_TTL_SECONDS,
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmUserSelect,
  isPrewarmCall,
  logger,
  makeInteraction,
  makeUser,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmUserSelect', () => {
  const u1 = '100000000000000001';

  function makeSelectInteraction({
    users = [makeUser(u1)],
    roles = [],
    canMentionEveryone = false,
    guildMemberCache = null,
    ...rest
  } = {}) {
    const int = makeInteraction(rest);
    int.users = new Map(users.map((u) => [u.id, u]));
    int.roles = new Map(roles);
    int.memberPermissions = {
      has: jest.fn(() => canMentionEveryone),
    };
    if (guildMemberCache && int.guild) {
      int.guild.members.cache = guildMemberCache;
    }
    return int;
  }

  const initialPayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  test('valid pick → transitionFlow with new recipientIds + update', async () => {
    const beforeSecs = Math.floor(Date.now() / 1000);
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ recipientIds: [u1] }),
      terminal: false,
      set_expires_at: expect.any(Number),
    }));
    const callArgs = mockTransitionFlow.mock.calls[0][2];
    const SKEW = 5;
    expect(callArgs.set_expires_at).toBeGreaterThanOrEqual(beforeSecs + SEND_FLOW_TTL_SECONDS - SKEW);
    expect(callArgs.set_expires_at).toBeLessThanOrEqual(Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS + SKEW);
    expect(int.editReply).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Sending file/);
  });

  test('empty pick → deferUpdate, no transition, no editReply', async () => {
    const int = makeSelectInteraction({ users: [] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('deferUpdate fires before transitionFlow await — protects Discord 3s ack budget on slow DDB', async () => {
    let deferAckedBeforeTransition = false;
    const int = makeSelectInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('duplicate whose deferUpdate is rejected cannot mutate flow state', async () => {
    const duplicate = makeSelectInteraction();
    duplicate.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmUserSelect(duplicate, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(duplicate.editReply).not.toHaveBeenCalled();
    expect(duplicate.followUp).not.toHaveBeenCalled();
  });

  test('client-side already-replied error continues when this object owns the acknowledgement', async () => {
    const int = makeSelectInteraction();
    int.deferred = true;
    const err = new Error('The reply to this interaction has already been sent or deferred.');
    err.code = 'InteractionAlreadyReplied';
    int.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmUserSelect(int, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).toHaveBeenCalledTimes(1);
    expect(int.editReply).toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('already acknowledged by this handler'),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('client-side already-replied error stops when this object does not own the acknowledgement', async () => {
    const duplicate = makeSelectInteraction();
    const err = new Error('The reply to this interaction has already been sent or deferred.');
    err.code = 'InteractionAlreadyReplied';
    duplicate.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmUserSelect(duplicate, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(duplicate.deferred).toBe(false);
    expect(duplicate.replied).toBe(false);
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(duplicate.editReply).not.toHaveBeenCalled();
  });

  test('two delivered copies produce one successful pick and no superseded card', async () => {
    const accepted = makeSelectInteraction();
    const duplicate = makeSelectInteraction();
    let rejectDuplicateAck;
    duplicate.deferUpdate.mockImplementationOnce(() => new Promise((_, reject) => {
      rejectDuplicateAck = reject;
    }));
    let transitionEntered;
    const transitionStarted = new Promise((resolve) => { transitionEntered = resolve; });
    let finishTransition;
    mockTransitionFlow.mockImplementationOnce(() => new Promise((resolve) => {
      finishTransition = resolve;
      transitionEntered();
    }));

    const deliveries = Promise.all([
      handleConfirmUserSelect(accepted, {
        flow_id: 'fid', row: { payload: initialPayload, version: 1 },
      }),
      handleConfirmUserSelect(duplicate, {
        flow_id: 'fid', row: { payload: initialPayload, version: 1 },
      }),
    ]);
    await transitionStarted;
    rejectDuplicateAck(new Error('Unknown interaction'));
    finishTransition({ result: 'ok', version: 2 });
    await deliveries;

    expect(mockTransitionFlow).toHaveBeenCalledTimes(1);
    expect(accepted.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Sending file/),
    }));
    const allEdits = [accepted, duplicate]
      .flatMap((int) => int.editReply.mock.calls.map(([payload]) => payload.content));
    expect(allEdits).not.toEqual(expect.arrayContaining([
      expect.stringMatching(/superseded/i),
    ]));
  });

  test('pick combining a bot AND sender → sender survives, bot is dropped, flow advances', async () => {
    const bot1 = '100000000000000099';
    const int = makeSelectInteraction({
      users: [makeUser(bot1, { bot: true }), makeUser(SENDER_ID)],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot/);
    expect(updated.content).toMatch(/Send includes you/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('all bots picked → re-prompt warning prepended to full confirm card (resource header preserved)', async () => {
    const int = makeSelectInteraction({
      users: [makeUser(u1, { bot: true })],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bots/);
    expect(updated.content).toMatch(/Sending file/);
    expect(updated.content).toMatch(/Expires/);
  });

  test('defense-in-depth: cap-exceeded pick rejected even though picker setMaxValues makes it unreachable today', async () => {
    const users = Array.from({ length: 26 }, (_, i) => makeUser(`1000000000000000${String(i).padStart(2, '0')}`));
    const int = makeSelectInteraction({ users });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Pick at most/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('partial-bot pick → transitionFlow with non-bot users + warning surfaces on card', async () => {
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const bot1 = '100000000000000099';
    const int = makeSelectInteraction({
      users: [
        makeUser(u1),
        makeUser(u2),
        makeUser(u3),
        makeUser(bot1, { bot: true }),
      ],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ recipientIds: [u1, u2, u3] }),
    }));
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('transitionFlow conflict → superseded message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/),
    }));
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/),
    }));
  });

  test('transitionFlow throw → targeted ephemeral retry, NOT generic "superseded" copy', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save your pick/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/transitionFlow threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('mentionable picker: role pick → role members expanded, flow advances with merged recipientIds', async () => {
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const role = ['role-eng', {
      id: 'role-eng',
      mentionable: true,
      members: new Map([
        [u2, { user: makeUser(u2) }],
        [u3, { user: makeUser(u3) }],
      ]),
    }];
    const int = makeSelectInteraction({
      users: [makeUser(u1)],
      roles: [role],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        recipientIds: expect.arrayContaining([u1, u2, u3]),
      }),
    }));
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds.length).toBe(3);
  });

  test('mentionable picker: @everyone role WITHOUT MENTION_EVERYONE → all-invalid branch surfaces gated warning', async () => {
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: false,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@everyone/);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: @everyone role WITH MENTION_EVERYONE → expands via guild.members.cache, flow advances', async () => {
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const bot1 = '100000000000000099';
    const cache = new Map([
      [u2, { user: makeUser(u2) }],
      [u3, { user: makeUser(u3) }],
      [bot1, { user: makeUser(bot1, { bot: true }) }],
    ]);
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
      guildMemberCache: cache,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const recipientIds = mockTransitionFlow.mock.calls[0][2].payload.recipientIds;
    expect(recipientIds.sort()).toEqual([u2, u3].sort());
    expect(recipientIds).not.toContain(bot1);
  });

  test('mentionable picker: bot-only role pick → all-invalid branch with role-specific reason (no silent swallow)', async () => {
    const bot1 = makeUser('100000000000000091', { bot: true });
    const bot2 = makeUser('100000000000000092', { bot: true });
    const botRole = ['role-bots', {
      id: 'role-bots',
      mentionable: true,
      members: new Map([[bot1.id, { user: bot1 }], [bot2.id, { user: bot2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [botRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/no non-bot members/i);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: sender via role pick → flow advances, selfIncluded flips on (#322 parity)', async () => {
    const senderUser = makeUser(SENDER_ID);
    const senderRole = ['role-sender', {
      id: 'role-sender',
      mentionable: true,
      members: new Map([[SENDER_ID, { user: senderUser }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [senderRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Send includes you/);
  });

  test('mentionable picker: sender via @everyone-cache expansion → selfIncluded flips on (parity with named-role path)', async () => {
    const senderMember = { user: makeUser(SENDER_ID) };
    const cache = new Map([[SENDER_ID, senderMember]]);
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
      guildMemberCache: cache,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Send includes you/);
  });

  test('mentionable picker: missing interaction.memberPermissions → canMentionEveryone defaults false, @everyone denied', async () => {
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: false,
    });
    int.memberPermissions = undefined;
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@everyone/);
    expect(updated.content).toMatch(/Mention Everyone/);
  });

  test.each([false, true])('bounded role expansion: partial valid=%s', async (partialValid) => {
    const members = new Map(Array.from({ length: 101 }, (_, i) => {
      const bot = makeUser(String(200000000000000000n + BigInt(i)), { bot: true });
      return [bot.id, { user: bot }];
    }));
    const int = makeSelectInteraction({
      users: partialValid ? [makeUser(u1)] : [],
      roles: [['large-role', { id: 'large-role', mentionable: true, members }]],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    if (partialValid) expect(mockTransitionFlow).toHaveBeenCalled();
    else expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls.at(-1)[0];
    expect(updated.content).toContain('Picked role(s) could not be fully checked — pick fewer or smaller roles');
    expect(updated.content).not.toContain('have no non-bot members');
    if (partialValid) expect(updated.content).toContain('At least 100 bot(s)');
  });

  test.each([false, true])('unavailable named role members: partial valid=%s', async (partialValid) => {
    const int = makeSelectInteraction({
      users: partialValid ? [makeUser(u1)] : [],
      roles: [['missing-role', { id: 'missing-role', mentionable: true }]],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    if (partialValid) expect(mockTransitionFlow).toHaveBeenCalled();
    else expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls.at(-1)[0];
    expect(updated.content).toContain('Picked role(s) could not be loaded — try again in a few seconds');
    expect(updated.content).not.toContain('Member cache not yet ready');
  });

  test('mentionable picker: partial-valid pick with cold-cache @everyone → flow advances AND everyoneCacheCold warning surfaces', async () => {
    const u1 = makeUser('100000000000000001');
    const int = makeSelectInteraction({
      users: [u1],
      roles: [],
      canMentionEveryone: true,
    });
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    expect(updated.content).toMatch(/expanded to 0 members/);
  });

  test('mentionable picker: guild without members.cache (cold cache after restart) → surfaces "Member cache not yet ready" reason', async () => {
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
    });
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: partial-valid role (humans + bots) → flow advances AND droppedFromRoles warning surfaces', async () => {
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000091', { bot: true });
    const mixedRole = ['role-mixed', {
      id: 'role-mixed',
      mentionable: true,
      members: new Map([[u1.id, { user: u1 }], [bot1.id, { user: bot1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [mixedRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot\(s\) filtered from picked role/i);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: bot user + bot-only-role pick → BOTH reasons surface (independent signals)', async () => {
    const directBot = makeUser('100000000000000099', { bot: true });
    const roleBot = makeUser('100000000000000091', { bot: true });
    const botRole = ['role-bots', {
      id: 'role-bots',
      mentionable: true,
      members: new Map([[roleBot.id, { user: roleBot }]]),
    }];
    const int = makeSelectInteraction({
      users: [directBot],
      roles: [botRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Cannot send to bots/);
    expect(updated.content).toMatch(/no non-bot members/i);
  });

  test('mentionable picker: multi-signal pick (bot user + denied non-mentionable role) → banner reasons follow renderRecipientWarnings ordering', async () => {
    const directBot = makeUser('100000000000000099', { bot: true });
    const u1 = makeUser('100000000000000001');
    const deniedRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [directBot],
      roles: [deniedRole],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    const botIdx = updated.content.indexOf('Cannot send to bots');
    const roleIdx = updated.content.indexOf('Non-mentionable role');
    expect(botIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThanOrEqual(0);
    expect(botIdx).toBeLessThan(roleIdx);
  });

  test('re-pick preserves personalMessageRaw + personalMessage through the spread', async () => {
    const payloadWithNote = {
      ...initialPayload,
      personalMessage: '\\*\\*hi\\*\\*',
      personalMessageRaw: '**hi**',
    };
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: '\\*\\*hi\\*\\*',
        personalMessageRaw: '**hi**',
      }),
    }));
  });

  test('mentionable picker: non-mentionable role WITHOUT MENTION_EVERYONE → all-invalid banner with role-specific reason', async () => {
    const u1 = makeUser('100000000000000001');
    const adminRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [adminRole],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Non-mentionable role requires/i);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/have the role marked as mentionable/i);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: multiple non-mentionable roles → banner uses plural noun + verb ("roles require")', async () => {
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const roleA = ['role-a', {
      id: 'role-a', name: 'admin-a', mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const roleB = ['role-b', {
      id: 'role-b', name: 'admin-b', mentionable: false,
      members: new Map([[u2.id, { user: u2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [roleA, roleB],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Non-mentionable roles require/i);
  });

  test('mentionable picker: non-mentionable role + valid user pick → partial-valid, warnings block lists role NAME', async () => {
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const adminRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin-team',
      mentionable: false,
      members: new Map([[u2.id, { user: u2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [u1],
      roles: [adminRole],
      canMentionEveryone: false,
    });
    int.guild.roles.cache.set('role-admin', { id: 'role-admin', name: 'admin-team', mentionable: false });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@admin-team/);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/role\.mentionable: true/);
  });

  test('mentionable picker: denied role with deleted-from-cache name → fallback "unknown-role" renders', async () => {
    const u1 = makeUser('100000000000000001');
    const denied = ['role-ghost', {
      id: 'role-ghost',
      name: 'ghost',
      mentionable: false,
      members: new Map(),
    }];
    const int = makeSelectInteraction({
      users: [u1],
      roles: [denied],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@unknown-role/);
  });

  test('mentionable picker: non-mentionable role + canMentionEveryone → expands normally (no deny)', async () => {
    const u1 = makeUser('100000000000000001');
    const role = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [role],
      canMentionEveryone: true,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
  });

  test('role pick → members.list() pre-warm fires', async () => {
    const role = { id: 'roleA', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleA', role]] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('users-only pick → members.list() pre-warm does NOT fire', async () => {
    const int = makeSelectInteraction({ users: [makeUser(u1)], roles: [] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('role pick + members.list() rejection is swallowed — flow continues', async () => {
    const role = { id: 'roleB', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleB', role]] });
    int.guild.members.list = jest.fn(async () => {
      const err = new Error('rate limited'); err.code = 429; throw err;
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
  });
});
