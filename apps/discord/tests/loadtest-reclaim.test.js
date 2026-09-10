
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: jest.fn(),
}));

jest.mock('../src/connector', () => ({
  mintLinks: jest.fn(),
  reUploadBuffer: jest.fn(),
}));

const { deleteLink } = require('../src/qurl');
const { resourcePath } = require('../src/utils/resource-id');
const { qurlApiError, qurlApiErrorMessage } = require('../src/utils/qurl-errors');
const config = require('../src/config');
const {
  LEDGER_PATH,
  preflightLedger,
  readLedger, pruneLedger, ledgerEndpoints, reclaim, parseReclaimArg, trackCreate, recordResource,
  reclaimOnce, resetReclaimStateForTests, resolveLedgerArg, resolveReclaimArg,
} = require('../scripts/loadtest-standalone');

let created = [];

function tempLedger(contents) {
  const p = path.join(os.tmpdir(), `loadtest-ledger-test-${process.pid}-${created.length}.jsonl`);
  fs.writeFileSync(p, contents);
  created.push(p);
  return p;
}

function absentLedgerPath(name) {
  const p = path.join(os.tmpdir(), `loadtest-ledger-absent-${process.pid}-${name}.jsonl`);
  created.push(p);
  return p;
}

function line(id, extra = {}) {
  return `${JSON.stringify({
    resource_id: id, kind: 'location', endpoint: config.QURL_ENDPOINT, ...extra,
  })}\n`;
}

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

describe('preflightLedger', () => {
  const isUnix = process.platform !== 'win32';

  it('creates the ledger rather than only probing for it', () => {
    const ledger = absentLedgerPath('preflight-create');
    preflightLedger(ledger);
    expect(fs.existsSync(ledger)).toBe(true);
  });

  (isUnix ? it : it.skip)('creates it owner-only', () => {
    const ledger = absentLedgerPath('preflight-mode');
    const priorUmask = process.umask(0o000);
    try {
      preflightLedger(ledger);
    } finally {
      process.umask(priorUmask);
    }
    expect(fs.statSync(ledger).mode & 0o777).toBe(0o600);
  });

  (isUnix ? it : it.skip)('leaves the mode of a ledger that already exists alone', () => {
    const ledger = tempLedger('');
    fs.chmodSync(ledger, 0o644);
    preflightLedger(ledger);
    expect(fs.statSync(ledger).mode & 0o777).toBe(0o644);
  });

  it('appends rather than truncates, so a resumed run keeps what is outstanding', () => {
    const ledger = tempLedger(line('r_kept'));
    preflightLedger(ledger);
    expect(readLedger(ledger)).toEqual(['r_kept']);
  });
});

describe('readLedger', () => {
  it('keeps every intact entry when the final line is torn', () => {
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
    const ledger = tempLedger(line('r_1'));
    const [first, second] = await Promise.all([reclaimOnce(ledger), reclaimOnce(ledger)]);
    expect(deleteLink).toHaveBeenCalledTimes(1);
    expect(first).toBe(second);
  });

  it('clears the memo on rejection so the error path can genuinely retry', async () => {
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
    recordResource('r_roundtrip', 'upload');
    expect(readLedger(LEDGER_PATH)).toContain('r_roundtrip');
    expect([...ledgerEndpoints(LEDGER_PATH)]).toEqual([config.QURL_ENDPOINT]);
  });

  it.each([
    ['missing', undefined],
    ['empty', ''],
    ['non-string', 12345],
    ['malformed', 'not a valid id!'],
    ['overlong', 'a'.repeat(1025)],
  ])('warns and records nothing for a %s resource_id', (_label, value) => {
    const before = fs.existsSync(LEDGER_PATH) ? fs.readFileSync(LEDGER_PATH, 'utf8') : '';
    recordResource(value, 'upload');
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('carried no usable resource_id'),
    );
    const after = fs.existsSync(LEDGER_PATH) ? fs.readFileSync(LEDGER_PATH, 'utf8') : '';
    expect(after).toBe(before);
  });
});

describe('runRound ledgering', () => {
  const { mintLinks, reUploadBuffer } = require('../src/connector');

  function loadWith(argvTail) {
    const savedArgv = process.argv;
    let mod;
    try {
      process.argv = ['node', 'loadtest-standalone.js', ...argvTail];
      jest.isolateModules(() => { mod = require('../scripts/loadtest-standalone'); });
    } finally {
      process.argv = savedArgv;
    }
    return mod;
  }

  it('records every parent a round creates, re-uploads included', async () => {
    const payload = path.join(os.tmpdir(), `loadtest-payload-${process.pid}.bin`);
    const ledger = path.join(os.tmpdir(), `loadtest-round-ledger-${process.pid}.jsonl`);
    fs.writeFileSync(payload, 'x');
    created.push(payload, ledger);

    mintLinks.mockReset();
    reUploadBuffer.mockReset();
    mintLinks.mockResolvedValue({});
    reUploadBuffer
      .mockResolvedValueOnce({ resource_id: 'res-1' })
      .mockResolvedValueOnce({ resource_id: 'res-2' })
      .mockResolvedValueOnce({ resource_id: 'res-3' });

    const mod = loadWith(['--count', '30', '--file', payload, '--ledger', ledger]);
    await mod.runRound(1);

    expect(reUploadBuffer).toHaveBeenCalledTimes(3);
    expect(mod.readLedger(ledger)).toEqual(['res-1', 'res-2', 'res-3']);
  });
});

describe('resolveLedgerArg', () => {
  const FALLBACK = '/tmp/generated-default.jsonl';

  it('falls back when the flag is absent', () => {
    expect(resolveLedgerArg(['--count', '10'], FALLBACK))
      .toEqual({ ledgerPath: FALLBACK, errors: [] });
  });

  it('reads the separated form', () => {
    expect(resolveLedgerArg(['--ledger', '/tmp/x.jsonl'], FALLBACK))
      .toEqual({ ledgerPath: '/tmp/x.jsonl', errors: [] });
  });

  it('reads the inline form, which the removed reader silently ignored', () => {
    expect(resolveLedgerArg(['--ledger=/tmp/x.jsonl'], FALLBACK))
      .toEqual({ ledgerPath: '/tmp/x.jsonl', errors: [] });
  });

  it('reports a flag given no value, keeping the fallback', () => {
    const { ledgerPath, errors } = resolveLedgerArg(['--ledger'], FALLBACK);
    expect(ledgerPath).toBe(FALLBACK);
    expect(errors).toHaveLength(1);
  });

  it('reports the next argument being a flag rather than consuming it', () => {
    const { errors } = resolveLedgerArg(['--ledger', '--location'], FALLBACK);
    expect(errors).toHaveLength(1);
  });

  it('reports a whitespace-only path rather than writing to it', () => {
    const { ledgerPath, errors } = resolveLedgerArg(['--ledger', '   '], FALLBACK);
    expect(ledgerPath).toBe(FALLBACK);
    expect(errors[0]).toContain('--ledger must name a file');
  });
});

describe('parseReclaimArg', () => {
  it('reports no request when the flag is absent', () => {
    expect(parseReclaimArg(['--count', '10'])).toEqual({ requested: false, path: null });
  });

  it('rejects a bare --reclaim rather than letting it start a load test', () => {
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
    expect(parseReclaimArg(['--reclaim=/tmp/x.jsonl']))
      .toEqual({ requested: true, path: '/tmp/x.jsonl' });
  });

  it('rejects an empty --reclaim= value', () => {
    expect(parseReclaimArg(['--reclaim='])).toEqual({ requested: true, path: null });
  });
});

describe('resolveReclaimArg', () => {
  it('forwards the mode parseReclaimArg read, adding only the refusal', () => {
    expect(resolveReclaimArg(['--reclaim'])).toEqual({
      requested: true, path: null, errors: [expect.stringContaining('--reclaim')],
    });
  });

  it('points at the temp directory rather than a hard-coded /tmp', () => {
    const [message] = resolveReclaimArg(['--reclaim']).errors;
    expect(message).toContain('--reclaim <tmpdir>/loadtest-ledger-');
    expect(message).not.toContain('/tmp/');
  });

  it('stays silent on the paths that are not a refusal', () => {
    expect(resolveReclaimArg(['--reclaim', '/tmp/x.jsonl']))
      .toEqual({ requested: true, path: '/tmp/x.jsonl', errors: [] });
    expect(resolveReclaimArg(['--count', '10']))
      .toEqual({ requested: false, path: null, errors: [] });
  });

  it('reports a whitespace-only path, quoting it so the fault is visible', () => {
    expect(resolveReclaimArg(['--reclaim', '   '])).toEqual({
      requested: true,
      path: '   ',
      errors: [expect.stringContaining(
        '--reclaim must name the ledger file to reclaim from, got "   "',
      )],
    });
  });

  it('applies the same refusal to the inline spelling', () => {
    expect(resolveReclaimArg(['--reclaim=  ']).errors)
      .toEqual([expect.stringContaining('got "  "')]);
  });
});

describe('reclaim', () => {
  it('revokes the rest when one resource fails', async () => {
    const ledger = tempLedger(['r_1', 'r_2', 'r_3', 'r_4', 'r_5'].map((id) => line(id)).join(''));
    deleteLink.mockImplementation(async (id) => {
      if (id === 'r_2') {
        throw new Error(qurlApiErrorMessage('DELETE', resourcePath(id), 500));
      }
    });

    const result = await reclaim(ledger);

    expect(deleteLink).toHaveBeenCalledTimes(5);
    expect(result).toMatchObject({ missing: false, revoked: 4, failed: 1 });
    expect(readLedger(ledger)).toEqual(['r_2']);
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('1 other resource(s) failed with potentially retryable errors'),
    );
  });

  it('aggregates failures sharing a cause into one tally line', async () => {
    const ledger = tempLedger(['r_1', 'r_2', 'r_3'].map((id) => line(id)).join(''));
    deleteLink.mockImplementation(async (id) => {
      throw new Error(qurlApiErrorMessage('DELETE', resourcePath(id), 401));
    });

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 0, failed: 3 });
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining(
        `3x ${qurlApiErrorMessage('DELETE', '/resources/<id>', 401)}`,
      ),
    );
  });

  it('keeps an ambiguous resource-route 404 for another reclaim attempt', async () => {
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockRejectedValue(
      qurlApiError('DELETE', resourcePath('r_1'), 404),
    );

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 0, failed: 1 });
    expect(readLedger(ledger)).toEqual(['r_1']);
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('1 resource(s) returned 404'),
    );
    expect(console.error).not.toHaveBeenCalledWith(
      expect.stringContaining('re-run with --reclaim'),
    );
  });

  it('keeps a legacy-ID 400 visible for another reclaim attempt', async () => {
    const ledger = tempLedger(line('r_legacy42'));
    deleteLink.mockRejectedValue(
      new Error(qurlApiErrorMessage('DELETE', resourcePath('r_legacy42'), 400)),
    );

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 0, failed: 1 });
    expect(readLedger(ledger)).toEqual(['r_legacy42']);
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('1 legacy resource ID(s) were rejected with 400'),
    );
    expect(console.error).not.toHaveBeenCalledWith(
      expect.stringContaining('re-run with --reclaim'),
    );
  });

  it('continues after an invalid non-string ledger ID and flags manual repair', async () => {
    const ledger = tempLedger(`${line(12345)}${line('valid-id')}`);
    deleteLink.mockImplementation(async (id) => {
      if (typeof id !== 'string') throw new Error('Invalid resource ID format: <number>');
    });

    const result = await reclaim(ledger);

    expect(deleteLink).toHaveBeenCalledTimes(2);
    expect(result).toMatchObject({ revoked: 1, failed: 1 });
    expect(readLedger(ledger)).toEqual([12345]);
    expect(console.error).toHaveBeenCalledWith(
      expect.stringContaining('1 invalid ledger resource ID(s) cannot be reclaimed automatically'),
    );
    expect(console.error).not.toHaveBeenCalledWith(
      expect.stringContaining('re-run with --reclaim'),
    );
  });

  it('does not read a 404 inside the resource ID as already gone', async () => {
    const ledger = tempLedger(line('abc404def'));
    deleteLink.mockRejectedValue(
      new Error(qurlApiErrorMessage('DELETE', resourcePath('abc404def'), 500)),
    );

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 0, failed: 1 });
    expect(readLedger(ledger)).toEqual(['abc404def']);
  });

  it('continues reclaiming after a non-Error rejection', async () => {
    const ledger = tempLedger(['r_1', 'r_2', 'r_3'].map((id) => line(id)).join(''));
    deleteLink.mockImplementation(async (id) => {
      if (id === 'r_1') return Promise.reject('network unavailable');
    });

    const result = await reclaim(ledger);

    expect(deleteLink).toHaveBeenCalledTimes(3);
    expect(result).toMatchObject({ revoked: 2, failed: 1 });
    expect(readLedger(ledger)).toEqual(['r_1']);
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
    const ledger = tempLedger(line('r_1'));
    let release;
    const suspended = new Promise((resolve) => { release = resolve; });
    const creating = trackCreate(async () => {
      await suspended;
      fs.appendFileSync(ledger, line('r_late'));
    });

    const sweep = reclaim(ledger);
    await new Promise((r) => { setTimeout(r, 250); });
    release();
    await creating;
    const result = await sweep;

    expect(deleteLink).toHaveBeenCalledWith('r_late');
    expect(result).toMatchObject({ revoked: 2, failed: 0 });
  });

  it('counts a structural 410 as already gone', async () => {
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockRejectedValue(
      qurlApiError('DELETE', resourcePath('r_1'), 410),
    );
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(readLedger(ledger)).toEqual([]);
  });

  it('counts a serialized 410 without structural status as already gone', async () => {
    const ledger = tempLedger(line('r_1'));
    deleteLink.mockRejectedValue(
      new Error(qurlApiErrorMessage('DELETE', resourcePath('r_1'), 410)),
    );

    const result = await reclaim(ledger);

    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(readLedger(ledger)).toEqual([]);
  });

  it('accepts an endpoint differing only by a trailing slash', async () => {
    const ledger = tempLedger(line('r_1', { endpoint: `${config.QURL_ENDPOINT}/` }));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ revoked: 1, failed: 0 });
    expect(deleteLink).toHaveBeenCalledWith('r_1');
  });

  it('treats a ledger with content but no readable ids as corrupt, not clean', async () => {
    const ledger = tempLedger('{"resource_id":"r_1\n{"resou\n');
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ unreadable: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });

  it('refuses an entry that records no endpoint at all', async () => {
    const ledger = tempLedger(bareLine('r_1'));
    const result = await reclaim(ledger);
    expect(result).toMatchObject({ refused: true });
    expect(deleteLink).not.toHaveBeenCalled();
  });
});
