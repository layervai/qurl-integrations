/**
 * Tests for `buildDeliveryPayload` — specifically the senderAlias
 * sanitization layer that strips bidi / zero-width / control / soft-
 * hyphen / line-separator / BOM characters before rendering the alias
 * inside `**...**` in the description. This is a security control: a
 * display name with a leading U+202E (RLO) would otherwise flip the
 * direction of the description and let an attacker visually spoof a
 * different sender identity. Regression here would silently lose the
 * spoof defense.
 */

const capturedEmbeds = [];
// Capture every ButtonBuilder constructed so tests can assert what
// `setStyle` / `setLabel` / `setURL` were called with — locks down the
// Step Through button shape against silent regressions.
const capturedButtons = [];

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
    ButtonBuilder: jest.fn().mockImplementation(() => {
      const btn = {
        _style: null, _label: null, _url: null, _customId: null,
        setCustomId: jest.fn(function (id) { btn._customId = id; return btn; }),
        setLabel: jest.fn(function (l) { btn._label = l; return btn; }),
        setStyle: jest.fn(function (s) { btn._style = s; return btn; }),
        setEmoji: jest.fn().mockReturnThis(),
        setURL: jest.fn(function (u) { btn._url = u; return btn; }),
      };
      capturedButtons.push(btn);
      return btn;
    }),
    ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
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
const { buildDeliveryPayload, resolveSenderAlias } = _test;

const baseArgs = {
  qurlLink: 'https://qurl.link/#at_test',
  expiresIn: '15 minutes',
  personalMessage: null,
};

beforeEach(() => { capturedEmbeds.length = 0; capturedButtons.length = 0; });

describe('buildDeliveryPayload — senderAlias sanitization', () => {
  it('renders a normal alias unchanged in the description', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._description).toContain('**Vik** opened a door for you.');
  });

  it('strips U+202E (RLO) from the alias to prevent direction-flip spoof', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u202EAdmin' });
    const desc = capturedEmbeds[0]._description;
    expect(desc.includes('\u202E')).toBe(false);
    expect(desc).toContain('**Admin** opened a door for you.');
  });

  it('strips zero-width spaces and bidi isolates from the alias', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u200BVik\u2066\u2069' });
    const desc = capturedEmbeds[0]._description;
    expect(/[\u200B\u2066\u2069]/.test(desc)).toBe(false);
    expect(desc).toContain('**Vik** opened a door for you.');
  });

  it('strips U+061C (Arabic Letter Mark) — completes bidi-control parity with RLM/LRM', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u061CVik' });
    const desc = capturedEmbeds[0]._description;
    expect(desc).not.toMatch(/\u061C/);
    expect(desc).toContain('**Vik** opened a door for you.');
  });

  it('strips line/paragraph separators and BOM (would otherwise break embed layout)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\uFEFFVik\u2028\u2029' });
    const desc = capturedEmbeds[0]._description;
    expect(/[\uFEFF\u2028\u2029]/.test(desc)).toBe(false);
    expect(desc).toContain('**Vik** opened a door for you.');
  });

  it('falls back to "Someone" when alias is entirely strip-eligible chars', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u200B\u202E\u2066\u00AD' });
    expect(capturedEmbeds[0]._description).toContain('**Someone** opened a door for you.');
  });

  it('falls back to "Someone" when alias is null/undefined/empty', () => {
    for (const alias of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: alias });
      expect(capturedEmbeds[0]._description).toContain('**Someone** opened a door for you.');
    }
  });

  it('escapes markdown chars in alias (e.g. masked-link injection)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '[click](https://evil.com)' });
    const desc = capturedEmbeds[0]._description;
    // Brackets and parens must be backslash-escaped so Discord renders them
    // literally instead of as a clickable masked link.
    expect(desc).toContain('\\[click\\]\\(https://evil.com\\)');
  });

  it('caps long aliases at 64 chars (defensive upper bound vs Discord 32-char display-name cap)', () => {
    const long = 'A'.repeat(200);
    buildDeliveryPayload({ ...baseArgs, senderAlias: long });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('**' + 'A'.repeat(64) + '** opened a door for you.');
    expect(desc).not.toContain('**' + 'A'.repeat(65));
  });

  // Regression net for: expiresIn used to render the raw choice value
  // ('30m', '1h') instead of the human label ('30 minutes', '1 hour').
  // Locks the formatExpiryLabel call site inside buildDeliveryPayload.
  it('renders the human-readable expiry label, not the raw choice value', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresIn: '30m' });
    const fields = capturedEmbeds[0].addFields.mock.calls.flatMap(call => call);
    const portalField = fields.find(f => typeof f.value === 'string' && f.value.includes('Portal closes in'));
    expect(portalField).toBeDefined();
    expect(portalField.value).toContain('Portal closes in **30 minutes**');
    expect(portalField.value).not.toContain('**30m**');
  });

  it('handles each known expiry choice with its proper label', () => {
    const cases = [
      ['30m', '30 minutes'],
      ['1h', '1 hour'],
      ['6h', '6 hours'],
      ['24h', '24 hours'],
      ['7d', '7 days'],
    ];
    for (const [value, label] of cases) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresIn: value });
      const fields = capturedEmbeds[0].addFields.mock.calls.flatMap(c => c);
      const portalField = fields.find(f => typeof f.value === 'string' && f.value.includes('Portal closes in'));
      expect(portalField.value).toContain(`Portal closes in **${label}**`);
    }
  });

  // Locks the Step Through button shape: a future refactor that drops
  // `.setURL(qurlLink)` (or downgrades to a non-Link style) would leave
  // recipients with a button that doesn't navigate anywhere. This test
  // asserts the button is built as Link-style with the supplied qURL.
  it('builds the Step Through button as a Link-style button with the qURL as its URL', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', qurlLink: 'https://qurl.link/#at_unique_token' });
    // Last button constructed in the buildDeliveryPayload call is the Step Through.
    const stepThrough = capturedButtons[capturedButtons.length - 1];
    expect(stepThrough).toBeDefined();
    expect(stepThrough._label).toBe('Step Through →');
    expect(stepThrough._style).toBe(5); // ButtonStyle.Link
    expect(stepThrough._url).toBe('https://qurl.link/#at_unique_token');
    expect(stepThrough.setURL).toHaveBeenCalledWith('https://qurl.link/#at_unique_token');
  });

  it('flattens newlines in personal message so the styled blockquote stays single-line', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: 'line one\nline two\r\nline three' });
    const fields = capturedEmbeds[0].addFields.mock.calls.flatMap(c => c);
    const msgField = fields.find(f => typeof f.value === 'string' && f.value.includes('line one'));
    expect(msgField).toBeDefined();
    expect(msgField.value).toBe('> *"line one line two line three"*');
    expect(msgField.value).not.toMatch(/\n/);
  });
});

describe('resolveSenderAlias — fallback chain', () => {
  it('uses member.displayName first (guild nickname / globalName)', () => {
    const interaction = {
      member: { displayName: 'Vik (Eng)' },
      user: { displayName: 'vikramlayerv', username: 'vikram' },
    };
    expect(resolveSenderAlias(interaction)).toBe('Vik (Eng)');
  });

  it('falls through to user.displayName when member is null (user-app DM context)', () => {
    const interaction = {
      member: null,
      user: { displayName: 'vikramlayerv', username: 'vikram' },
    };
    expect(resolveSenderAlias(interaction)).toBe('vikramlayerv');
  });

  it('falls through to user.username when displayName is missing (older mocks / shapes)', () => {
    const interaction = {
      member: null,
      user: { username: 'vikram' },
    };
    expect(resolveSenderAlias(interaction)).toBe('vikram');
  });

  it('returns "Someone" for malformed interactions instead of throwing', () => {
    expect(resolveSenderAlias({})).toBe('Someone');
    expect(resolveSenderAlias({ member: null, user: null })).toBe('Someone');
    expect(resolveSenderAlias(null)).toBe('Someone');
    expect(resolveSenderAlias(undefined)).toBe('Someone');
  });
});
