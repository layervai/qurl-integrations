const saved = process.env.DISCORD_INSTALL_HASHED_STATE;

afterEach(() => {
  if (saved === undefined) delete process.env.DISCORD_INSTALL_HASHED_STATE;
  else process.env.DISCORD_INSTALL_HASHED_STATE = saved;
  jest.resetModules();
});

test.each([[undefined, false], ['false', false], ['true', true], [' TRUE ', true]])('install hash rollout setting %s is %s', (value, expected) => {
  if (value === undefined) delete process.env.DISCORD_INSTALL_HASHED_STATE;
  else process.env.DISCORD_INSTALL_HASHED_STATE = value;
  jest.resetModules();
  expect(require('../src/config').DISCORD_INSTALL_HASHED_STATE).toBe(expected);
});

test('disables only customer install for a malformed rollout setting', () => {
  process.env.DISCORD_INSTALL_HASHED_STATE = 'tru';
  jest.resetModules();
  const config = require('../src/config');
  expect(config.isDiscordInstallConfigured).toBe(false);
  expect(config.discordInstallNotConfiguredReason).toBe('DISCORD_INSTALL_HASHED_STATE must be true or false');
});
