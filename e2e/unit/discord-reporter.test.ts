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

test('shows skipped assertions outside the pass/fail denominator', async () => {
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
  expect(body.embeds[0].fields[0].value).toBe('✅ 3/3 ⏭ 1');
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
  expect(body.embeds[0].fields[0].value).toBe('⚠️ 0 ran ⏭ 2');
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
  expect(body.embeds[0].description).toContain('❌ 1 test suite had execution errors, ✅ 1 passed');
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
    '❌ 2 test suites had execution errors, ❌ 1 failed, ✅ 0 passed',
  );
});

test('keeps assertion details when a suite also has an execution error', async () => {
  const reporter = new DiscordReporter({}, {});
  await reporter.onRunComplete(new Set(), {
    numPassedTests: 1,
    numFailedTests: 1,
    numTotalTests: 2,
    startTime: Date.now() - 1_000,
    testResults: [
      {
        testFilePath: '/workspace/e2e/tests/after-all-error.test.ts',
        numPassingTests: 1,
        numFailingTests: 1,
        testExecError: new Error('unhandled teardown error'),
        testResults: [
          { status: 'passed', title: 'healthy assertion' },
          { status: 'failed', title: 'failed assertion' },
        ],
      },
    ],
  });

  const [, request] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(request.body as string);
  expect(body.embeds[0].description).toContain(
    '❌ 1 test suite had execution errors, ❌ 1 failed, ✅ 1 passed',
  );
  expect(body.embeds[0].fields[0]).toEqual({
    name: 'after-all-error.test.ts',
    value: '❌ 1/2\n• suite execution error\n• failed assertion',
  });
});


function resultsFor(statuses: string[], title = 'test') {
  const assertions = statuses.map((status) => ({ status, title }));
  const passed = statuses.filter((status) => status === 'passed').length;
  const failed = statuses.filter((status) => status === 'failed').length;
  return {
    numPassedTests: passed, numFailedTests: failed, numTotalTests: statuses.length,
    startTime: Date.now() - 1000,
    testResults: [{
      testFilePath: '/workspace/e2e/tests/fixture.test.ts',
      numPassingTests: passed, numFailingTests: failed, testResults: assertions,
    }],
  };
}

async function report(results: ReturnType<typeof resultsFor>) {
  await new DiscordReporter().onRunComplete(new Set(), results);
  const request = fetchMock.mock.calls.at(-1)![1] as RequestInit;
  return JSON.parse(request.body as string).embeds[0];
}

test.each([
  [['passed'], 0x2ecc71, 'All 1 tests passed'],
  [['failed'], 0xe74c3c, '1 failed'],
  [[], 0xf1c40f, 'No tests ran'],
  [['pending', 'todo'], 0xf1c40f, 'All 2 tests skipped'],
])('selects the report color and summary for %j', async (statuses, color, description) => {
  const embed = await report(resultsFor(statuses as string[]));
  expect(embed.color).toBe(color);
  expect(embed.description).toContain(description);
});

test('limits expanded failures and reports the remaining count', async () => {
  const embed = await report(resultsFor(Array(9).fill('failed'), 'failure title'));
  expect(embed.fields[0].value.match(/• failure title/g)).toHaveLength(5);
  expect(embed.fields[0].value).toContain('… and 4 more');
});

test('truncates field text without splitting a surrogate pair', async () => {
  const title = 'a'.repeat(1015) + '😀';
  const embed = await report(resultsFor(['failed'], title));
  const value = embed.fields[0].value as string;
  expect(value.length).toBeLessThanOrEqual(1024);
  expect(value).not.toMatch(/[\uD800-\uDBFF]$/);
  expect(value).not.toContain('\uFFFD');
});

test('limits a large failed report and includes the run link and truncation note', async () => {
  const saved = { GITHUB_RUN_ID: process.env.GITHUB_RUN_ID, GITHUB_REPOSITORY: process.env.GITHUB_REPOSITORY,
    GITHUB_SERVER_URL: process.env.GITHUB_SERVER_URL };
  Object.assign(process.env, { GITHUB_RUN_ID: '123', GITHUB_REPOSITORY: 'layervai/qurl-integrations', GITHUB_SERVER_URL: 'https://github.com' });
  try {
    const results = resultsFor(['failed'], 'x'.repeat(2000));
    results.testResults = Array.from({ length: 25 }, (_, index) => ({ ...results.testResults[0], testFilePath: `/workspace/e2e/tests/file-${index}.test.ts` }));
    results.numFailedTests = 25; results.numTotalTests = 25;
    const embed = await report(results);
    expect(embed.fields.length).toBeLessThan(25);
    expect(embed.description).toContain(`showing ${embed.fields.length}/25 files`);
    expect(embed.description).toContain('[View run on GitHub](https://github.com/layervai/qurl-integrations/actions/runs/123)');
    const count = embed.title.length + embed.description.length + embed.footer.text.length +
      embed.fields.reduce((sum: number, field: { name: string; value: string }) => sum + field.name.length + field.value.length, 0);
    expect(count).toBeLessThanOrEqual(6000);
    for (const field of embed.fields) expect(field.value.length).toBeLessThanOrEqual(1024);
  } finally {
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key]; else process.env[key] = value;
    }
  }
});

test.each(['BOT_TOKEN', 'CHANNEL_ID'])('missing %s skips the post and never fails the run', async (key) => {
  const saved = process.env[key];
  const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
  delete process.env[key];
  try {
    const reporter = new DiscordReporter();
    await expect(reporter.onRunComplete(new Set(), resultsFor(['passed']))).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('skipping Discord post'));
    expect(reporter.getLastError()).toBeUndefined();
  } finally {
    if (saved === undefined) delete process.env[key]; else process.env[key] = saved;
    warn.mockRestore();
  }
});

test.each(['HTTP 503', 'network', 'AbortError'])('%s warns without changing the test result', async (failure) => {
  const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
  if (failure === 'HTTP 503') fetchMock.mockResolvedValue(new Response('unavailable', { status: 503 }));
  else fetchMock.mockRejectedValue(Object.assign(new Error('fixture failure'), { name: failure }));
  try {
    const reporter = new DiscordReporter();
    await expect(reporter.onRunComplete(new Set(), resultsFor(['passed']))).resolves.toBeUndefined();
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('test results NOT affected'));
    expect(reporter.getLastError()).toBeUndefined();
  } finally { warn.mockRestore(); }
});


test('an actual POST deadline aborts the request without failing the run', async () => {
  jest.useFakeTimers();
  const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
  let signal!: AbortSignal;
  fetchMock.mockImplementation((_url: string, init: RequestInit) => {
    signal = init.signal!;
    return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
  });
  try {
    const reporter = new DiscordReporter();
    const pending = reporter.onRunComplete(new Set(), resultsFor(['passed']));
    await jest.advanceTimersByTimeAsync(7999);
    expect(signal.aborted).toBe(false);
    await jest.advanceTimersByTimeAsync(1);
    await expect(pending).resolves.toBeUndefined();
    expect(signal.aborted).toBe(true);
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('timeout after 8000ms'));
    expect(reporter.getLastError()).toBeUndefined();
  } finally { jest.useRealTimers(); warn.mockRestore(); }
});

test('a report construction error is visible but does not fail the run', async () => {
  const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
  const results = resultsFor(['passed']);
  results.startTime = NaN;
  try {
    const reporter = new DiscordReporter();
    await expect(reporter.onRunComplete(new Set(), results)).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('failed to post embed'));
    expect(reporter.getLastError()).toBeUndefined();
  } finally { warn.mockRestore(); }
});
