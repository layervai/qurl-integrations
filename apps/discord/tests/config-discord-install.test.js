const { withFreshConfig } = require('./helpers/fresh-config');

describe('Discord install readiness derivation', () => {
  it('stays disabled when qURL OAuth is configured without the Discord client secret', () => {
    withFreshConfig(
      {
        AUTH0_DOMAIN: 'layerv.us.auth0.com',
        AUTH0_CLIENT_ID: 'client-id',
        AUTH0_CLIENT_SECRET: 'client-secret',
        AUTH0_AUDIENCE: 'https://api.example.test',
        DISCORD_CLIENT_SECRET: undefined,
      },
      (config) => {
        expect(config.isQurlOAuthConfigured).toBe(true);
        expect(config.isDiscordInstallConfigured).toBe(false);
      },
    );
  });

  it('enables the install flow when both qURL OAuth and Discord are configured', () => {
    withFreshConfig(
      {
        AUTH0_DOMAIN: 'layerv.us.auth0.com',
        AUTH0_CLIENT_ID: 'client-id',
        AUTH0_CLIENT_SECRET: 'client-secret',
        AUTH0_AUDIENCE: 'https://api.example.test',
        DISCORD_CLIENT_ID: '123456789012345678',
        DISCORD_CLIENT_SECRET: 'discord-secret-present',
        BASE_URL: 'https://bot.example.test',
        AUTH0_EMAIL_CONNECTION: undefined,
      },
      (config) => {
        expect(config.isQurlOAuthConfigured).toBe(true);
        expect(config.isDiscordInstallConfigured).toBe(true);
      },
    );
  });

  it('cannot enable the install flow when qURL OAuth is not configured', () => {
    withFreshConfig(
      {
        AUTH0_DOMAIN: undefined,
        AUTH0_CLIENT_ID: undefined,
        AUTH0_CLIENT_SECRET: undefined,
        AUTH0_AUDIENCE: undefined,
        DISCORD_CLIENT_ID: '123456789012345678',
        DISCORD_CLIENT_SECRET: 'discord-secret-present',
        BASE_URL: 'https://bot.example.test',
        AUTH0_EMAIL_CONNECTION: undefined,
      },
      (config) => {
        expect(config.isQurlOAuthConfigured).toBe(false);
        expect(config.isDiscordInstallConfigured).toBe(false);
      },
    );
  });
});
