const { setupSendPhases } = require('./helpers/send-pipeline-phases');
const phase = setupSendPhases();

beforeEach(() => { jest.clearAllMocks(); });
afterEach(() => { phase.setActiveFileSends(0); });

test('prepare mints recipient links without persisting or dispatching', async () => {
  const result = await phase.prepareSendResources(phase.interaction, phase.params);
  expect(result).toEqual({ ...phase.prepared, sendId: expect.any(String) });
  expect(phase.connector.mintLinks).toHaveBeenCalled();
  expect(phase.db.recordQURLSendBatch).not.toHaveBeenCalled();
  expect(phase.sendDM).not.toHaveBeenCalled();
});

test('prepare stops on upload failure and retains the reply result', async () => {
  const reply = { id: 'error-reply' };
  phase.interaction.editReply.mockResolvedValueOnce(undefined).mockResolvedValueOnce(reply);
  phase.connector.uploadJsonToConnector.mockRejectedValueOnce(new Error('upload unavailable'));
  const result = await phase.prepareSendResources(phase.interaction, phase.params);
  expect(await result.reply).toBe(reply);
  expect(result.qurlLinks).toBeUndefined();
  expect(phase.connector.mintLinks).not.toHaveBeenCalled();
});

test('file slot is released before a pending short-mint error reply settles', async () => {
  let resolveReply;
  const pendingReply = new Promise(resolve => { resolveReply = resolve; });
  phase.interaction.editReply.mockResolvedValueOnce(undefined).mockReturnValueOnce(pendingReply);
  phase.connector.downloadAndUpload.mockResolvedValueOnce({ resource_id: 'resource-1', fileBuffer: Buffer.from('file') });
  phase.connector.mintLinks.mockResolvedValueOnce([]);
  const result = await phase.prepareSendResources(phase.interaction, {
    ...phase.params, resourceType: 'file',
    attachment: { url: 'https://cdn.discordapp.com/file', name: 'file.txt', contentType: 'text/plain' },
  });
  expect(result.reply).toBe(pendingReply);
  expect(phase.getActiveFileSends()).toBe(0);
  resolveReply('reply');
  expect(await result.reply).toBe('reply');
});

test('driver returns the mint error reply without entering later phases', async () => {
  const reply = { id: 'error-reply' };
  phase.interaction.editReply.mockResolvedValueOnce(undefined).mockResolvedValueOnce(reply);
  phase.connector.uploadJsonToConnector.mockRejectedValueOnce(new Error('upload unavailable'));
  expect(await phase.executeSendPipeline(phase.interaction, phase.params)).toBe(reply);
  expect(phase.db.recordQURLSendBatch).not.toHaveBeenCalled();
  expect(phase.sendDM).not.toHaveBeenCalled();
});
