
const fs = require('fs');
const os = require('os');
const path = require('path');

const mockReUploadBuffer = jest.fn();
const mockMintLinks = jest.fn();
jest.mock('../src/connector', () => ({
  reUploadBuffer: mockReUploadBuffer,
  mintLinks: mockMintLinks,
}));

const mockCreateOneTimeLink = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: mockCreateOneTimeLink,
  deleteLink: jest.fn().mockResolvedValue({}),
}));

const TOKENS = 10; // TOKENS_PER_RESOURCE, asserted against the export below.

let tmpFile;

beforeAll(() => {
  tmpFile = path.join(os.tmpdir(), `loadtest-accounting-${process.pid}.bin`);
  fs.writeFileSync(tmpFile, Buffer.alloc(64, 'A'));
});

afterAll(() => {
  fs.rmSync(tmpFile, { force: true });
});

function loadWithCount(count) {
  const savedArgv = process.argv;
  let mod;
  try {
    process.argv = ['node', 'loadtest-standalone.js', '--count', String(count), '--file', tmpFile];
    jest.isolateModules(() => {
      mod = require('../scripts/loadtest-standalone');
    });
  } finally {
    process.argv = savedArgv;
  }
  return mod;
}

const upload = (id) => ({ resource_id: id });

function loadWithBothLegs(count) {
  const savedArgv = process.argv;
  let mod;
  try {
    process.argv = [
      'node', 'loadtest-standalone.js',
      '--count', String(count), '--file', tmpFile, '--location',
    ];
    jest.isolateModules(() => {
      mod = require('../scripts/loadtest-standalone');
    });
  } finally {
    process.argv = savedArgv;
  }
  return mod;
}

const locationLink = () => ({ resource_id: 'r_abcdefghijk' });

beforeEach(() => {
  mockReUploadBuffer.mockReset();
  mockMintLinks.mockReset();
  mockCreateOneTimeLink.mockReset();
  jest.spyOn(console, 'error').mockImplementation(() => {});
  jest.spyOn(console, 'log').mockImplementation(() => {});
});

afterEach(() => {
  jest.restoreAllMocks();
});

describe('runRound accounting — pool depth', () => {
  it('agrees with the constant the batch plan is built from', () => {
    expect(loadWithCount(10).TOKENS_PER_RESOURCE).toBe(TOKENS);
  });
});

describe('runRound accounting — all uploads succeed', () => {
  it('counts one re-upload per drained pool and mints every batch', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockResolvedValueOnce(upload('res-2'))
      .mockResolvedValueOnce(upload('res-3'));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(mockReUploadBuffer).toHaveBeenCalledTimes(3);
    expect(r.reuploads).toBe(2);
    expect(r.reuploadFail).toBe(0);
    expect(r.fileLinks).toBe(30);
    expect(r.fileFail).toBe(0);
  });

  it('mints each batch against the resource its own re-upload produced', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockResolvedValueOnce(upload('res-2'))
      .mockResolvedValueOnce(upload('res-3'));
    mockMintLinks.mockResolvedValue({});

    await runRound(1);

    expect(mockMintLinks.mock.calls.map((c) => c[0])).toEqual(['res-1', 'res-2', 'res-3']);
  });

  it('registers every resource in a round under one filename series', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockResolvedValue(upload('res'));
    mockMintLinks.mockResolvedValue({});

    await runRound(7);

    const names = mockReUploadBuffer.mock.calls.map((c) => c[1]);
    expect(names).toEqual(Array(3).fill('loadtest-round7.bin'));
  });
});

describe('runRound accounting — a re-upload fails', () => {
  it('charges the batch and continues without minting against the spent resource', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockRejectedValueOnce(new Error('connector blip'))
      .mockResolvedValueOnce(upload('res-3'));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(r.fileFail).toBe(10);
    expect(r.fileLinks).toBe(20);
    expect(r.reuploads).toBe(1);
    expect(r.reuploadFail).toBe(1);

    expect(mockMintLinks).toHaveBeenCalledTimes(2);
    expect(mockMintLinks.mock.calls.map((c) => c[0])).toEqual(['res-1', 'res-3']);
  });

  it('reports every distinct re-upload failure, weighted by attempts', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockRejectedValueOnce(new Error('first blip'))
      .mockRejectedValueOnce(new Error('second blip'));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(r.reuploadFail).toBe(2);
    expect(r.fileFail).toBe(20);
    const logged = console.error.mock.calls.map((c) => String(c[0]));
    expect(logged.filter((m) => m.includes('re-upload error'))).toEqual([
      '  File re-upload error x1: first blip',
      '  File re-upload error x1: second blip',
    ]);
  });
});

describe('runRound accounting — mint failures are a separate population', () => {
  it('still logs the first mint error on a round where a re-upload failed first', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockRejectedValueOnce(new Error('blip'))
      .mockResolvedValueOnce(upload('res-3'));
    mockMintLinks
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(new Error('mint exploded'));

    const r = await runRound(1);

    expect(r.fileFail).toBe(20);
    expect(r.fileLinks).toBe(10);
    const logged = console.error.mock.calls.map((c) => String(c[0]));
    expect(logged.filter((m) => m.includes('mint error'))).toHaveLength(1);
    expect(logged.find((m) => m.includes('mint error'))).toContain('mint exploded');
  });

  it('logs the first mint error only, across batches', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockResolvedValue(upload('res'));
    mockMintLinks.mockRejectedValue(new Error('always down'));

    const r = await runRound(1);

    expect(r.fileFail).toBe(30);
    expect(r.fileLinks).toBe(0);
    expect(r.reuploads).toBe(2);
    const logged = console.error.mock.calls.map((c) => String(c[0]));
    expect(logged.filter((m) => m.includes('mint error'))).toHaveLength(1);
  });
});

describe('runRound accounting — latency figures', () => {
  const slow = (ms, value) => () => new Promise((resolve, reject) => {
    setTimeout(() => (value instanceof Error ? reject(value) : resolve(value)), ms);
  });

  it('keeps re-upload latency out of the mint figure', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockImplementation(slow(40, upload('res')));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(r.reuploadMs).toBeGreaterThanOrEqual(70);
    expect(r.mintMs).toBeLessThan(r.reuploadMs / 2);
  });

  it('accumulates reuploadMs over failed attempts too', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockImplementationOnce(slow(5, upload('res-1')))
      .mockImplementationOnce(slow(40, new Error('slow failure')))
      .mockImplementationOnce(slow(40, new Error('slow failure')));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(r.reuploads).toBe(0);
    expect(r.reuploadFail).toBe(2);
    expect(r.reuploadMs).toBeGreaterThanOrEqual(70);
  });

  it('charges the initial upload to uploadMs, not reuploadMs', async () => {
    const { runRound } = loadWithCount(10);
    mockReUploadBuffer.mockImplementation(slow(40, upload('res')));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    expect(r.uploadMs).toBeGreaterThanOrEqual(35);
    expect(r.reuploads).toBe(0);
    expect(r.reuploadMs).toBe(0);
  });
});

describe('runRound truncation — what --duration actually stops', () => {
  const EXPIRED = 1;

  it('skips the file leg entirely when the deadline has already passed', async () => {
    const { runRound, setRunDeadlineForTests } = loadWithCount(30);
    setRunDeadlineForTests(EXPIRED);

    const r = await runRound(1);

    expect(mockReUploadBuffer).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(r.fileLinks).toBe(0);
    expect(r.partial).toBe(true);
    expect(r.mintPartial).toBe(true);
  });

  it('stops minting mid-plan and charges only the batches it ran', async () => {
    const { runRound, setRunDeadlineForTests } = loadWithCount(30);
    setRunDeadlineForTests(Infinity);
    mockReUploadBuffer.mockResolvedValue(upload('res-1'));
    mockMintLinks.mockImplementationOnce(async () => {
      setRunDeadlineForTests(EXPIRED);
      return {};
    });

    const r = await runRound(1);

    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(r.fileLinks).toBe(10);
    expect(r.partial).toBe(true);
    expect(r.mintPartial).toBe(true);
  });

  it('keeps mintPartial false when the mint plan finished before the clock did', async () => {
    const { runRound, setRunDeadlineForTests } = loadWithBothLegs(10);
    setRunDeadlineForTests(Infinity);
    mockReUploadBuffer.mockResolvedValue(upload('res-1'));
    mockMintLinks.mockImplementationOnce(async () => {
      setRunDeadlineForTests(EXPIRED);
      return {};
    });
    mockCreateOneTimeLink.mockResolvedValue(locationLink());

    const r = await runRound(1);

    expect(r.fileLinks).toBe(10);        // mint plan ran in full
    expect(mockCreateOneTimeLink).not.toHaveBeenCalled();
    expect(r.locLinks).toBe(0);
    expect(r.partial).toBe(true);        // the ROUND was cut short
    expect(r.mintPartial).toBe(false);   // ...but its mint sample is complete
  });

  it('stops the location leg mid-way and marks only the round', async () => {
    const { runRound, setRunDeadlineForTests } = loadWithBothLegs(10);
    setRunDeadlineForTests(Infinity);
    mockReUploadBuffer.mockResolvedValue(upload('res-1'));
    mockMintLinks.mockResolvedValue({});
    let created = 0;
    mockCreateOneTimeLink.mockImplementation(async () => {
      if (++created === 3) setRunDeadlineForTests(EXPIRED);
      return locationLink();
    });

    const r = await runRound(1);

    expect(r.locLinks).toBe(3);
    expect(r.fileLinks).toBe(10);
    expect(r.partial).toBe(true);
    expect(r.mintPartial).toBe(false);
  });
});
