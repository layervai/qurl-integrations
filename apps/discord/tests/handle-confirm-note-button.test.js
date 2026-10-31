const {
  handleConfirmNoteButton,
  makeInteraction,
  mockDeleteFlow,
  mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

describe('handleConfirmNoteButton', () => {
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeButtonInteraction(rest = {}) {
    const int = makeInteraction(rest);
    int.showModal = jest.fn().mockResolvedValue(undefined);
    return int;
  }

  test('opens modal — does NOT mutate flow state (no transitionFlow / deleteFlow)', async () => {
    const int = makeButtonInteraction();
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.showModal).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  test('modal pre-filled with RAW input (not sanitized) for round-trip safety', async () => {
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    const payload = {
      ...basePayload,
      personalMessage: '\\*\\*bold\\*\\*',  // sanitized form (would render literally)
      personalMessageRaw: '**bold**',         // what the user actually typed
    };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(int.showModal).toHaveBeenCalled();
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('**bold**');
  });

  test('modal pre-fills empty string when no personalMessage is set', async () => {
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('');
  });

  test('modal pre-fills empty for legacy flow rows missing personalMessageRaw', async () => {
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    const legacyPayload = { ...basePayload, personalMessage: '\\*\\*bold\\*\\*' };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('');
  });

  test('safe when clicked after recipients fully chosen (idempotent — no transitionFlow)', async () => {
    const int = makeButtonInteraction();
    const payload = { ...basePayload, recipientIds: ['100000000000000001', '100000000000000002'] };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload, version: 5 } });
    expect(int.showModal).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → ephemeral reply fallback (no silent "interaction failed" toast)', async () => {
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not open the note editor/i),
      ephemeral: true,
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → if fallback reply ALSO rejects, no unhandled rejection escapes', async () => {
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    int.reply.mockRejectedValueOnce(new Error('Already acked'));
    let unhandled = null;
    const listener = (reason) => { unhandled = reason; };
    process.on('unhandledRejection', listener);
    try {
      await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      await new Promise((resolve) => setImmediate(resolve));
      expect(unhandled).toBeNull();
    } finally {
      process.off('unhandledRejection', listener);
    }
    expect(int.showModal).toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalled();
  });
});
