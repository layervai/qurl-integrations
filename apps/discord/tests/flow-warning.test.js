const mockClaim = jest.fn();
jest.mock('../src/flow-state', () => ({ claimFlowWarning: (...args) => mockClaim(...args) }));

let warn;
const settle = () => new Promise(resolve => setImmediate(resolve));
const input = (id = 'flow', nonce = 'lifecycle', kind = 'expiry') => ({ flow_id: id, sendNonce: nonce, kind, value: '999d' });

beforeEach(() => {
  jest.resetModules();
  mockClaim.mockReset();
  warn = require('../src/flow-warning').warnFlowCorruptionOnce;
});

test('three immediate and later rerenders emit one warning', async () => {
  mockClaim.mockResolvedValue('claimed');
  const emit = jest.fn();
  for (let i = 0; i < 3; i++) warn(input(), emit);
  await settle();
  for (let i = 0; i < 3; i++) warn(input(), emit);
  await settle();
  expect(mockClaim).toHaveBeenCalledTimes(1);
  expect(emit).toHaveBeenCalledTimes(1);
});

test('durable claims preserve dedup through 64-entry eviction and a worker restart', async () => {
  const claimed = new Set();
  mockClaim.mockImplementation(async (...args) => {
    const key = JSON.stringify(args.slice(0, 3));
    if (claimed.has(key)) return 'seen';
    claimed.add(key);
    return 'claimed';
  });
  const emit = jest.fn();
  for (let i = 0; i < 65; i++) { warn(input(`flow-${i}`), emit); await settle(); }
  warn(input('flow-0'), emit);
  await settle();
  jest.resetModules();
  warn = require('../src/flow-warning').warnFlowCorruptionOnce;
  warn(input('flow-0'), emit);
  await settle();
  expect(mockClaim).toHaveBeenCalledTimes(67);
  expect(emit).toHaveBeenCalledTimes(65);
});

test('warning kinds and replacement lifecycle nonces have separate claims', async () => {
  mockClaim.mockResolvedValue('claimed');
  const emit = jest.fn();
  warn(input(), emit);
  warn(input('flow', 'lifecycle', 'self_destruct'), emit);
  warn(input('flow', 'replacement'), emit);
  await settle();
  expect(emit).toHaveBeenCalledTimes(3);
});

test('diagnostic work stays at four active claims with no queued backlog', async () => {
  let pending = [];
  mockClaim.mockImplementation(() => new Promise(resolve => pending.push(resolve)));
  const emit = jest.fn();
  for (let i = 0; i < 10; i++) warn(input(`flow-${i}`), emit);
  await settle();
  expect(mockClaim).toHaveBeenCalledTimes(4);
  for (const resolve of pending) resolve('claimed');
  await settle();
  pending = [];
  for (let i = 4; i < 8; i++) warn(input(`flow-${i}`), emit);
  await settle();
  expect(mockClaim).toHaveBeenCalledTimes(8);
  for (const resolve of pending) resolve('claimed');
  await settle();
  expect(emit).toHaveBeenCalledTimes(8);
});

test('storage conflicts and errors remain retryable without leaking errors to the renderer', async () => {
  mockClaim.mockRejectedValueOnce(new Error('storage unavailable')).mockResolvedValueOnce('retry').mockResolvedValue('claimed');
  const emit = jest.fn();
  for (let i = 0; i < 4; i++) { warn(input(), emit); await settle(); }
  expect(mockClaim).toHaveBeenCalledTimes(3);
  expect(emit).toHaveBeenCalledTimes(1);
});

test('legacy rows consult the durable marker after every rerender and lifecycle reuse', async () => {
  mockClaim.mockResolvedValueOnce('claimed').mockResolvedValueOnce('seen').mockResolvedValueOnce('claimed');
  const emit = jest.fn();
  for (let i = 0; i < 3; i++) { warn({ ...input(), sendNonce: undefined }, emit); await settle(); }
  expect(mockClaim).toHaveBeenCalledTimes(3);
  expect(emit).toHaveBeenCalledTimes(2);
});
