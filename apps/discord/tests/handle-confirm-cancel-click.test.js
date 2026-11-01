const {
  SENDER_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmCancelClick,
  isOnCooldown,
  logger,
  makeInteraction,
  mockDeleteFlow,
  sendCooldowns,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmCancelClick', () => {
  test('failed acknowledgement stops before delete or cooldown mutation', async () => {
    const int = makeInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);

    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });

    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(sendCooldowns.get(SENDER_ID)).toBe(cooldownAt);
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path → version-gated deleteFlow + cooldown CLEARED + update', async () => {
    const int = makeInteraction();
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 3,
    }));
    expect(sendCooldowns.has(SENDER_ID)).toBe(false);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/cancelled/),
    }));
  });

  test('deleteFlow dedup loser → ephemeral message + cooldown PRESERVED (no clear)', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      flags: 64,
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    expect(sendCooldowns.get(SENDER_ID)).toBe(cooldownAt);
  });

  test('Cancel deleteFlow is version-fenced against picker race', async () => {
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 11 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 11,
    }));
  });

  test('deleteFlow throw → ephemeral retry, cooldown preserved (Send may still be in flight)', async () => {
    mockDeleteFlow.mockRejectedValueOnce(new Error('ddb gone'));
    sendCooldowns.set(SENDER_ID, Date.now());
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not cancel right now/),
      flags: 64,
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/deleteFlow threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });
});

describe('send-confirm cancel audit', () => {
  test('records dwell from original creation after the atomic delete wins', async () => {
    const now = Math.floor(Date.now() / 1000);
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: {
      version: 3, created_at: now - 60, expires_at: now + 180,
    } });
    expect(logger.audit).toHaveBeenCalledWith('send_confirm_cancelled', {
      flow_id: 'fid', user_id: SENDER_ID, dwell_seconds: expect.any(Number),
    });
    const dwell = logger.audit.mock.calls.find(([event]) => event === 'send_confirm_cancelled')[1].dwell_seconds;
    expect(dwell).toBeGreaterThanOrEqual(60);
    expect(dwell).toBeLessThan(65);
  });
  test.each(['lost', 'throw'])('%s delete does not count cancellation', async failure => {
    if (failure === 'lost') mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    else mockDeleteFlow.mockRejectedValueOnce(new Error('unavailable'));
    await handleConfirmCancelClick(makeInteraction(), { flow_id: 'fid', row: { version: 3 } });
    expect(logger.audit).not.toHaveBeenCalledWith('send_confirm_cancelled', expect.anything());
  });
});

test('cancel audit omits unknown dwell instead of emitting an invalid number', async () => {
  await handleConfirmCancelClick(makeInteraction(), { flow_id: 'fid', row: { version: 3 } });
  expect(logger.audit).toHaveBeenCalledWith('send_confirm_cancelled', { flow_id: 'fid', user_id: SENDER_ID });
});
