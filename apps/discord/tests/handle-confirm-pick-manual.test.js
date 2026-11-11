const {
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmPickManual,
  makeInteraction,
  mockDeleteFlow,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmPickManual', () => {
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const VOICE_CH = 'voice-ch-pm';

  const voicePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1, u2],
    recipientAliases: { [u1]: 'alice', [u2]: 'bob' },
    recipientMode: 'voice',
    voiceChannelId: VOICE_CH,
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    warningsBlock: '',
  };

  test('failed acknowledgement stops before manual-picker flow mutation', async () => {
    const int = makeInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmPickManual(int, {
      flow_id: 'fid', row: { payload: voicePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('clears recipientIds + recipientAliases and flips recipientMode → "picker"', async () => {
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({
        recipientMode: 'picker',
        recipients: [],
        selfIncluded: false,
      }),
      terminal: false,
    }));
  });

  test('preserves resourceType / expiresIn / personalMessage from the original payload', async () => {
    const int = makeInteraction();
    const payloadWithExtras = {
      ...voicePayload,
      expiresIn: '7d',
      selfDestructSeconds: 60,
      personalMessage: 'hi team',
    };
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: payloadWithExtras, version: 1 } });
    const newPayload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(newPayload.resourceType).toBe('file');
    expect(newPayload.expiresIn).toBe('7d');
    expect(newPayload.selfDestructSeconds).toBe(60);
    expect(newPayload.personalMessage).toBe('hi team');
    expect(newPayload.voiceChannelId).toBe(VOICE_CH);
  });

  test('corrupt resourceType → deleteFlow + "Card data is corrupted" copy', async () => {
    const int = makeInteraction();
    const corrupt = { ...voicePayload, resourceType: 'audio' };
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: corrupt, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Card data is corrupted/);
  });

  test('transitionFlow conflict → superseded message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/superseded/);
    expect(lastCall.components).toEqual([]);
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/expired/);
    expect(lastCall.components).toEqual([]);
  });

  test('transitionFlow synchronous throw → ephemeral retry followUp (DDB blip recovery)', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not switch to manual picker/i),
      flags: 64,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });
});
