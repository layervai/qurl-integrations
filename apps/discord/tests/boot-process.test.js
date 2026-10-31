const { spawnSync } = require('node:child_process');
const { mkdtempSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');
const bootEnvironment = require('./helpers/boot-environment');

jest.setTimeout(20000);

const PREVIOUS_KEY = Buffer.alloc(32, 8).toString('base64');
let directory;
let fence;

beforeAll(() => {
  directory = mkdtempSync(path.join(tmpdir(), 'discord-boot-'));
  fence = path.join(directory, 'network-fence.cjs');
  writeFileSync(fence, `
    const deny = () => { require('node:fs').writeSync(2, 'BOOT_TEST_NETWORK_BLOCKED'); process.exit(97); };
    require('node:net').Socket.prototype.connect = deny;
    require('node:net').Server.prototype.listen = deny;
    const dns = require('node:dns');
    dns.lookup = deny;
    dns.resolve = deny;
    dns.promises.lookup = deny;
    dns.promises.resolve = deny;
    globalThis.fetch = deny;
  `);
});
afterAll(() => rmSync(directory, { recursive: true, force: true }));

function boot(overrides = {}) {
  const env = bootEnvironment(directory, overrides);
  const result = spawnSync(process.execPath, ['--require', fence, path.resolve(__dirname, '../src/index.js')], {
    cwd: directory, env, encoding: 'utf8', timeout: 15000, killSignal: 'SIGKILL',
  });
  expect(result.error).toBeUndefined();
  return { status: result.status, output: result.stdout + result.stderr };
}

function expectBootFailure(overrides, message) {
  const result = boot(overrides);
  expect(result.output).not.toContain('BOOT_TEST_NETWORK_BLOCKED');
  expect(result.output).toContain(message);
  expect(result.status).toBe(1);
}

test.each(['development', 'staging', 'production'])('active OAuth without a state key fails before network in %s (#62)', (NODE_ENV) => {
  expectBootFailure({ NODE_ENV, QURL_OAUTH_STATE_SECRET: undefined, GITHUB_CLIENT_SECRET: 'unused-legacy-secret' }, 'no state-signing secret');
});
test('missing Discord credential fails before network (#192)', () => {
  expectBootFailure({ DISCORD_TOKEN: undefined }, 'DISCORD_TOKEN');
});
test('missing production metrics key fails before network (#192)', () => {
  expectBootFailure({ METRICS_TOKEN: undefined }, 'METRICS_TOKEN');
});
test('production requires the current key even with OAuth disabled (#192)', () => {
  expectBootFailure({ KEY_ENCRYPTION_KEY: undefined, AUTH0_DOMAIN: undefined }, 'KEY_ENCRYPTION_KEY');
});
test('OAuth requires encryption in staging too (#192)', () => {
  expectBootFailure({ NODE_ENV: 'staging', KEY_ENCRYPTION_KEY: undefined }, 'KEY_ENCRYPTION_KEY is missing');
});
test('malformed current encryption key fails before network (#192)', () => {
  expectBootFailure({ KEY_ENCRYPTION_KEY: 'not-base64' }, 'KEY_ENCRYPTION_KEY is malformed');
});
test('malformed previous encryption key fails before network (#191)', () => {
  expectBootFailure({ KEY_ENCRYPTION_KEY_PREV: 'not-base64' }, 'KEY_ENCRYPTION_KEY_PREV is malformed');
});
test('a previous key without a current key cannot boot (#191)', () => {
  expectBootFailure({ NODE_ENV: 'development', AUTH0_DOMAIN: undefined, KEY_ENCRYPTION_KEY: undefined, KEY_ENCRYPTION_KEY_PREV: PREVIOUS_KEY }, 'KEY_ENCRYPTION_KEY is required');
});
test('OAuth refuses an insecure production callback origin (#192)', () => {
  expectBootFailure({ BASE_URL: 'http://localhost:3000' }, 'BASE_URL');
});
test('valid current and previous keys pass boot gates and reach the network fence (#191)', () => {
  const result = boot({ KEY_ENCRYPTION_KEY_PREV: PREVIOUS_KEY });
  expect(result.output).toContain('BOOT_TEST_NETWORK_BLOCKED');
  expect(result.status).toBe(97);
});

test('development permits a local HTTP callback with valid OAuth secrets (#62)', () => {
  const result = boot({ NODE_ENV: 'development', BASE_URL: 'http://localhost:3000' });
  expect(result.output).toContain('BOOT_TEST_NETWORK_BLOCKED');
  expect(result.status).toBe(97);
});

test.each(['QURL_API_KEY', 'DETECT_TUNNEL_SLUG'].flatMap(name =>
  [undefined, ' \t', ...(name === 'QURL_API_KEY' ? ['PLACEHOLDER', ' placeholder '] : [])].map(value => [name, value])
))('enabled detect fails before network when %s = %p (#834)', (name, value) => {
  expectBootFailure({ DETECT_COMMAND_ENABLED: 'true', QURL_API_KEY: 'lv_live_synthetic_detect', DETECT_TUNNEL_SLUG: 'detect-fixture', [name]: value }, `DETECT_COMMAND_ENABLED=true but settings are missing/blank or QURL_API_KEY is the PLACEHOLDER seed value: ${name}.`);
});
test.each(['false', 'true'])('detect gate %s with matching configuration reaches the network fence (#834)', flag => {
  const result = boot({ DETECT_COMMAND_ENABLED: flag,
    ...(flag === 'true' ? { QURL_API_KEY: 'lv_live_synthetic_detect', DETECT_TUNNEL_SLUG: 'detect-fixture' } : {}),
  });
  expect(result.output).toContain('BOOT_TEST_NETWORK_BLOCKED');
  expect(result.status).toBe(97);
});
