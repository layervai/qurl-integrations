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

const mockLogger = {
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
};
jest.mock('../src/logger', () => mockLogger);

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
    expect(mockEditDMToPastTense).toHaveBeenCalledWith('c1', 'm1');
    expect(mockEditDMToPastTense).toHaveBeenCalledWith('c2', 'm2');
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

  it('skips the second sweep while the first is in flight (re-entrancy guard)', async () => {
    // First sweep: rows + a deliberately slow editDMToPastTense so we can
    // launch a second sweepOnce() while the first is still awaiting.
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    let resolveEdit;
    const editPromise = new Promise(res => { resolveEdit = res; });
    mockEditDMToPastTense.mockReturnValue(editPromise);

    const first = sweepOnce();
    const second = sweepOnce();
    resolveEdit(true);
    await Promise.all([first, second]);

    // Only one edit should have fired — the second sweep was skipped on
    // entry. listExpiredUneditedDMs is called exactly once because the
    // second sweep returns before fetching rows.
    expect(mockEditDMToPastTense).toHaveBeenCalledTimes(1);
    expect(mockListExpiredUneditedDMs).toHaveBeenCalledTimes(1);
    expect(mockLogger.warn).toHaveBeenCalledWith(
      'Expired-DM-label sweep skipped (prior sweep still in flight)'
    );
  });

  it('after a sweep completes, the next sweep is allowed (guard releases on success)', async () => {
    // Regression check: the `finally` block must release the `sweeping`
    // flag, otherwise a single completed sweep would block all future
    // sweeps for the lifetime of the process.
    mockListExpiredUneditedDMs.mockReturnValue([]);
    await sweepOnce();
    await sweepOnce();
    expect(mockListExpiredUneditedDMs).toHaveBeenCalledTimes(2);
  });

  it('after a sweep throws, the next sweep is allowed (guard releases in finally)', async () => {
    // The list-failure branch returns inside the try; the finally block
    // must still release `sweeping` so a permanent DB error doesn't lock
    // the sweeper out forever.
    mockListExpiredUneditedDMs
      .mockImplementationOnce(() => { throw new Error('db down'); })
      .mockReturnValueOnce([]);
    await sweepOnce();
    await sweepOnce();
    expect(mockListExpiredUneditedDMs).toHaveBeenCalledTimes(2);
  });

  it('emits saturation warn when the sweep fills the BATCH ceiling', async () => {
    // BATCH = 50 rows ⇒ backlog signal. Build 50 unique-message-id rows.
    const rows = Array.from({ length: 50 }, (_, i) => ({
      id: i + 1,
      send_id: `s${i + 1}`,
      recipient_discord_id: `r${i + 1}`,
      dm_channel_id: `c${i + 1}`,
      dm_message_id: `m${i + 1}`,
    }));
    mockListExpiredUneditedDMs.mockReturnValue(rows);
    mockEditDMToPastTense.mockResolvedValue(true);

    await sweepOnce();

    expect(mockLogger.warn).toHaveBeenCalledWith(
      'Expired-DM-label sweep hit batch ceiling — backlog likely',
      expect.objectContaining({ processedRows: 50, batch: 50 }),
    );
  });

  it('distinguishes "edit succeeded but mark failed" from "edit failed"', async () => {
    // If the Discord edit lands but the DB mark then throws (transient
    // SQLite busy/lock), the sweeper must NOT log the same line as a
    // real edit failure — otherwise dashboards conflate self-healing
    // mark hiccups with actual edit failures and the apparent failure
    // rate inflates. The fix wraps the mark call in its own try/catch
    // and logs a distinct line that names the actual failure mode.
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    mockEditDMToPastTense.mockResolvedValue(true);
    mockMarkDMExpiredLabelEditedByMessageId.mockImplementationOnce(() => {
      throw new Error('SQLITE_BUSY: database is locked');
    });

    await sweepOnce();

    // Distinct warn line — the message text must NOT be the
    // edit-failed wording (which is what callers would scan logs for
    // if they were tracking edit failures).
    expect(mockLogger.warn).toHaveBeenCalledWith(
      expect.stringContaining('edit succeeded but DB mark failed'),
      expect.objectContaining({ messageId: 'm1' }),
    );
    // The edit-failed line must NOT have been logged for this row.
    expect(mockLogger.warn).not.toHaveBeenCalledWith(
      expect.stringContaining('Expired-DM-label edit failed'),
      expect.anything(),
    );
    // The row counts as edited because the Discord-side edit DID
    // land — next sweep finds the embed already past-tense and
    // idempotently re-marks via the markDMExpiredLabelEditedByMessageId
    // call from the already-past path. Confirm the happy-path info
    // log fires (edited=1, permanentFails=0).
    expect(mockLogger.info).toHaveBeenCalledWith(
      'Expired-DM-label sweep complete',
      expect.objectContaining({ edited: 1 }),
    );
  });

  it('separates permanent-fail signal from happy-path info log', async () => {
    // permanentFails > 0 must surface as warn, not just be folded into
    // the info line — dashboards key on level for alerting.
    mockListExpiredUneditedDMs.mockReturnValue([
      { id: 1, send_id: 's1', recipient_discord_id: 'r1', dm_channel_id: 'c1', dm_message_id: 'm1' },
    ]);
    mockEditDMToPastTense.mockResolvedValue(false);

    await sweepOnce();

    expect(mockLogger.warn).toHaveBeenCalledWith(
      'Expired-DM-label sweep recorded permanent failures',
      expect.objectContaining({ permanentFails: 1, edited: 0 }),
    );
    // No info-level "complete" line on the perm-fail path — the warn
    // carries the same shape.
    expect(mockLogger.info).not.toHaveBeenCalled();
  });
});
