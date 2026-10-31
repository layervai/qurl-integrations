const {
  handleConfirmNoteModal,
  makeInteraction,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmNoteModal', () => {
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: ['100000000000000001'],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeModalInteraction({ inputValue = 'hello', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.fields = { getTextInputValue: jest.fn(() => inputValue) };
    return int;
  }

  test('failed acknowledgement stops before note flow mutation', async () => {
    const int = makeModalInteraction({ inputValue: 'hello' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmNoteModal(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path: defers, trims/sanitizes, persists, editReply re-renders', async () => {
    const int = makeModalInteraction({ inputValue: '  **bold** message  ' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: expect.stringMatching(/\\\*\\\*bold\\\*\\\* message/),
        personalMessageRaw: '**bold** message',
      }),
    }));
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/\\\*\\\*bold\\\*\\\* message/);
  });

  test('round-trip no-op: re-submit unchanged input → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeModalInteraction({ inputValue: '**bold**' });
    const existingPayload = {
      ...basePayload,
      personalMessage: '\\*\\*bold\\*\\*',
      personalMessageRaw: '**bold**',
    };
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: existingPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  const payloadWithNote = { ...basePayload, personalMessage: 'old note', personalMessageRaw: 'old note' };

  test('empty input on a payload with an existing note → personalMessage: null (clear)', async () => {
    const int = makeModalInteraction({ inputValue: '' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ personalMessage: null, personalMessageRaw: null }),
    }));
  });

  test('whitespace-only input on a payload with an existing note → personalMessage: null', async () => {
    const int = makeModalInteraction({ inputValue: '   \n  \t' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ personalMessage: null, personalMessageRaw: null }),
    }));
  });

  test('ZWSP-only input on a payload with an existing note → both fields null in lockstep', async () => {
    const zwsp = String.fromCharCode(0x200B);
    const int = makeModalInteraction({ inputValue: zwsp.repeat(3) });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: null,
        personalMessageRaw: null,
      }),
    }));
  });

  test('empty submit on a payload with NO existing note → no-op skip (already cleared)', async () => {
    const int = makeModalInteraction({ inputValue: '' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('conflict → superseded copy via editReply (post-deferUpdate)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
      components: [],
    }));
  });

  test('not_found → expired copy via editReply', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
    }));
  });

  test('getTextInputValue throws → ephemeral followUp ("could not read"), NO transitionFlow, existing note preserved', async () => {
    const int = makeModalInteraction({ inputValue: 'hi' });
    int.fields.getTextInputValue = jest.fn(() => {
      throw new Error('Unknown custom_id');
    });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not read your note input/i),
      flags: 64,
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('transitionFlow throw → ephemeral followUp (NOT update/reply post-defer)', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save your note/i),
      flags: 64,
    }));
    expect(int.reply).not.toHaveBeenCalled();
    expect(int.update).not.toHaveBeenCalled();
  });

  test('sibling-mutation merge: row carries an expiry change made during typing → persisted alongside the note', async () => {
    const int = makeModalInteraction({ inputValue: 'hello' });
    const rowAfterSiblingMenu = {
      ...basePayload,
      expiresIn: '7d',          // sibling-changed
      selfDestructSeconds: 30,  // sibling-changed
      version: 2,                // version bumped by the sibling
    };
    await handleConfirmNoteModal(int, {
      flow_id: 'fid',
      row: { payload: rowAfterSiblingMenu, version: 2 },
    });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 2, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: expect.stringMatching(/hello/),
        personalMessageRaw: 'hello',
        expiresIn: '7d',
        selfDestructSeconds: 30,
      }),
    }));
  });
});
