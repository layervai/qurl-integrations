const {
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmSelfDestructSelect,
  makeInteraction,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmSelfDestructSelect', () => {
  const u1 = '100000000000000001';
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeSelectInteraction({ value = '60', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('failed acknowledgement stops before self-destruct flow mutation', async () => {
    const int = makeSelectInteraction({ value: '30' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmSelfDestructSelect(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path persists new selfDestructSeconds + re-renders', async () => {
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ selfDestructSeconds: 30 }),
      terminal: false,
    }));
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/30 seconds/);
  });

  test('no-op re-pick (same selfDestructSeconds as payload) → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('"no-timer" form-side sentinel → null (when changing FROM a previous timer)', async () => {
    const payloadWithTimer = { ...basePayload, selfDestructSeconds: 30 };
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: payloadWithTimer, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfDestructSeconds: null }),
    }));
  });

  test('unknown forged value → reply warn BEFORE defer, NO transitionFlow', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: '999999' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.deferUpdate).not.toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Unrecognized self-destruct/i),
      flags: 64,
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/forged off-set self-destruct/i),
      expect.objectContaining({ flow_id: 'fid', value: '999999' })
    );
  });

  test('legitimate "no-timer" value does NOT trigger forgery warn', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('conflict → superseded copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });

  test('not_found → expired copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
    }));
  });
});
