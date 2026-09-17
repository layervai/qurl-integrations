
const capturedEmbeds = [];
const capturedButtons = [];

jest.mock('discord.js', () => {
  const makeEmbed = () => {
    const embed = {
      _description: null,
      _author: null,
      _footer: null,
      setColor: jest.fn().mockReturnThis(),
      setAuthor: jest.fn(function (a) { embed._author = a; return embed; }),
      setDescription: jest.fn(function (d) { embed._description = d; return embed; }),
      addFields: jest.fn().mockReturnThis(),
      setFooter: jest.fn(function (f) { embed._footer = f; return embed; }),
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
        _style: null, _label: null, _url: null, _customId: null, _emoji: null,
        setCustomId: jest.fn(function (id) { btn._customId = id; return btn; }),
        setLabel: jest.fn(function (l) { btn._label = l; return btn; }),
        setStyle: jest.fn(function (s) { btn._style = s; return btn; }),
        setEmoji: jest.fn(function (e) { btn._emoji = e; return btn; }),
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
      setDefaultValues: jest.fn().mockReturnThis(),
      addDefaultUsers: jest.fn().mockReturnThis(),
    })),
    MentionableSelectMenuBuilder: jest.fn().mockImplementation(() => ({
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
    GatewayIntentBits: { Guilds: 1, GuildMembers: 2, GuildVoiceStates: 128 },
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
  QURL_DETECT_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  isMultiTenant: false,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

jest.mock('../src/store', () => ({
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
const {
  buildDeliveryPayload,
  buildDeliveryEmbed,
  buildStepThroughButton,
  buildTrustButton,
  packBulkDeliveryComponents,
  buildRevokedDMPayload,
  resolveSenderAlias,
} = _test;

const baseArgs = {
  qurlLink: 'https://qurl.link/#at_test',
  expiresAt: 1735689600,  // arbitrary fixed timestamp; tests assert it survives into the embed
  personalMessage: null,
  guildName: 'Acme Discord',
  guildIconUrl: 'https://cdn.discordapp.com/icons/g/icon.png',
};

const TEST_NOW_SECONDS = 1704067200;

beforeEach(() => {
  capturedEmbeds.length = 0;
  capturedButtons.length = 0;
  jest.spyOn(Date, 'now').mockReturnValue(TEST_NOW_SECONDS * 1000);
});

afterEach(() => { jest.restoreAllMocks(); });

describe('buildDeliveryPayload — senderAlias sanitization (author row)', () => {
  it('renders a normal alias unchanged in the author row name', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._author.name).toContain('Vik');
    expect(capturedEmbeds[0]._description).toContain('opened a door for you.');
    expect(capturedEmbeds[0]._description).not.toContain('**Vik**');
  });

  it('strips U+202E (RLO) from the alias to prevent direction-flip spoof', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u202EAdmin' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName.includes('\u202E')).toBe(false);
    expect(authorName).toContain('Admin');
  });

  it('strips zero-width spaces and bidi isolates from the alias', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u200BVik\u2066\u2069' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(/[\u200B\u2066\u2069]/.test(authorName)).toBe(false);
    expect(authorName).toContain('Vik');
  });

  it('strips U+061C (Arabic Letter Mark) — completes bidi-control parity with RLM/LRM', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u061CVik' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).not.toMatch(/\u061C/);
    expect(authorName).toContain('Vik');
  });

  it('strips line/paragraph separators and BOM (would otherwise break embed layout)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\uFEFFVik\u2028\u2029' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(/[\uFEFF\u2028\u2029]/.test(authorName)).toBe(false);
    expect(authorName).toContain('Vik');
  });

  it('falls back to "Someone" when alias is entirely strip-eligible chars', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '\u200B\u202E\u2066\u00AD' });
    expect(capturedEmbeds[0]._author.name).toContain('Someone');
  });

  it('falls back to "Someone" when alias is null/undefined/empty', () => {
    for (const alias of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: alias });
      expect(capturedEmbeds[0]._author.name).toContain('Someone');
    }
  });

  it('renders markdown-injection alias as literal text (no escape, no clickable link)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '[click](https://evil.com)' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toContain('[click](https://evil.com)');
    expect(authorName).not.toContain('\\[');
    expect(capturedEmbeds[0]._description).not.toContain('[click]');
  });

  it('caps long aliases at 64 chars (defensive upper bound vs Discord 32-char display-name cap)', () => {
    const long = 'A'.repeat(200);
    buildDeliveryPayload({ ...baseArgs, senderAlias: long });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toContain('A'.repeat(64));
    expect(authorName).not.toContain('A'.repeat(65));
  });

  it('does not split surrogate pairs at the 64-char boundary', () => {
    const alias = 'A'.repeat(63) + '🎉';
    buildDeliveryPayload({ ...baseArgs, senderAlias: alias });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toContain('A'.repeat(63) + '🎉');
    expect(authorName).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
  });

  it('renders Discord native relative-time <t:N:R> in the description (Closes line)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600 });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toMatch(/🕐 Closes <t:1735689600:R>/);
    expect(desc).not.toMatch(/Closes in \*\*\d/);
  });

  it('renders Closed when the qURL already expired at delivery render time', () => {
    const expiredAt = TEST_NOW_SECONDS - 60;
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: expiredAt });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toMatch(new RegExp(`🕐 Closed <t:${expiredAt}:R>`));
    expect(desc).not.toMatch(/🕐 Closes <t:/);
  });

  it('accepts Number.MAX_SAFE_INTEGER as a positive integer (no synthetic upper bound)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: Number.MAX_SAFE_INTEGER });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain(`🕐 Closes <t:${Number.MAX_SAFE_INTEGER}:R>`);
  });

  it.each([
    [undefined],
    [null],
    [NaN],
    [Infinity],
    [-Infinity],
    ['soon'],
    [{}],
    [1735689600.5],                  // float
    [0.1],                           // float
    [0],                             // non-positive
    [-1],                            // non-positive (would render as "55 years ago")
    [-1735689600],                   // non-positive (negative timestamp)
  ])('throws fail-loud for invalid expiresAt: %p', (bad) => {
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: bad }))
      .toThrow(/expiresAt must be a positive integer Unix-seconds number/);
  });

  it('error message exposes both String(value) and typeof for diagnosis', () => {
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: {} }))
      .toThrow(/got \[object Object\], typeof=object/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 'soon' }))
      .toThrow(/got soon, typeof=string/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600.5 }))
      .toThrow(/got 1735689600\.5, typeof=number/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: -1 }))
      .toThrow(/got -1, typeof=number/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: null }))
      .toThrow(/got null, typeof=object/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: undefined }))
      .toThrow(/got undefined, typeof=undefined/);
  });

  it('builds the Step Through button as a Link-style button with the qURL as its URL', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', qurlLink: 'https://qurl.link/#at_unique_token' });
    const stepThrough = capturedButtons.find(b => b._label === 'Step Through');
    expect(stepThrough).toBeDefined();
    expect(stepThrough._emoji).toBe('🚪');
    expect(stepThrough._style).toBe(5); // ButtonStyle.Link
    expect(stepThrough._url).toBe('https://qurl.link/#at_unique_token');
    expect(stepThrough.setURL).toHaveBeenCalledWith('https://qurl.link/#at_unique_token');
  });

  it('renders personalMessage as-is (no internal markdown escape — by contract)', () => {
    const raw = '[click](https://evil.com) **bold**';
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: raw });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain(`> *"${raw}"*`);
    expect(desc).not.toContain('\\[click');
    expect(desc).not.toContain('\\*\\*bold');
  });

  it('does not split surrogate pairs at the 280-codepoint personalMessage boundary (#345)', () => {
    const message = 'A'.repeat(279) + '🎉' + 'X';  // 281 codepoints; codepoint 280 = 🎉
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: message });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('🎉');
    expect(desc).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
    expect(desc).not.toMatch(/🎉X/);
  });

  it('renders personalMessage with `> ` prefixes verbatim (no auto-fix of nested blockquote)', () => {
    const raw = '> faux quote\n> still faux';
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: raw });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('> *"> faux quote > still faux"*');
  });

  it('flattens newlines in personal message so the styled blockquote stays single-line', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: 'line one\nline two\r\nline three' });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('> *"line one line two line three"*');
    const messageLine = desc.split('\n').find(l => l.includes('line one'));
    expect(messageLine).toBeDefined();
    expect(messageLine).not.toMatch(/[\n\r]/);
  });

  it('renders action → personal message → expiry in that order when all three are present', () => {
    buildDeliveryPayload({
      ...baseArgs,
      senderAlias: 'Vik',
      personalMessage: 'Quarterly numbers — for your eyes only.',
      expiresAt: 1735689600,
    });
    const desc = capturedEmbeds[0]._description;
    const lines = desc.split('\n');
    expect(lines).toHaveLength(3);
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toContain('Quarterly numbers');
    expect(lines[2]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  it('renders action → expiry with no gap when personalMessage is omitted', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600 });
    const desc = capturedEmbeds[0]._description;
    const lines = desc.split('\n');
    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  it('omits the blockquote line when personalMessage collapses to empty after trim', () => {
    buildDeliveryPayload({
      ...baseArgs,
      senderAlias: 'Vik',
      personalMessage: '  \n \n  ',
      expiresAt: 1735689600,
    });
    const desc = capturedEmbeds[0]._description;
    expect(desc).not.toContain('> *""*');
    const lines = desc.split('\n');
    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
  });
});

describe('buildDeliveryPayload — author row provenance', () => {
  it('composes author name as "sender · guildName" when both are present', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: 'Acme Discord' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik · Acme Discord');
  });

  it('falls back to sender-only when guildName is missing/empty', () => {
    for (const guildName of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName });
      expect(capturedEmbeds[0]._author.name).toBe('Vik');
    }
  });

  it('falls back to sender-only when guildName is entirely strip-eligible chars (no "Someone" leak)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: '‮​­' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik');
    expect(capturedEmbeds[0]._author.name).not.toContain('Someone');
  });

  it('sanitizes sender and guildName independently (hostile half does not influence the other)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '‮Vik', guildName: 'Acme Discord' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik · Acme Discord');
    capturedEmbeds.length = 0;
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: '‮Acme' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik · Acme');
  });

  it('both-halves-hostile: composes to bare "Someone" (no Someone · Someone, no trailing separator)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: '‮​', guildName: '‮​' });
    expect(capturedEmbeds[0]._author.name).toBe('Someone');
  });

  it('caps combined author.name at 256 UTF-16 units (worst-case surrogate-pair emoji on both halves)', () => {
    const allEmoji = '🎉'.repeat(64);  // 64 codepoints, 128 UTF-16 units
    buildDeliveryPayload({ ...baseArgs, senderAlias: allEmoji, guildName: allEmoji });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName.length).toBeLessThanOrEqual(256);
    expect(authorName).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
  });

  it('caps long guildName at 64 codepoints', () => {
    const long = 'G'.repeat(200);
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: long });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toBe('Vik · ' + 'G'.repeat(64));
  });

  it('applies plain (non-markdown-escaping) sanitization to guildName', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: '‮Acme​' });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).not.toMatch(/[\u202E\u200B]/);
    expect(authorName).toContain('Acme');
    expect(authorName).toBe('Vik · Acme');
  });

  it('attaches guild iconURL when provided', () => {
    const iconUrl = 'https://cdn.discordapp.com/icons/g/icon.png';
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildIconUrl: iconUrl });
    expect(capturedEmbeds[0]._author.iconURL).toBe(iconUrl);
  });

  it('omits iconURL key entirely when guild has no icon', () => {
    for (const noIcon of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildIconUrl: noIcon });
      expect(capturedEmbeds[0]._author).not.toHaveProperty('iconURL');
    }
  });
});

describe('buildDeliveryPayload — footer + trust button', () => {
  it('sets a footer naming the destination domain', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._footer).toEqual({ text: 'opens qurl.link' });
  });

  it('builds a Link-style "What is qURL?" trust button pointing at the brand landing', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    const trust = capturedButtons.find(b => b._label === 'What is qURL?');
    expect(trust).toBeDefined();
    expect(trust._emoji).toBe('🛡️');
    expect(trust._style).toBe(5); // ButtonStyle.Link
    expect(trust._url).toBe('https://layerv.ai/qurl/');
  });

  it('ships Step Through and What is qURL? in the same ActionRow', () => {
    const { components } = buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(components).toHaveLength(1);
    expect(components[0].addComponents).toHaveBeenCalledTimes(1);
    const args = components[0].addComponents.mock.calls[0];
    expect(args).toHaveLength(2);
    expect(args[0]._label).toBe('Step Through');
    expect(args[1]._label).toBe('What is qURL?');
  });

  it('delivers an over-512-character qv2 link as equal-weight Markdown actions', () => {
    const qv2Link = `https://qurl.link/#qv2t1.${'A'.repeat(600)}`;
    expect(qv2Link.length).toBeGreaterThan(512);

    const { components } = buildDeliveryPayload({
      ...baseArgs,
      senderAlias: 'Vik',
      qurlLink: qv2Link,
    });

    expect(components).toEqual([]);
    expect(capturedButtons).toHaveLength(0);
    expect(capturedEmbeds[0]._description).toContain(`[🚪 Step Through](${qv2Link})`);
    expect(capturedEmbeds[0]._description)
      .toContain('[🛡️ What is qURL?](https://layerv.ai/qurl/)');
  });
});

describe('packBulkDeliveryComponents — 5-per-row chunking', () => {
  const rowButtons = (row) => row.addComponents.mock.calls[0][0];

  it('N=4: packs [s1, s2, s3, s4, trust] into a single ActionRow', () => {
    const rows = packBulkDeliveryComponents(['u1', 'u2', 'u3', 'u4'].map(t => `https://q.test/${t}`));
    expect(rows).toHaveLength(1);
    expect(rowButtons(rows[0]).map(b => b._label))
      .toEqual(['Step Through', 'Step Through', 'Step Through', 'Step Through', 'What is qURL?']);
  });

  it('N=5: splits into [s1..s5] + [trust] across two rows', () => {
    const rows = packBulkDeliveryComponents(['u1', 'u2', 'u3', 'u4', 'u5'].map(t => `https://q.test/${t}`));
    expect(rows).toHaveLength(2);
    const row1 = rowButtons(rows[0]);
    expect(row1).toHaveLength(5);
    expect(row1.every(b => b._label === 'Step Through')).toBe(true);
    const row2 = rowButtons(rows[1]);
    expect(row2).toHaveLength(1);
    expect(row2[0]._label).toBe('What is qURL?');
  });

  it('N=9: trust button shares the second row with 4 step-throughs', () => {
    const links = Array.from({ length: 9 }, (_, i) => `https://q.test/u${i}`);
    const rows = packBulkDeliveryComponents(links);
    expect(rows).toHaveLength(2);
    expect(rowButtons(rows[0])).toHaveLength(5);
    const row2 = rowButtons(rows[1]);
    expect(row2).toHaveLength(5);
    expect(row2.slice(0, 4).every(b => b._label === 'Step Through')).toBe(true);
    expect(row2[4]._label).toBe('What is qURL?');
  });

  it('throws on empty links — Discord rejects components-only payloads', () => {
    expect(() => packBulkDeliveryComponents([])).toThrow(/non-empty array/);
    expect(() => packBulkDeliveryComponents(null)).toThrow(/non-empty array/);
    expect(() => packBulkDeliveryComponents(undefined)).toThrow(/non-empty array/);
  });

  it('throws when qurlLinks length exceeds the 10-link cap', () => {
    const eleven = Array.from({ length: 11 }, (_, i) => `https://q.test/u${i}`);
    expect(() => packBulkDeliveryComponents(eleven)).toThrow(/exceeds the 10-link cap/);
  });

  it('N=10 (Discord embed cap): three rows of [s1..s5], [s6..s10], [trust]', () => {
    const links = Array.from({ length: 10 }, (_, i) => `https://q.test/u${i}`);
    const rows = packBulkDeliveryComponents(links);
    expect(rows).toHaveLength(3);
    expect(rowButtons(rows[0])).toHaveLength(5);
    expect(rowButtons(rows[1])).toHaveLength(5);
    const lastRow = rowButtons(rows[2]);
    expect(lastRow).toHaveLength(1);
    expect(lastRow[0]._label).toBe('What is qURL?');
    const stepUrls = [
      ...rowButtons(rows[0]).map(b => b._url),
      ...rowButtons(rows[1]).map(b => b._url),
    ];
    expect(stepUrls).toEqual(links);
  });

  it('buildStepThroughButton ships as a Link-style button with the supplied qURL as its URL', () => {
    buildStepThroughButton('https://qurl.link/#at_step');
    const stepThrough = capturedButtons[capturedButtons.length - 1];
    expect(stepThrough._label).toBe('Step Through');
    expect(stepThrough._emoji).toBe('🚪');
    expect(stepThrough._style).toBe(5); // ButtonStyle.Link
    expect(stepThrough._url).toBe('https://qurl.link/#at_step');
  });
});

describe('buildDeliveryEmbed — embed-only primitive used by bulk path', () => {
  it('returns the embed alone (no button row construction)', () => {
    const embed = buildDeliveryEmbed({
      senderAlias: 'Vik',
      guildName: 'Acme Discord',
      guildIconUrl: undefined,
      expiresAt: 1735689600,
      personalMessage: null,
    });
    expect(embed).toBe(capturedEmbeds[0]);
    expect(capturedButtons).toHaveLength(0);
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

describe('buildRevokedDMPayload — post-revoke recipient-side render', () => {
  it('renders the "closed the door" embed with the sender alias bolded', () => {
    capturedEmbeds.length = 0;
    buildRevokedDMPayload({ senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._description).toContain('**Vik** closed the door.');
    expect(capturedEmbeds[0]._description).toContain('This qURL is no longer active.');
  });

  it('passes components: [] explicitly so the Step Through button is cleared on edit', () => {
    const payload = buildRevokedDMPayload({ senderAlias: 'Vik' });
    expect(payload.components).toEqual([]);
  });

  it('strips bidi / zero-width spoof chars from the alias before rendering', () => {
    capturedEmbeds.length = 0;
    buildRevokedDMPayload({ senderAlias: '‮Admin' });
    expect(capturedEmbeds[0]._description).not.toContain('‮');
  });
});
