const {
  SEND_STAGE_AWAITING_CONFIRM, handleConfirmExpirySelect, handleConfirmSelfDestructSelect,
  handleConfirmNoteModal, makeInteraction, mockLoadFlow, mockTransitionFlow,
} = require('./helpers/qurl-send-fixtures');

const payload = {
  resourceType: 'file', resourceLabel: 'x.png', recipientIds: ['100000000000000001'],
  expiresIn: '24h', selfDestructSeconds: null, personalMessage: null, sendNonce: '0123456789abcdef',
};
const cases = [
  ['expiry', handleConfirmExpirySelect, '7d'],
  ['self-destruct', handleConfirmSelfDestructSelect, '30'],
  ['note', handleConfirmNoteModal, null],
];
function interactionFor(value) {
  const interaction = makeInteraction();
  interaction.values = [value];
  interaction.fields = { getTextInputValue: jest.fn(() => 'new private note') };
  return interaction;
}
beforeEach(() => mockLoadFlow.mockReset());

test.each(cases)('%s conflict keeps the current card and discards the stale edit', async (_kind, handler, value) => {
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockResolvedValueOnce({ stage: SEND_STAGE_AWAITING_CONFIRM, version: 2,
    payload: { ...payload, expiresIn: '30d' } });
  const interaction = interactionFor(value);
  await handler(interaction, { flow_id: 'fid', row: { version: 1, payload } });
  expect(mockLoadFlow).toHaveBeenCalledWith('fid');
  expect(mockTransitionFlow).toHaveBeenCalledTimes(1);
  expect(interaction.editReply.mock.calls.at(-1)[0].components.length).toBeGreaterThan(0);
  expect(interaction.followUp).toHaveBeenCalledWith({
    content: expect.stringMatching(/another change landed first/i), flags: 64,
  });
  expect(JSON.stringify(interaction.editReply.mock.calls)).not.toContain('new private note');
});

test.each([null, { stage: 'another-stage', payload },
  { stage: SEND_STAGE_AWAITING_CONFIRM, payload: { ...payload, sendNonce: 'fedcba9876543210' } },
])('a missing or replaced send keeps the terminal fallback: %j', async fresh => {
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockResolvedValueOnce(fresh);
  const interaction = interactionFor('7d');
  await handleConfirmExpirySelect(interaction, { flow_id: 'fid', row: { version: 1, payload } });
  expect(interaction.editReply).toHaveBeenCalledWith({
    content: expect.stringMatching(/superseded/), components: [],
  });
});

test('a failed refresh preserves the card and asks for a retry', async () => {
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockRejectedValueOnce(new Error('temporary database error'));
  const interaction = interactionFor('7d');
  await handleConfirmExpirySelect(interaction, { flow_id: 'fid', row: { version: 1, payload } });
  expect(interaction.editReply).not.toHaveBeenCalled();
  expect(interaction.followUp).toHaveBeenCalledWith({ content: expect.stringMatching(/refresh.*try again/i), flags: 64 });
});

test('rapid expiry then timer conflict displays the committed expiry and keeps the card', async () => {
  const stale = { version: 1, payload };
  const first = interactionFor('7d');
  await handleConfirmExpirySelect(first, { flow_id: 'fid', row: stale });
  const committed = mockTransitionFlow.mock.calls[0][2].payload;
  expect(committed.expiresIn).toBe('7d');
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockResolvedValueOnce({ stage: SEND_STAGE_AWAITING_CONFIRM, version: 2, payload: committed });
  const second = interactionFor('30');
  await handleConfirmSelfDestructSelect(second, { flow_id: 'fid', row: stale });
  const card = second.editReply.mock.calls.at(-1)[0];
  expect(card.components.length).toBeGreaterThan(0);
  expect(card.content).toContain('7 days');
  expect(card.content).not.toContain('30 seconds');
  expect(mockTransitionFlow).toHaveBeenCalledTimes(2);
});

test.each([undefined, 'malformed'])('a missing or malformed original nonce cannot refresh a card: %s', async sendNonce => {
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockResolvedValueOnce({ stage: SEND_STAGE_AWAITING_CONFIRM,
    payload: { ...payload, sendNonce } });
  const interaction = interactionFor('7d');
  await handleConfirmExpirySelect(interaction, { flow_id: 'fid', row: { version: 1, payload: { ...payload, sendNonce } } });
  expect(interaction.editReply).toHaveBeenCalledWith({ content: expect.stringMatching(/superseded/), components: [] });
});

test('a Discord redraw rejection still reaches the private recovery notice', async () => {
  mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
  mockLoadFlow.mockResolvedValueOnce({ stage: SEND_STAGE_AWAITING_CONFIRM, payload });
  const interaction = interactionFor('7d');
  interaction.editReply.mockRejectedValueOnce(new Error('Discord temporarily unavailable'));
  await handleConfirmExpirySelect(interaction, { flow_id: 'fid', row: { version: 1, payload } });
  expect(interaction.followUp).toHaveBeenCalledWith({
    content: expect.stringMatching(/another change landed first/i), flags: 64,
  });
});
