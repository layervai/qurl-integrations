const { setupSendPhases } = require('./helpers/send-pipeline-phases');
const phase = setupSendPhases();

beforeEach(() => { jest.clearAllMocks(); });

test('persist writes recipient identities before dispatch and returns the shared expiry', async () => {
  jest.spyOn(Date, 'now').mockReturnValue(1700000000000);
  try {
    expect(await phase.persistSendBatch(phase.interaction, { ...phase.params, ...phase.prepared }))
      .toEqual({ expiresAt: 1700086400 });
    expect(phase.db.recordQURLSendBatch).toHaveBeenCalledWith([expect.objectContaining({
      sendId: 'send-1', senderDiscordId: 'sender-1', recipientDiscordId: 'recipient-1',
      qurlId: 'q_aaaaaaaaaa1', guildId: 'guild-1', targetType: 'user',
    })], { initialSend: true });
    expect(phase.db.saveSendConfig).toHaveBeenCalledWith(expect.objectContaining({
      sendId: 'send-1', initialSend: true, connectorResourceId: 'resource-1',
      actualUrl: phase.params.locationUrl, locationName: 'Test',
    }));
    expect(phase.db.saveSendConfig.mock.invocationCallOrder[0])
      .toBeLessThan(phase.db.recordQURLSendBatch.mock.invocationCallOrder[0]);
    expect(phase.sendDM).not.toHaveBeenCalled();
  } finally { Date.now.mockRestore(); }
});

test('persist failure returns the error reply and records only non-secret orphan identities', async () => {
  const reply = { id: 'error-reply' };
  phase.interaction.editReply.mockResolvedValueOnce(reply);
  phase.db.recordQURLSendBatch.mockRejectedValueOnce(new Error('write unavailable'));
  const result = await phase.persistSendBatch(phase.interaction, { ...phase.params, ...phase.prepared });
  expect(await result.reply).toBe(reply);
  expect(result.expiresAt).toBeUndefined();
  expect(phase.logger.error).toHaveBeenCalledWith(expect.any(String), expect.objectContaining({
    orphanedResources: [{ resourceId: 'resource-1', qurlId: 'q_aaaaaaaaaa1' }],
  }));
  expect(JSON.stringify(phase.logger.error.mock.calls)).not.toContain('https://q.test/1');
});

test('driver returns the persistence error reply without dispatching', async () => {
  const reply = { id: 'error-reply' };
  phase.interaction.editReply.mockResolvedValueOnce(undefined).mockResolvedValueOnce(reply);
  phase.db.recordQURLSendBatch.mockRejectedValueOnce(new Error('write unavailable'));
  expect(await phase.executeSendPipeline(phase.interaction, phase.params)).toBe(reply);
  expect(phase.sendDM).not.toHaveBeenCalled();
});


test('config failure cleans fresh grants and returns a pending reply without persistence or dispatch', async () => {
  const reply = new Promise(() => {});
  phase.interaction.editReply.mockReturnValueOnce(reply);
  phase.db.saveSendConfig.mockRejectedValueOnce(new Error('config unavailable'));
  const result = await phase.persistSendBatch(phase.interaction, { ...phase.params, ...phase.prepared });
  expect(result.reply).toBe(reply);
  expect(phase.db.recordQURLSendBatch).not.toHaveBeenCalled();
  expect(phase.sendDM).not.toHaveBeenCalled();
  expect(phase.connector.revokeMintedLinks).toHaveBeenCalled();
});
