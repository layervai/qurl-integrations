const saved = process.env.DISCORD_INSTALL_HASHED_STATE;

afterEach(() => {
  if (saved === undefined) delete process.env.DISCORD_INSTALL_HASHED_STATE;
  else process.env.DISCORD_INSTALL_HASHED_STATE = saved;
  jest.resetModules();
  jest.restoreAllMocks();
});

test.each([[undefined, false], ['false', false], ['true', true], [' true ', true]])('install hash rollout setting %s is %s', (value, expected) => {
  if (value === undefined) delete process.env.DISCORD_INSTALL_HASHED_STATE;
  else process.env.DISCORD_INSTALL_HASHED_STATE = value;
  jest.resetModules();
  expect(require('../src/config').DISCORD_INSTALL_HASHED_STATE).toBe(expected);
});

test.each(['tru', 'TRUE', '1', 'yes'])('disables only customer install for malformed setting %s', value => {
  const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
  process.env.DISCORD_INSTALL_HASHED_STATE = value;
  jest.resetModules();
  const config = require('../src/config');
  expect(config.isDiscordInstallConfigured).toBe(false);
  expect(config.discordInstallNotConfiguredReason).toBe('DISCORD_INSTALL_HASHED_STATE must be true or false');
  expect(warn).toHaveBeenCalledWith('[config] DISCORD_INSTALL_HASHED_STATE must be true or false; customer install disabled, other bot operations remain available.');
});
