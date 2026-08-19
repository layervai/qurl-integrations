/**
 * Tests for runRound's per-round accounting in scripts/loadtest-standalone.js.
 *
 * Why this file exists: scripts/ is outside this app's jest
 * `collectCoverageFrom` (`src/**` only), and runRound's only caller, main(),
 * is behind `require.main === module`. tests/loadtest-mint-batches.test.js
 * covers the batch *plan* — planMintBatches is pure and exported — but the
 * stateful accounting wrapped around that plan is a different thing, and until
 * runRound was exported nothing could reach it:
 *
 *   - a failed re-upload must charge the batch and CONTINUE, never mint
 *     against the spent resource whose pool is what forced the re-upload;
 *   - `reuploads` / `reuploadFail` count attempts, while `reuploadMs`
 *     accumulates over BOTH populations because it sits outside the try/catch;
 *   - `mintMs` is accumulated per batch specifically so re-upload latency
 *     stays out of the mint figure.
 *
 * These are all counters on a summary line. Getting one wrong does not fail
 * the run — it misreports it, which is the same failure mode the re-upload leg
 * itself was added to fix (ok=10 fail=90 read as a service problem when it was
 * a script problem). So they are worth pinning.
 *
 * The connector is stubbed and the script's own call sites are left untouched,
 * so tests/loadtest-silent-failure.test.js still asserts the real shape of the
 * reUploadBuffer/mintLinks calls against the same source.
 *
 * Requiring the script does NOT run the load test: its CLI entry point is
 * behind `require.main === module`, matching tests/loadtest-target-guard.test.js.
 */

const fs = require('fs');
const os = require('os');
const path = require('path');

// jest.mock is hoisted above these declarations, but the factory is lazy — it
// runs on require, by which point they are initialized. The `mock` prefix is
// what lets the factory close over them at all.
const mockReUploadBuffer = jest.fn();
const mockMintLinks = jest.fn();
jest.mock('../src/connector', () => ({
  reUploadBuffer: mockReUploadBuffer,
  mintLinks: mockMintLinks,
}));

const TOKENS = 10; // TOKENS_PER_RESOURCE, asserted against the export below.

let tmpFile;

beforeAll(() => {
  // A real file: runRound reads it with fs.readFileSync before the first
  // upload. Small on purpose — the payload's size is irrelevant here, and
  // generateTestFile's 1MB is only about producing realistic load.
  tmpFile = path.join(os.tmpdir(), `loadtest-accounting-${process.pid}.bin`);
  fs.writeFileSync(tmpFile, Buffer.alloc(64, 'A'));
});

afterAll(() => {
  fs.rmSync(tmpFile, { force: true });
});

/**
 * Load the script with a chosen --count and a real --file.
 *
 * COUNT / FILE_PATH / INCLUDE_LOCATION are module-level constants resolved
 * from process.argv at require time, so the flags have to be in place before
 * the require and the module registry has to be fresh for each one.
 *
 * `--file` without `--location` selects the file leg and skips the location
 * leg, which is the leg under test.
 */
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

beforeEach(() => {
  mockReUploadBuffer.mockReset();
  mockMintLinks.mockReset();
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

    // 3 batches of 10: the first mints against the initial upload, the other
    // two each re-upload first. So 3 uploads total but only 2 `reuploads`.
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

    // The whole point of the leg: a fresh pool per batch. Reusing res-1
    // throughout is the bug it fixed.
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

    // Batch 2 is charged as failed and skipped; batch 3 still runs. Abandoning
    // the round instead would cost the remaining batches too.
    expect(r.fileFail).toBe(10);
    expect(r.fileLinks).toBe(20);
    expect(r.reuploads).toBe(1);
    expect(r.reuploadFail).toBe(1);

    // The `continue` is the assertion that matters: res-1's pool is spent, so
    // minting batch 2 against it would take quota_exceeded and report a mint
    // fault for what was an upload fault.
    expect(mockMintLinks).toHaveBeenCalledTimes(2);
    expect(mockMintLinks.mock.calls.map((c) => c[0])).toEqual(['res-1', 'res-3']);
  });

  it('logs the first re-upload failure only', async () => {
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
    expect(logged.filter((m) => m.includes('re-upload error'))).toHaveLength(1);
    expect(logged[0]).toContain('first blip');
  });
});

describe('runRound accounting — mint failures are a separate population', () => {
  it('still logs the first mint error on a round where a re-upload failed first', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockResolvedValueOnce(upload('res-1'))
      .mockRejectedValueOnce(new Error('blip'))
      .mockResolvedValueOnce(upload('res-3'));
    // Batch 1 mints fine; batch 3 fails.
    mockMintLinks
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(new Error('mint exploded'));

    const r = await runRound(1);

    // fileFail is already 10 from the failed re-upload by the time the mint
    // error happens. Keying the "logged yet?" check off fileFail would swallow
    // this message — the diagnostic for the round most in need of one.
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

    // Two 40ms re-uploads land in reuploadMs. mintMs is accumulated per batch
    // rather than wrapped around the loop, so none of that reaches it —
    // wrapping the loop would report ~80ms of upload time as mint latency.
    expect(r.reuploadMs).toBeGreaterThanOrEqual(70);
    expect(r.mintMs).toBeLessThan(35);
  });

  it('accumulates reuploadMs over failed attempts too', async () => {
    const { runRound } = loadWithCount(30);
    mockReUploadBuffer
      .mockImplementationOnce(slow(5, upload('res-1')))
      .mockImplementationOnce(slow(40, new Error('slow failure')))
      .mockImplementationOnce(slow(40, new Error('slow failure')));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    // Both attempts failed, so `reuploads` is 0 — but the time they spent is
    // real and is still charged, because the timer closes outside the
    // try/catch. reuploadMs is a different population from reuploads.
    expect(r.reuploads).toBe(0);
    expect(r.reuploadFail).toBe(2);
    expect(r.reuploadMs).toBeGreaterThanOrEqual(70);
  });

  it('charges the initial upload to uploadMs, not reuploadMs', async () => {
    const { runRound } = loadWithCount(10);
    mockReUploadBuffer.mockImplementation(slow(40, upload('res')));
    mockMintLinks.mockResolvedValue({});

    const r = await runRound(1);

    // A single batch needs no re-upload at all.
    expect(r.uploadMs).toBeGreaterThanOrEqual(35);
    expect(r.reuploads).toBe(0);
    expect(r.reuploadMs).toBe(0);
  });
});
