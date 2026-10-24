const fs = require('node:fs');
const path = require('node:path');
const { startMintFailureCanary } = require('../src/mint-failure-canary');
const { emitMintFailureAudit } = require('../src/mint-failure-audit');

let output;
beforeEach(() => {
  jest.useFakeTimers();
  output = jest.spyOn(console, 'log').mockImplementation(() => {});
});
afterEach(() => {
  jest.clearAllTimers();
  jest.useRealTimers();
  jest.restoreAllMocks();
});

test('disabled default and non-sandbox endpoints never emit or schedule', () => {
  for (const options of [
    {}, { enabled: false, endpoint: 'https://api.layerv.xyz' },
    { enabled: true, endpoint: 'https://api.layerv.ai' },
    { enabled: true, endpoint: 'https://api.layerv.xyz.evil.example' },
    { enabled: true, endpoint: 'invalid' },
  ]) {
    expect(startMintFailureCanary(options)).toBeNull();
  }
  expect(output).not.toHaveBeenCalled();
  expect(jest.getTimerCount()).toBe(0);
});

test('the real audit emitter labels one startup and hourly sample, and its timer stops', () => {
  const timer = startMintFailureCanary({ enabled: true, endpoint: 'https://api.layerv.xyz' });
  const sample = JSON.parse(output.mock.calls[0][0]);
  expect(sample.audit).toEqual({
    event: 'qurl_send_create_link_failure', agent: 'discord',
    send_id: 'TEST-mint-failure-canary', reason: 'upstream_5xx',
    api_code: null, status_code: 503, kind: 'file', synthetic: true,
  });
  jest.advanceTimersByTime(60 * 60 * 1000);
  expect(output).toHaveBeenCalledTimes(2);
  clearInterval(timer);
  jest.advanceTimersByTime(60 * 60 * 1000);
  expect(output).toHaveBeenCalledTimes(2);
});

test('ordinary failures keep their audit shape and quota failures stay silent', () => {
  emitMintFailureAudit({ status: 503 }, { sendId: 'ordinary', kind: 'file' });
  expect(JSON.parse(output.mock.calls[0][0]).audit).not.toHaveProperty('synthetic');
  emitMintFailureAudit({ apiCode: 'quota_exceeded' }, { sendId: 'quota', kind: 'file' });
  expect(output).toHaveBeenCalledTimes(1);
});

test('shutdown clears the canary before draining and startup keeps the shutdown guard', () => {
  const source = fs.readFileSync(path.join(__dirname, '../src/index.js'), 'utf8');
  const stop = source.indexOf('clearInterval(mintFailureCanaryTimer)');
  expect(stop).toBeGreaterThan(source.indexOf('async function gracefulShutdownTeardown'));
  expect(stop).toBeLessThan(source.indexOf("await tryClose('HTTP server'"));
  expect(source).toContain('if (!isShuttingDown) {\n    mintFailureCanaryTimer = startMintFailureCanary');
});


test('audit and warning failures cannot stop startup or later samples', () => {
  const logger = require('../src/logger');
  const warning = jest.spyOn(logger, 'warn').mockImplementationOnce(() => { throw new Error('warning detail'); });
  jest.spyOn(logger, 'audit').mockImplementationOnce(() => { throw new Error('untrusted detail'); });
  let timer;
  expect(() => { timer = startMintFailureCanary({ enabled: true, endpoint: 'https://api.layerv.xyz' }); }).not.toThrow();
  expect(output).not.toHaveBeenCalled();
  expect(warning).toHaveBeenCalledWith('TEST canary: mint-failure audit sample could not be emitted');
  expect(JSON.stringify(warning.mock.calls)).not.toContain('untrusted detail');
  expect(() => jest.advanceTimersByTime(60 * 60 * 1000)).not.toThrow();
  expect(output).toHaveBeenCalledTimes(1);
  clearInterval(timer);
});
