const DiscordReporter = require('../helpers/discord-reporter.js');
export {};

const originalFetch = global.fetch;
const originalEnv = {
  BOT_TOKEN: process.env.BOT_TOKEN,
  CHANNEL_ID: process.env.CHANNEL_ID,
  MINT_API_URL: process.env.MINT_API_URL,
};

const fetchMock = jest.fn();

beforeEach(() => {
  fetchMock.mockReset();
  fetchMock.mockResolvedValue(new Response('', { status: 200 }));
  global.fetch = fetchMock as typeof fetch;
  process.env.BOT_TOKEN = 'test-token';
  process.env.CHANNEL_ID = 'test-channel';
  process.env.MINT_API_URL = 'https://api.layerv.xyz/v1/qurls';
});

afterAll(() => {
  global.fetch = originalFetch;
  for (const [key, value] of Object.entries(originalEnv)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
});

test('renders Jest 30 per-file totals from assertion results', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 1,
    numFailedTests: 3,
    numTotalTests: 4,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/file-revoke.test.ts',
        numPassingTests: 1,
        numFailingTests: 3,
        numPendingTests: 0,
        numTodoTests: 0,
        testResults: [
          { status: 'passed', title: 'upload file' },
          { status: 'failed', title: 'upload file → view 200 → revoke' },
          { status: 'failed', title: 'distinct-per-viewer watermark' },
          { status: 'failed', title: 'single-use enforced' },
        ],
      },
    ],
  });

  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].fields).toEqual([
    {
      name: 'file-revoke.test.ts',
      value:
        '❌ 1/4\n' +
        '• upload file → view 200 → revoke\n' +
        '• distinct-per-viewer watermark\n' +
        '• single-use enforced',
    },
  ]);
});

test('includes skipped assertions in the per-file total', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 3,
    numFailedTests: 0,
    numTotalTests: 4,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/smoke.test.ts',
        numPassingTests: 3,
        numFailingTests: 0,
        testResults: [
          { status: 'passed', title: 'first' },
          { status: 'passed', title: 'second' },
          { status: 'passed', title: 'third' },
          { status: 'pending', title: 'later' },
        ],
      },
    ],
  });

  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].fields[0].value).toBe('✅ 3/4 ⏭ 1');
});

test('does not render an all-skipped file as green', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 0,
    numFailedTests: 0,
    numTotalTests: 2,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/smoke.test.ts',
        numPassingTests: 0,
        numFailingTests: 0,
        testResults: [
          { status: 'pending', title: 'later' },
          { status: 'todo', title: 'eventually' },
        ],
      },
    ],
  });

  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].fields[0].value).toBe('⚠️ 0/2 ⏭ 2');
});

test('renders a suite execution error as a red failure', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 1,
    numFailedTests: 0,
    numTotalTests: 1,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/smoke.test.ts',
        numPassingTests: 1,
        numFailingTests: 0,
        testResults: [{ status: 'passed', title: 'healthy test' }],
      },
      {
        testFilePath: '/workspace/e2e/tests/file-revoke.test.ts',
        numPassingTests: 0,
        numFailingTests: 0,
        testExecError: new Error('module failed to load'),
        testResults: [],
      },
    ],
  });

  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].color).toBe(0xe74c3c);
  expect(body.embeds[0].description).toContain('❌ 1 test suite failed to run, ✅ 1 passed');
  expect(body.embeds[0].fields[1]).toEqual({
    name: 'file-revoke.test.ts',
    value: '❌ failed to run',
  });
});

test('derives plural suite crashes and failed-test counts from per-file results', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 0,
    numFailedTests: 1,
    numTotalTests: 1,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/failed.test.ts',
        numPassingTests: 0,
        numFailingTests: 1,
        testResults: [{ status: 'failed', title: 'failed assertion' }],
      },
      {
        testFilePath: '/workspace/e2e/tests/load-one.test.ts',
        numPassingTests: 0,
        numFailingTests: 0,
        testExecError: new Error('first load failure'),
        testResults: [],
      },
      {
        testFilePath: '/workspace/e2e/tests/load-two.test.ts',
        numPassingTests: 0,
        numFailingTests: 0,
        testExecError: new Error('second load failure'),
        testResults: [],
      },
    ],
  });

  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].description).toContain(
    '❌ 2 test suites failed to run, ❌ 1 failed, ✅ 0 passed',
  );
});
