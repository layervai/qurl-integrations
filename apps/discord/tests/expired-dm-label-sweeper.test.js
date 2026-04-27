// Tests for the expired-dm-label sweeper. Covers:
//   - empty queue is a no-op
//   - happy path: edit + bulk-mark per unique message_id
//   - permanent failure: edit returns false → still mark (don't retry forever)
//   - transient failure: edit throws → DON'T mark (next sweep retries)
//   - dedupe: multiple rows sharing one message_id → one edit call
//   - DB list failure: caught + logged, no throw

const mockListExpiredUneditedDMs = jest.fn();
const mockMarkDMExpiredLabelEditedByMessageId = jest.fn();
jest.mock('../src/database', () => ({
  listExpiredUneditedDMs: mockListExpiredUneditedDMs,
  markDMExpiredLabelEditedByMessageId: mockMarkDMExpiredLabelEditedByMessageId,
}));

const mockEditDMToPastTense = jest.fn();
jest.mock('../src/discord', () => ({
  editDMToPastTense: mockEditDMToPastTense,
}));

// Stub commands.js — the sweeper imports the prefix constants from there.
// Inline mock keeps the test fast (commands.js drags discord.js, db, etc.).
jest.mock('../src/commands', () => ({
  EXPIRY_PREFIX_PRESENT: 'PRESENT_PREFIX_',
  EXPIRY_PREFIX_PAST: 'PAST_PREFIX_',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const { sweepOnce } = require('../src/expired-dm-label-sweeper');

beforeEach(() => {
  jest.clearAllMocks();
});

describe('expired-dm-label-sweeper.sweepOnce', () => {
  it('is a no-op when there are no expired unedited DMs', async () => {
    mockListExpiredUneditedDMs.mockReturnValue([]);
    await sweepOnce();
    expect(mockEditDMToPastTense).not.toHaveBeenCalled();
    expect(mockMarkDMExpiredLabelEditedByMessageId).not.toHaveBeenCalled();
  });

  it('edits each unique message and bulk-marks by message_id', async () => {
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
      { id: 2, send_id: 's2', recipient_discord_id: 'r2', dm_channel_id: 'c2', dm_message_id: 'm2' },
    ]);
    mockEditDMToPastTense.mockResolvedValue(true);

    await sweepOnce();

    expect(mockEditDMToPastTense).toHaveBeenCalledTimes(2);
    expect(mockEditDMToPastTense).toHaveBeenCalledWith('c1', 'm1', 'PRESENT_PREFIX_', 'PAST_PREFIX_');
    expect(mockEditDMToPastTense).toHaveBeenCalledWith('c2', 'm2', 'PRESENT_PREFIX_', 'PAST_PREFIX_');
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledTimes(2);
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledWith('m1');
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledWith('m2');
  });

  it('dedupes by dm_message_id — one edit call when multiple rows share a message', async () => {
    // Three qurl_sends rows for one consolidated DM (e.g. /qurl add
    // recipients delivered 3 links to the same recipient).
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
      { id: 2, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
      { id: 3, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    mockEditDMToPastTense.mockResolvedValue(true);

    await sweepOnce();

    expect(mockEditDMToPastTense).toHaveBeenCalledTimes(1);
    // markByMessageId is also called once — the SQL UPDATE matches all 3
    // rows because they share dm_message_id, so one call covers the group.
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledTimes(1);
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledWith('m1');
  });

  it('marks edited even when edit returns false (permanent Discord failure)', async () => {
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    // false = permanent failure (DM channel deleted, bot blocked, etc.).
    // Marking anyway prevents the sweeper from retrying every minute forever.
    mockEditDMToPastTense.mockResolvedValue(false);

    await sweepOnce();

    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledWith('m1');
  });

  it('does NOT mark when edit throws (transient failure → retry next sweep)', async () => {
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    mockEditDMToPastTense.mockRejectedValue(new Error('discord 500'));

    await sweepOnce();

    expect(mockMarkDMExpiredLabelEditedByMessageId).not.toHaveBeenCalled();
  });

  it('continues sweeping siblings when one row throws', async () => {
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
      { id: 2, send_id: 's2', recipient_discord_id: 'r2', dm_channel_id: 'c2', dm_message_id: 'm2' },
    ]);
    mockEditDMToPastTense
      .mockRejectedValueOnce(new Error('discord 500'))
      .mockResolvedValueOnce(true);

    await sweepOnce();

    // m1 throws → not marked; m2 succeeds → marked. Sweeper doesn't bail
    // on first error.
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledTimes(1);
    expect(mockMarkDMExpiredLabelEditedByMessageId).toHaveBeenCalledWith('m2');
  });

  it('returns silently when listExpiredUneditedDMs throws', async () => {
    mockListExpiredUneditedDMs.mockImplementation(() => {
      throw new Error('db down');
    });
    // Should NOT throw — DB outage is logged and the sweep ends without
    // attempting any edits.
    await expect(sweepOnce()).resolves.toBeUndefined();
    expect(mockEditDMToPastTense).not.toHaveBeenCalled();
  });
});
