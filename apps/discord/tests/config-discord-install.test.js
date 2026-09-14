const { withFreshConfig } = require('./helpers/fresh-config');

describe('Discord install readiness derivation', () => {
  it('stays disabled when qURL OAuth is configured without the Discord client secret', () => {
    withFreshConfig(
      {
        AUTH0_DOMAIN: 'layerv.us.auth0.com',
        AUTH0_CLIENT_ID: 'client-id',
        AUTH0_CLIENT_SECRET: 'client-secret',
        AUTH0_AUDIENCE: 'https://api.example.test',
        BASE_URL: 'https://discord.example.test',
        DISCORD_CLIENT_ID: '123456789012345678',
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
        BASE_URL: 'https://discord.example.test',
        DISCORD_CLIENT_ID: '123456789012345678',
        DISCORD_CLIENT_SECRET: 'discord-secret-present',
      },
      (config) => {
        expect(config.isQurlOAuthConfigured).toBe(true);
        expect(config.isDiscordInstallConfigured).toBe(true);
      },
    );
  });

  it.each([
    ['http://x.localhost', null],
    ['http://localhost.', null],
    ['http://localhost.evil.com', 'BASE_URL cannot retain the Secure install cookie'],
    ['not a url', 'BASE_URL cannot retain the Secure install cookie'],
  ])('BASE_URL %s -> install readiness reason %s', (baseUrl, reason) => {
    withFreshConfig(
      {
        AUTH0_DOMAIN: 'layerv.us.auth0.com',
        AUTH0_CLIENT_ID: 'client-id',
        AUTH0_CLIENT_SECRET: 'client-secret',
        AUTH0_AUDIENCE: 'https://api.example.test',
        BASE_URL: baseUrl,
        DISCORD_CLIENT_ID: '123456789012345678',
        DISCORD_CLIENT_SECRET: 'discord-secret-present',
      },
      (config) => {
        expect(config.discordInstallNotConfiguredReason).toBe(reason);
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
        BASE_URL: 'https://discord.example.test',
        DISCORD_CLIENT_ID: '123456789012345678',
        DISCORD_CLIENT_SECRET: 'discord-secret-present',
      },
      (config) => {
        expect(config.isQurlOAuthConfigured).toBe(false);
        expect(config.isDiscordInstallConfigured).toBe(false);
      },
    );
  });
});
