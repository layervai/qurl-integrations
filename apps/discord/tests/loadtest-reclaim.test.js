/**
 * Tests for the reclaim-ledger helpers in scripts/loadtest-standalone.js.
 *
 * The load test itself is unmockable live API traffic, but the ledger is the
 * mechanism the whole leak-prevention story rests on, and its invariants are
 * pure and cheap to pin:
 *
 *   - a torn final line (what a SIGKILL mid-append leaves) must not cost the
 *     rest of the ledger — that case IS the reason recovery mode exists, and
 *     a regression is invisible until the exact day it matters;
 *   - a missing ledger must be distinguishable from an empty one, because in
 *     recovery mode those mean opposite things and reporting "clean" for a
 *     mistyped path is how thousands of live resources get abandoned;
 *   - one failing revoke must not abandon the remaining thousands (the
 *     natural refactor to Promise.all breaks this silently);
 *   - an already-gone resource counts as reclaimed, or re-running --reclaim
 *     after a partial sweep can never report clean.
 */

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: jest.fn(),
}));

const { deleteLink } = require('../src/qurl');
const config = require('../src/config');
const {
  LEDGER_PATH,
  readLedger, pruneLedger, ledgerEndpoints, reclaim, parseReclaimArg, trackCreate, recordResource,
  reclaimOnce, resetReclaimStateForTests,
} = require('../scripts/loadtest-standalone');

let created = [];

function tempLedger(contents) {
  const p = path.join(os.tmpdir(), `loadtest-ledger-test-${process.pid}-${created.length}.jsonl`);
  fs.writeFileSync(p, contents);
  created.push(p);
  return p;
}

// Mirrors what recordResource actually writes, endpoint included — the
// tenancy guard is fail-closed, so a fixture without one is refused.
function line(id, extra = {}) {
  return `${JSON.stringify({
    resource_id: id, kind: 'location', endpoint: config.QURL_ENDPOINT, ...extra,
  })}\n`;
}

// An older or hand-edited entry carrying no provenance.
function bareLine(id) {
  return `${JSON.stringify({ resource_id: id, kind: 'location' })}\n`;
}

beforeEach(() => {
  jest.spyOn(console, 'log').mockImplementation(() => {});
  jest.spyOn(console, 'error').mockImplementation(() => {});
  deleteLink.mockReset();
  deleteLink.mockResolvedValue(undefined);
});

afterEach(() => {
  jest.restoreAllMocks();
  for (const p of created) fs.rmSync(p, { force: true });
  created = [];
});

describe('readLedger', () => {
  it('keeps every intact entry when the final line is torn', () => {
    // A hard kill mid-append leaves exactly this shape.
    const ledger = tempLedger(`${line('r_1')}${line('r_2')}{"resource_id":"r_3`);
    expect(readLedger(ledger)).toEqual(['r_1', 'r_2']);
  });

  it('returns null for a path that does not exist', () => {
    const missing = path.join(os.tmpdir(), `loadtest-ledger-absent-${process.pid}.jsonl`);
    expect(readLedger(missing)).toBeNull();
  });

  it('returns null for a directory rather than throwing EISDIR', () => {
    expect(readLedger(os.tmpdir())).toBeNull();
  });

  it('skips an entry carrying no resource_id', () => {
    const ledger = tempLedger(`${line('r_1')}${JSON.stringify({ kind: 'upload' })}\n`);
    expect(readLedger(ledger)).toEqual(['r_1']);
  });
});

describe('ledgerEndpoints', () => {
  it('collects the endpoints a ledger was written against', () => {
    const ledger = tempLedger(line('r_1', { endpoint: 'https://sandbox.example' }));
    expect([...ledgerEndpoints(ledger)]).toEqual(['https://sandbox.example']);
  });
});

describe('pruneLedger', () => {
  it('leaves only the outstanding ids behind', () => {
    const ledger = tempLedger(`${line('r_1')}${line('r_2')}`);
    pruneLedger(ledger, new Set(['r_2']));
    expect(readLedger(ledger)).toEqual(['r_2']);
  });

  it('truncates rather than deletes on a clean sweep, so the path stays readable', () => {
    const ledger = tempLedger(line('r_1'));
    pruneLedger(ledger, new Set());
    expect(fs.existsSync(ledger)).toBe(true);
    expect(readLedger(ledger)).toEqual([]);
  });

  it('preserves endpoint provenance, so the tenancy guard survives a prune', () => {
    // A prune that drops `endpoint` disarms the guard on exactly the
    // recovery re-run it exists to protect: ledgerEndpoints would come back
    // empty and the sweep would delete against whatever host is configured.
    const ledger = tempLedger(line('r_1', { endpoint: 'https://sandbox.example' }));
    pruneLedger(ledger, new Set(['r_1']));
    expect([...ledgerEndpoints(ledger)]).toEqual(['https://sandbox.example']);
  });

  it('keeps the whole original entry, not just the fields a reader happens to use', () => {
    const ledger = tempLedger(line('r_1', { endpoint: 'https://sandbox.example' }));
    pruneLedger(ledger, new Set(['r_1']));
    expect(JSON.parse(fs.readFileSync(ledger, 'utf8').trim())).toEqual({
      resource_id: 'r_1', kind: 'location', endpoint: 'https://sandbox.example',
    });
  });
});

describe('reclaimOnce', () => {
  beforeEach(() => resetReclaimStateForTests());
  afterAll(() => resetReclaimStateForTests());

  it('shares one sweep between concurrent callers instead of racing them', async () => {
    // The guarantee the memoization exists for: a Ctrl-C landing during the
    // end-of-run sweep must join it, not start a competitor that doubles the
    // delete rate and then exits out from under the first.
    const ledger = tempLedger(line('r_1'));
    const [first, second] = await Promise.all([reclaimOnce(ledger), reclaimOnce(ledger)]);
    expect(deleteLink).toHaveBeenCalledTimes(1);
    expect(first).toBe(second);
  });

  it('clears the memo on rejection so the error path can genuinely retry', async () => {
    // Without clearing, main().catch's fallback re-awaits the same rejected
    // promise — reading as a retry while structurally unable to retry.
    const ledger = tempLedger(line('r_1'));
    const spy = jest.spyOn(fs, 'readFileSync').mockImplementationOnce(() => {
      throw new Error('boom');
    });
    await expect(reclaimOnce(ledger)).rejects.toThrow('boom');
    spy.mockRestore();
    await expect(reclaimOnce(ledger)).resolves.toMatchObject({ revoked: 1, failed: 0 });
  });
});

describe('recordResource', () => {
  afterAll(() => fs.rmSync(LEDGER_PATH, { force: true }));

  it('writes an entry the ledger readers consume, endpoint included', () => {
    // Round-trips through the real writer rather than this file's `line()`
    // fixture. If the two drift, every tenancy-guard test above would still
    // pass while the guard's actual input no longer carried an endpoint.
    recordResource('r_roundtrip', 'upload');
    expect(readLedger(LEDGER_PATH)).toContain('r_roundtrip');
    expect([...ledgerEndpoints(LEDGER_PATH)]).toEqual([config.QURL_ENDPOINT]);
  });

  // A malformed id would otherwise be recorded, then fail every sweep with
  // "Invalid resource ID format" — a message matching neither 404 nor 410 —
  // so it would be retried forever instead of reported as unreclaimable.
  it.each([
    ['missing', undefined],
    ['empty', ''],
    ['non-string', 12345],
    ['malformed', 'not a valid id!'],
    // Charset-clean but missing the r_ prefix client.delete() requires: it
    // would fail every sweep with a message matching neither 404 nor 410.
    ['unprefixed', 'abc123'],
  ])('warns and records nothing for a %s resource_id', (_label, value) => {
    const before = fs.existsSync(LEDGER_PATH) ? fs.readFileSync(LEDGER_PATH, 'utf8') : '';
    recordResource(value, 'upload');
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('carried no usable resource_id'),
    );
    // The omission matters as much as the warning: recording a shape-rejected
    // id would make it fail every sweep forever, which is what the check
    // exists to prevent.
    const after = fs.existsSync(LEDGER_PATH) ? fs.readFileSync(LEDGER_PATH, 'utf8') : '';
    expect(after).toBe(before);
  });
});

describe('parseReclaimArg', () => {
  it('reports no request when the flag is absent', () => {
    expect(parseReclaimArg(['--count', '10'])).toEqual({ requested: false, path: null });
  });

  it('rejects a bare --reclaim rather than letting it start a load test', () => {
    // The worst outcome in the file: falling through here mints thousands of
    // resources when the operator asked to delete some.
    expect(parseReclaimArg(['--reclaim'])).toEqual({ requested: true, path: null });
  });

  it('rejects the next flag being taken as the ledger path', () => {
    expect(parseReclaimArg(['--reclaim', '--ledger', '/tmp/x.jsonl']))
      .toEqual({ requested: true, path: null });
  });

  it('accepts a real path', () => {
    expect(parseReclaimArg(['--reclaim', '/tmp/x.jsonl']))
      .toEqual({ requested: true, path: '/tmp/x.jsonl' });
  });

  it('recognizes the --reclaim=PATH form rather than starting a load test', () => {
    // An unmatched equals form reports no request at all and falls through to
    // the run loop — the same "meant to delete, minted thousands instead"
    // outcome the empty-value guard exists to prevent.
    expect(parseReclaimArg(['--reclaim=/tmp/x.jsonl']))
      .toEqual({ requested: true, path: '/tmp/x.jsonl' });
  });

  it('rejects an empty --reclaim= value', () => {
    expect(parseReclaimArg(['--reclaim='])).toEqual({ requested: true, path: null });
  });
});

describe('reclaim', () => {
  it('revokes the rest when one resource fails', async () => {
    const ledger = tempLedger(['r_1', 'r_2', 'r_3', 'r_4', 'r_5'].map((id) => line(id)).join(''));
    deleteLink.mockImplementation(async (id) => {
      if (id === 'r_2') throw new Error('qURL API DELETE /qurls/r_2 failed (500)');
    });

    const result = await reclaim(ledger);

    // Five attempts, not two: a single failure must not abandon the remainder.
    expect(deleteLink).toHaveBeenCalledTimes(5);
    expect(result).toMatchObject({ missing: false, revoked: 4, failed: 1 });
    // Only the failure survives, so a re-run sweeps just that one.
    expect(readLedger(ledger)).toEqual(['r_2']);
  });

  it('aggregates failures sharing a cause into one tally line', async () => {
    // callQurl embeds the resource id in every message, so keying the tally
    // on the raw message gives one bucket per id — a uniform 401 across
    // thousands of ids would print thousands of "1x" lines instead of one.
    const ledger = tempLedger(['r_1', 'r_2', 'r_3'].map((id) => line(id)).join(''));
    deleteLink.mockImplementation(async (id) => {
      throw new Error(`qURL API DELETE /qurls/${id} failed (401)`);
    });

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 0, failed: 3 });
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('3x qURL API DELETE /qurls/<id> failed (401)'),
    );
  });

  it('counts an already-gone resource as revoked, not failed', async () => {
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockRejectedValue(new Error('qURL API DELETE /qurls/r_1 failed (404)'));

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(readLedger(ledger)).toEqual([]);
  });

  it('revokes a repeated id once', async () => {
    const ledger = tempLedger(`${line('r_1')}${line('r_1')}${line('r_1')}`);
    await reclaim(ledger);
    expect(deleteLink).toHaveBeenCalledTimes(1);
  });

  it('reports a missing ledger instead of a clean sweep', async () => {
    const missing = path.join(os.tmpdir(), `loadtest-ledger-absent-${process.pid}-2.jsonl`);
    const result = await reclaim(missing);
    expect(result).toMatchObject({ missing: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });

  it('refuses to delete against a tenancy the ledger was not written against', async () => {
    const ledger = tempLedger(line('r_1', { endpoint: `${config.QURL_ENDPOINT}-somewhere-else` }));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ refused: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });

  it('refuses a ledger mixing two tenancies even when the current one is present', async () => {
    // Deletes carry no per-id host, so a ledger spanning two endpoints would
    // otherwise pass the guard and revoke the other tenancy's resources too.
    const ledger = tempLedger(
      line('r_1', { endpoint: config.QURL_ENDPOINT })
      + line('r_2', { endpoint: `${config.QURL_ENDPOINT}-elsewhere` }),
    );
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ refused: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });

  it('sweeps a ledger whose entries all name the current tenancy', async () => {
    const ledger = tempLedger(line('r_1', { endpoint: config.QURL_ENDPOINT }));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(deleteLink).toHaveBeenCalledWith('r_1');
  });

  it('drains an id appended after the first pass had already read the ledger', async () => {
    // The invariant the header comment leans on: a round still unwinding when
    // a sweep starts can append behind it, and a single-pass sweep would exit
    // having left that resource live.
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockImplementation(async (id) => {
      if (id === 'r_1') fs.appendFileSync(ledger, line('r_late'));
    });

    const result = await reclaim(ledger);

    expect(deleteLink).toHaveBeenCalledWith('r_late');
    expect(result).toMatchObject({ revoked: 2, failed: 0 });
    expect(readLedger(ledger)).toEqual([]);
  });

  it('waits for a create still in flight instead of finishing without it', async () => {
    // The interleaving the whole sweep-vs-run-loop fix exists for: `stopping`
    // cannot un-issue a request already awaiting a response, so a create can
    // resolve and record AFTER the drain's last on-disk pass. Re-reading the
    // ledger alone does not catch that — only waiting on the counter does.
    const ledger = tempLedger(line('r_1'));
    let release;
    const suspended = new Promise((resolve) => { release = resolve; });
    const creating = trackCreate(async () => {
      await suspended;
      fs.appendFileSync(ledger, line('r_late'));
    });

    const sweep = reclaim(ledger);
    // Let the sweep revoke r_1 and reach an empty pass with the create still
    // outstanding; without the in-flight wait it would resolve here.
    await new Promise((r) => { setTimeout(r, 250); });
    release();
    await creating;
    const result = await sweep;

    expect(deleteLink).toHaveBeenCalledWith('r_late');
    expect(result).toMatchObject({ revoked: 2, failed: 0 });
  });

  it('counts a 410 as already-gone, the same as a 404', async () => {
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockRejectedValue(new Error('qURL API DELETE /qurls/r_1 failed (410)'));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ revoked: 1, failed: 0 });
  });

  it('accepts an endpoint differing only by a trailing slash', async () => {
    const ledger = tempLedger(line('r_1', { endpoint: `${config.QURL_ENDPOINT}/` }));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(deleteLink).toHaveBeenCalledWith('r_1');
  });

  it('treats a ledger with content but no readable ids as corrupt, not clean', async () => {
    // Recovery mode exists because something already went wrong; reporting a
    // corrupt ledger as an all-clear is how live resources get abandoned.
    const ledger = tempLedger('{"resource_id":"r_1\n{"resou\n');
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ unreadable: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });

  it('refuses an entry that records no endpoint at all', async () => {
    // Missing provenance must read as foreign, not as absence of evidence —
    // otherwise the guard passes trivially on the ledger most likely to have
    // come from somewhere else.
    const ledger = tempLedger(bareLine('r_1'));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ refused: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });
});
