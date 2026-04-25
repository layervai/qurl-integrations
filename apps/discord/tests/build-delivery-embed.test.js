/**
 * Tests for `buildDeliveryEmbed` — specifically the senderAlias
 * sanitization layer that strips bidi / zero-width / control / soft-
 * hyphen / line-separator / BOM characters before rendering the alias
 * inside `**...**` in the description. This is a security control: a
 * display name with a leading U+202E (RLO) would otherwise flip the
 * direction of the description and let an attacker visually spoof a
 * different sender identity. Regression here would silently lose the
 * spoof defense.
 */

const capturedEmbeds = [];

jest.mock('discord.js', () => {
  const makeEmbed = () => {
    const embed = {
      _description: null,
      setColor: jest.fn().mockReturnThis(),
      setAuthor: jest.fn().mockReturnThis(),
      setDescription: jest.fn(function (d) { embed._description = d; return embed; }),
      addFields: jest.fn().mockReturnThis(),
      setFooter: jest.fn().mockReturnThis(),
      setTimestamp: jest.fn().mockReturnThis(),
    };
    capturedEmbeds.push(embed);
    return embed;
  };
  return {
    EmbedBuilder: jest.fn().mockImplementation(makeEmbed),
    SlashCommandBuilder: jest.fn().mockImplementation(() => ({
      setName: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      addSubcommand: jest.fn().mockReturnThis(),
      addStringOption: jest.fn().mockReturnThis(),
      addUserOption: jest.fn().mockReturnThis(),
      addAttachmentOption: jest.fn().mockReturnThis(),
      addIntegerOption: jest.fn().mockReturnThis(),
      setDefaultMemberPermissions: jest.fn().mockReturnThis(),
      setDMPermission: jest.fn().mockReturnThis(),
      toJSON: jest.fn(() => ({})),
    })),
    ActionRowBuilder: jest.fn().mockImplementation(() => ({ addComponents: jest.fn().mockReturnThis() })),
    ButtonBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
      setStyle: jest.fn().mockReturnThis(),
      setEmoji: jest.fn().mockReturnThis(),
    })),
    ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4 },
    StringSelectMenuBuilder: jest.fn().mockImplementation(() => ({})),
    UserSelectMenuBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setMinValues: jest.fn().mockReturnThis(),
      setMaxValues: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
    })),
    ModalBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setTitle: jest.fn().mockReturnThis(),
      addComponents: jest.fn().mockReturnThis(),
    })),
    TextInputBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
      setStyle: jest.fn().mockReturnThis(),
      setRequired: jest.fn().mockReturnThis(),
      setMinLength: jest.fn().mockReturnThis(),
      setMaxLength: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
    })),
    TextInputStyle: { Short: 1, Paragraph: 2 },
    InteractionType: { ApplicationCommand: 2 },
    PermissionFlagsBits: { Administrator: 1n << 3n, ManageGuild: 1n << 5n },
    ChannelType: { GuildText: 0, GuildVoice: 2 },
    ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
    Client: jest.fn().mockImplementation(() => ({ on: jest.fn(), once: jest.fn(), login: jest.fn() })),
    GatewayIntentBits: { Guilds: 1, GuildMembers: 2, GuildVoiceStates: 4, GuildMessages: 8 },
    Partials: { Channel: 0, Message: 1 },
    Events: { ClientReady: 'ready', InteractionCreate: 'interactionCreate' },
  };
});

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  ADMIN_USER_IDS: [],
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  isMultiTenant: false,
  ENABLE_OPENNHP_FEATURES: false,
  isOpenNHPActive: false,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
}));

jest.mock('../src/database', () => ({
  getGuildApiKey: jest.fn(), setGuildApiKey: jest.fn(),
  recordQURLSendBatch: jest.fn(), recordQURLSend: jest.fn(),
  updateSendDMStatus: jest.fn(), getSendByPrefix: jest.fn(),
  cleanupExpiredSends: jest.fn(), getStats: jest.fn(),
}));

jest.mock('../src/qurl', () => ({
  mintLinks: jest.fn(), revokeAllLinks: jest.fn(),
  getResourceStatus: jest.fn(), deleteLink: jest.fn(),
}));

jest.mock('../src/connector', () => ({ uploadJsonToConnector: jest.fn() }));

const { _test } = require('../src/commands');
const { buildDeliveryEmbed } = _test;

const baseArgs = {
  resourceType: 'file',
  qurlLink: 'https://qurl.link/#at_test',
  expiresIn: '15 minutes',
  personalMessage: null,
};

beforeEach(() => { capturedEmbeds.length = 0; });

describe('buildDeliveryEmbed — senderAlias sanitization', () => {
  it('renders a normal alias unchanged in the description', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._description).toContain('**Vik** shared a file with you.');
  });

  it('renders "location" in the description when resourceType is maps', () => {
    buildDeliveryEmbed({ ...baseArgs, resourceType: 'maps', senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._description).toContain('**Vik** shared a location with you.');
  });

  it('strips U+202E (RLO) from the alias to prevent direction-flip spoof', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: '\u202EAdmin' });
    const desc = capturedEmbeds[0]._description;
    expect(desc.includes('\u202E')).toBe(false);
    expect(desc).toContain('**Admin** shared a file with you.');
  });

  it('strips zero-width spaces and bidi isolates from the alias', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: '\u200BVik\u2066\u2069' });
    const desc = capturedEmbeds[0]._description;
    expect(/[\u200B\u2066\u2069]/.test(desc)).toBe(false);
    expect(desc).toContain('**Vik** shared a file with you.');
  });

  it('strips line/paragraph separators and BOM (would otherwise break embed layout)', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: '\uFEFFVik\u2028\u2029' });
    const desc = capturedEmbeds[0]._description;
    expect(/[\uFEFF\u2028\u2029]/.test(desc)).toBe(false);
    expect(desc).toContain('**Vik** shared a file with you.');
  });

  it('falls back to "Someone" when alias is entirely strip-eligible chars', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: '\u200B\u202E\u2066\u00AD' });
    expect(capturedEmbeds[0]._description).toContain('**Someone** shared a file with you.');
  });

  it('falls back to "Someone" when alias is null/undefined/empty', () => {
    for (const alias of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryEmbed({ ...baseArgs, senderAlias: alias });
      expect(capturedEmbeds[0]._description).toContain('**Someone** shared a file with you.');
    }
  });

  it('escapes markdown chars in alias (e.g. masked-link injection)', () => {
    buildDeliveryEmbed({ ...baseArgs, senderAlias: '[click](https://evil.com)' });
    const desc = capturedEmbeds[0]._description;
    // Brackets and parens must be backslash-escaped so Discord renders them
    // literally instead of as a clickable masked link.
    expect(desc).toContain('\\[click\\]\\(https://evil.com\\)');
  });

  it('caps long aliases at 64 chars (defensive upper bound vs Discord 32-char display-name cap)', () => {
    const long = 'A'.repeat(200);
    buildDeliveryEmbed({ ...baseArgs, senderAlias: long });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('**' + 'A'.repeat(64) + '** shared a file with you.');
    expect(desc).not.toContain('**' + 'A'.repeat(65));
  });
});
