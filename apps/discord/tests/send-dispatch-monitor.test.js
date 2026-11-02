const { setupSendPhases } = require('./helpers/send-pipeline-phases');
const phase = setupSendPhases();

afterEach(() => { for (const monitor of phase.activeMonitors) monitor.stop(); });
beforeEach(() => { jest.clearAllMocks(); });

test('dispatch records delivery and starts confirmation management without minting or initial persistence', async () => {
  const collector = { on: jest.fn() };
  const createMessageComponentCollector = jest.fn().mockReturnValue(collector);
  phase.interaction.editReply.mockResolvedValueOnce({ createMessageComponentCollector });
  await phase.dispatchAndMonitor(phase.interaction, { ...phase.params, ...phase.prepared, expiresAt: 1700086400 });
  expect(phase.sendDM).toHaveBeenCalledWith('recipient-1', expect.any(Object));
  expect(phase.db.markSendDMDelivered).toHaveBeenCalled();
  expect(phase.db.saveSendConfig).toHaveBeenCalledWith(expect.objectContaining({ sendId: 'send-1' }));
  expect(createMessageComponentCollector).toHaveBeenCalled();
  expect(collector.on.mock.calls.map(([event]) => event)).toEqual(['collect', 'end']);
  expect(phase.activeMonitors.size).toBe(1);
  expect(phase.connector.mintLinks).not.toHaveBeenCalled();
  expect(phase.db.recordQURLSendBatch).not.toHaveBeenCalled();
});

test('dispatch stops the monitor when the confirmation reply fails', async () => {
  phase.interaction.editReply.mockRejectedValueOnce(new Error('confirmation unavailable'));
  await expect(phase.dispatchAndMonitor(phase.interaction, {
    ...phase.params, ...phase.prepared, expiresAt: 1700086400,
  })).rejects.toThrow('confirmation unavailable');
  expect(phase.activeMonitors.size).toBe(0);
  expect(phase.sendDM).toHaveBeenCalledTimes(1);
});
