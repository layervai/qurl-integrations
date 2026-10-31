const {
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmExpirySelect,
  makeInteraction,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmExpirySelect', () => {
  const u1 = '100000000000000001';
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeSelectInteraction({ value = '7d', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('failed acknowledgement stops before expiry flow mutation', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmExpirySelect(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path persists new expiresIn + re-renders', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ expiresIn: '7d', recipientIds: [u1] }),
      terminal: false,
      set_expires_at: expect.any(Number),
    }));
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/7 days/);
  });

  test('selfIncluded notice survives an expiry change re-render', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    const payloadWithSelf = { ...basePayload, selfIncluded: true };
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: payloadWithSelf, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfIncluded: true, expiresIn: '7d' }),
    }));
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Send includes you/);
  });

  test('no-op re-pick (same value as payload) → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeSelectInteraction({ value: '24h' });  // same as basePayload.expiresIn
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('forged off-set expiry value → reply warn BEFORE defer, NO transitionFlow', async () => {
    const int = makeSelectInteraction({ value: '999d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.deferUpdate).not.toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Unrecognized expiry/i),
      ephemeral: true,
    }));
  });

  test('conflict result → superseded copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
      components: [],
    }));
  });

  test('not_found result → expired copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
      components: [],
    }));
  });

  test('transitionFlow throw → ephemeral retry followUp, no superseded copy', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });

  test('preserves all other payload fields across transition + warns on corrupted selfDestructSeconds', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: '7d' });
    const payload = {
      ...basePayload,
      selfDestructSeconds: 60,
      personalMessage: 'hi',
      personalMessageRaw: 'hi',
    };
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        expiresIn: '7d',
        recipientIds: [u1],
        selfDestructSeconds: 60,
        personalMessage: 'hi',
        personalMessageRaw: 'hi',
        resourceType: 'file',
      }),
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/off-preset selfDestructSeconds/i),
      expect.objectContaining({ selfDestructSeconds: '60' })
    );
  });
});
