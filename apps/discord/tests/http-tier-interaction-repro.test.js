
const { Client, GatewayIntentBits } = require('discord.js');
const { initHttpOnly } = require('../src/http-only-init');

function makeLogger() {
  return { info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() };
}

function makeGuildSlashInteractionPayload() {
  return {
    id: '1234567890123456789',
    application_id: '1491278419530285056',
    type: 2, // InteractionType.ApplicationCommand
    data: {
      id: '1491278419530285057',
      name: 'qurl',
      type: 1, // ApplicationCommandType.ChatInput
      options: [
        {
          name: 'help',
          type: 1, // Subcommand
          options: [],
        },
      ],
      guild_id: '935687893474758687',
    },
    guild_id: '935687893474758687',
    channel_id: '935687893474758690',
    channel: {
      id: '935687893474758690',
      type: 0,
      name: 'general',
      flags: 0,
      guild_id: '935687893474758687',
      parent_id: null,
      permissions: '4398046511103',
      position: 0,
    },
    member: {
      user: {
        id: '987654321098765432',
        username: 'testuser',
        global_name: 'TestUser',
        discriminator: '0',
        avatar: null,
        public_flags: 0,
      },
      roles: [],
      premium_since: null,
      permissions: '4398046511103',
      pending: false,
      nick: null,
      mute: false,
      joined_at: '2024-01-01T00:00:00.000000+00:00',
      flags: 0,
      deaf: false,
      communication_disabled_until: null,
      avatar: null,
    },
    locale: 'en-US',
    token: 'aW50ZXJhY3Rpb24tdG9rZW4tdGVzdA',
    version: 1,
    guild_locale: 'en-US',
    app_permissions: '4398046511103',
    entitlement_sku_ids: [],
    entitlements: [],
    authorizing_integration_owners: {
      '0': '935687893474758687',
    },
    context: 0,
  };
}

describe('http-tier interaction reconstruction (regression pin)', () => {
  let client;

  beforeEach(async () => {
    client = new Client({ intents: [GatewayIntentBits.Guilds] });
    client.rest.setToken = jest.fn();
    client.rest.get = jest.fn().mockResolvedValue({
      id: '1491278419530285056',
      username: 'qurl-bot-test',
      bot: true,
      discriminator: '0',
      global_name: 'qurl-bot',
    });

    const config = { DISCORD_TOKEN: 'test-token', GUILD_ID: null };
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();

    await initHttpOnly({ client, config, refreshCache, logger });
  });

  afterEach(async () => {
    await client.destroy().catch(() => {});
  });

  it('client.user is populated after initHttpOnly (the seed worked)', () => {
    expect(client.user).not.toBeNull();
    expect(client.user.id).toBe('1491278419530285056');
  });

  it('client.actions.InteractionCreate.handle does NOT throw on a guild slash-command payload', () => {
    const data = makeGuildSlashInteractionPayload();
    expect(() => {
      client.actions.InteractionCreate.handle(data);
    }).not.toThrow();
  });

  it('emits interactionCreate on a guild slash-command payload', () => {
    const data = makeGuildSlashInteractionPayload();
    const handler = jest.fn();
    client.on('interactionCreate', handler);
    client.actions.InteractionCreate.handle(data);
    expect(handler).toHaveBeenCalledTimes(1);
    const [interaction] = handler.mock.calls[0];
    expect(interaction.id).toBe('1234567890123456789');
    expect(interaction.commandName).toBe('qurl');
  });

  it('interaction.guildId is set even when client.guilds.cache is empty', () => {
    const data = makeGuildSlashInteractionPayload();
    let captured = null;
    client.on('interactionCreate', (i) => { captured = i; });
    client.actions.InteractionCreate.handle(data);
    expect(captured.guildId).toBe('935687893474758687');
  });

  it('interaction.guild is null when cache is empty (documents the http-only state)', () => {
    const data = makeGuildSlashInteractionPayload();
    let captured = null;
    client.on('interactionCreate', (i) => { captured = i; });
    client.actions.InteractionCreate.handle(data);
    expect(captured.guildId).not.toBeNull();
    expect(captured.guild).toBeNull();
  });
});
