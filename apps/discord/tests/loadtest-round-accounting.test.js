
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
  revokeResource: jest.fn().mockResolvedValue({}),
}));

const TOKENS = 10; // MINT_BATCH_SIZE, asserted against the export below.

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
    expect(loadWithCount(10).MINT_BATCH_SIZE).toBe(TOKENS);
  });
});

describe('runRound accounting — one upload per round', () => {
  it('mints all bounded batches against the original resource', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockResolvedValueOnce(upload('res-1'));
    mockMintLinks.mockResolvedValue({});
    const r = await runRound(7);
    expect(mockReUploadBuffer).toHaveBeenCalledTimes(1);
    expect(mockReUploadBuffer.mock.calls[0][1]).toBe('loadtest-round7.bin');
    expect(mockMintLinks.mock.calls.map(c => c[0])).toEqual(['res-1', 'res-1', 'res-1']);
    expect(r.fileLinks).toBe(30);
    expect(r.fileFail).toBe(0);
    expect(r.reuploads).toBe(0);
    expect(r.reuploadFail).toBe(0);
  });

  it('charges only the failed mint batch and keeps the original resource', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockResolvedValueOnce(upload('res-1'));
    mockMintLinks.mockResolvedValueOnce({}).mockRejectedValueOnce(new Error('mint exploded')).mockResolvedValueOnce({});
    const r = await runRound(1);
    expect(r.fileFail).toBe(10);
    expect(r.fileLinks).toBe(20);
    expect(mockReUploadBuffer).toHaveBeenCalledTimes(1);
    expect(mockMintLinks.mock.calls.map(c => c[0])).toEqual(['res-1', 'res-1', 'res-1']);
    expect(console.error.mock.calls.map(c => String(c[0])).filter(m => m.includes('mint error'))).toEqual(['  File mint error x10: mint exploded']);
  });

  it('aborts before minting when the initial upload fails', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockRejectedValueOnce(new Error('upload failed'));
    await expect(runRound(1)).rejects.toThrow('upload failed');
    expect(mockMintLinks).not.toHaveBeenCalled();
  });

  it('logs repeated mint failures once with their full failed-link count', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockResolvedValueOnce(upload('res'));
    mockMintLinks.mockRejectedValue(new Error('always down'));
    const r = await runRound(1);
    expect(r.fileFail).toBe(30);
    expect(r.fileLinks).toBe(0);
    expect(console.error.mock.calls.map(c => String(c[0])).filter(m => m.includes('mint error'))).toEqual(['  File mint error x30: always down']);
  });

  it('keeps initial upload latency out of mint latency', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer.mockImplementation(() => new Promise(resolve => setTimeout(() => resolve(upload('res')), 40)));
    mockMintLinks.mockResolvedValue({});
    const r = await runRound(1);
    expect(r.uploadMs).toBeGreaterThanOrEqual(35);
    expect(r.reuploadMs).toBe(0);
    expect(r.mintMs).toBeLessThan(r.uploadMs / 2);
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
