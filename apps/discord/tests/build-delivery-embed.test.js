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
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
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
  // Unix seconds (matches what production computes via expiryToMs at the
  // call site). buildDeliveryPayload renders this as <t:N:R> so Discord
  // shows the recipient a live "in 24 hours" / "in 16 hours" / etc.
  expiresAt: 1735689600,  // arbitrary fixed timestamp; tests assert it survives into the embed
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

  // The 64-char cap is codepoint-aware (Array.from + slice + join) — a
  // surrogate pair (e.g. 🎉 / U+1F389) sitting on the boundary must not
  // be split into a lone high surrogate, which Discord renders as tofu.
  // Regression net: 63 ASCII chars + 1 emoji = 64 codepoints, all kept.
  it('does not split surrogate pairs at the 64-char boundary', () => {
    const alias = 'A'.repeat(63) + '🎉';
    buildDeliveryPayload({ ...baseArgs, senderAlias: alias });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('**' + 'A'.repeat(63) + '🎉** opened a door for you.');
    // No lone high surrogate (\uD83C is the high half of 🎉)
    expect(desc).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
  });

  // Regression net for the live-countdown design: the Door-closes line
  // must render Discord's <t:N:R> relative-time markdown so the recipient
  // sees "in 24 hours" → "in 16 hours" → "1 hour ago" as time passes.
  // A future refactor that goes back to baking a static label into the
  // embed ("Door closes in **24 hours**" forever) would silently
  // regress this UX — the assertion below catches it.
  it('renders Discord native relative-time <t:N:R> in the description (Closes line)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600 });
    // Closes line is folded into the embed description alongside the
    // sender line (tightened layout \u2014 was a separate addFields() row,
    // which Discord padded with extra vertical whitespace and pushed
    // the button further away).
    const desc = capturedEmbeds[0]._description;
    expect(desc).toMatch(/\ud83d\udd50 Closes <t:1735689600:R>/);
    // Locks against accidental reversion to a static label
    expect(desc).not.toMatch(/Closes in \*\*\d/);
  });

  // Defensive guard: a future caller that drops `expiresAt` (or passes
  // null/undefined/NaN/a float) would otherwise render a malformed
  // "<t:undefined:R>" / "<t:NaN:R>" / "<t:1735689600.5:R>" to recipients.
  // Discord's <t:N:R> markdown accepts only integer Unix seconds, which
  // is what the lone call site produces via `Math.floor`; the
  // `Number.isInteger` guard tightens the contract from any-finite-number
  // to exactly-what-the-markdown-accepts. Matches the contract guard in
  // handleAddRecipients.
  // Positive test for the upper-bound reasoning documented in the
  // throw test below: there is no clean "finite integer that
  // Number.isInteger rejects" boundary because doubles can exactly
  // represent every integer up to 2^53. Pin that `Number.MAX_SAFE_INTEGER`
  // itself is accepted (and renders into the description as the literal
  // integer), so a future reader who tries `MAX_SAFE_INTEGER + 1` and
  // sees it still pass doesn't have to re-derive the reasoning. Discord's
  // <t:N:R> parser will overflow well before 2^53 (its accepted range is
  // ±10000 years from epoch), but that's Discord's responsibility — the
  // validator's contract is "positive integer", not "renderable timestamp".
  it('accepts Number.MAX_SAFE_INTEGER as a positive integer (no synthetic upper bound)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: Number.MAX_SAFE_INTEGER });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain(`🕐 Closes <t:${Number.MAX_SAFE_INTEGER}:R>`);
  });

  it('throws if expiresAt is missing, non-finite, a float, or non-positive (fail-loud)', () => {
    // Note on the "beyond MAX_SAFE_INTEGER" boundary: doubles can
    // exactly represent every integer up to 2^53, so 2^53 itself is
    // still `Number.isInteger == true`. Above 2^53, additions of 1
    // round to the nearest representable double (which is also an
    // integer at that magnitude), so `Number.isInteger` keeps
    // returning true. There is no clean "finite integer that
    // Number.isInteger rejects" boundary — the rejection set is
    // exactly: non-finite + non-integer-floats + non-positive.
    for (const bad of [
      undefined, null, NaN, Infinity, -Infinity, 'soon', {},
      1735689600.5, 0.1,            // floats
      0, -1, -1735689600,           // non-positive (negative timestamp would render as "55 years ago" in Discord)
    ]) {
      expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: bad }))
        .toThrow(/expiresAt must be a positive integer Unix-seconds number/);
    }
  });

  // Locks the operator-facing diagnostic shape: the throw message must
  // include both the stringified value AND its typeof so an oncall
  // doesn't have to guess whether the bad input was an object, a
  // function, or a stringified number. `${{}}` would otherwise coerce
  // to `[object Object]` via valueOf (acceptable), but the typeof tag
  // is what makes the distinction loud.
  it('error message exposes both String(value) and typeof for diagnosis', () => {
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: {} }))
      .toThrow(/got \[object Object\], typeof=object/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 'soon' }))
      .toThrow(/got soon, typeof=string/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600.5 }))
      .toThrow(/got 1735689600\.5, typeof=number/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: -1 }))
      .toThrow(/got -1, typeof=number/);
    // null vs undefined surfaces matter for triage: `typeof null` is
    // `'object'` (a longstanding JS oddity), so `got null, typeof=object`
    // is what the operator should see — different shape from
    // `got undefined, typeof=undefined`. Pin both.
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: null }))
      .toThrow(/got null, typeof=object/);
    expect(() => buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: undefined }))
      .toThrow(/got undefined, typeof=undefined/);
  });


  // Locks the Step Through button shape: a future refactor that drops
  // `.setURL(qurlLink)` (or downgrades to a non-Link style) would leave
  // recipients with a button that doesn't navigate anywhere. This test
  // asserts the button is built as Link-style with the supplied qURL,
  // and that the 🚪 emoji survives — Link buttons render gray, and the
  // door emoji ties the "opened a door for you" copy to the action so
  // the button reads as intentional rather than generic-CTA-grey.
  it('builds the Step Through button as a Link-style button with the qURL as its URL', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', qurlLink: 'https://qurl.link/#at_unique_token' });
    // Last button constructed in the buildDeliveryPayload call is the Step Through.
    const stepThrough = capturedButtons[capturedButtons.length - 1];
    expect(stepThrough).toBeDefined();
    expect(stepThrough._label).toBe('Step Through');
    expect(stepThrough._emoji).toBe('🚪');
    expect(stepThrough._style).toBe(5); // ButtonStyle.Link
    expect(stepThrough._url).toBe('https://qurl.link/#at_unique_token');
    expect(stepThrough.setURL).toHaveBeenCalledWith('https://qurl.link/#at_unique_token');
  });

  it('flattens newlines in personal message so the styled blockquote stays single-line', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: 'line one\nline two\r\nline three' });
    const desc = capturedEmbeds[0]._description;
    // Personal-message line sits inside the description block (folded
    // alongside sender + expiry so all three render in design order —
    // sender → message → expiry).
    expect(desc).toContain('> *"line one line two line three"*');
    // Both \n and \r inside the message itself are flattened to spaces;
    // any newline remaining on the message line would mean the
    // [\r\n]+ → ' ' collapse regressed.
    const messageLine = desc.split('\n').find(l => l.includes('line one'));
    expect(messageLine).toBeDefined();
    expect(messageLine).not.toMatch(/[\n\r]/);
  });

  // Ordering depends on which Embed slot each piece lands in: Discord
  // renders the description block above any fields, so a personal
  // message split into addFields would land AFTER the expiry line, not
  // between sender and expiry. Folding all three into one
  // setDescription is what guarantees the design ordering. Pin the
  // relative order so a future refactor that splits any of the three
  // back into addFields would be caught.
  it('renders sender → personal message → expiry in that order when all three are present', () => {
    buildDeliveryPayload({
      ...baseArgs,
      senderAlias: 'Vik',
      personalMessage: 'Quarterly numbers — for your eyes only.',
      expiresAt: 1735689600,
    });
    const desc = capturedEmbeds[0]._description;
    const senderIdx = desc.indexOf('**Vik**');
    const messageIdx = desc.indexOf('Quarterly numbers');
    const expiryIdx = desc.indexOf('Closes <t:');
    expect(senderIdx).toBeGreaterThanOrEqual(0);
    expect(messageIdx).toBeGreaterThan(senderIdx);
    expect(expiryIdx).toBeGreaterThan(messageIdx);
    // And no addFields call — folded entirely into description.
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  // When personalMessage is absent, the description still renders
  // sender → expiry in order (no orphaned blank line between them).
  it('renders sender → expiry with no gap when personalMessage is omitted', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600 });
    const desc = capturedEmbeds[0]._description;
    const lines = desc.split('\n');
    // EXACTLY 2 lines — guards against an orphan blank line creeping
    // in between sender and expiry when personalMessage is absent.
    // Do NOT loosen to `toBeGreaterThanOrEqual(2)`: a 3-line desc
    // would mean someone re-introduced `descLines.push('')` for
    // padding, which renders as a visible empty row in Discord.
    expect(lines).toHaveLength(2);
    expect(lines[0]).toContain('**Vik** opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
    // Mirror the all-three-pieces ordering test: also assert addFields
    // is never called on the no-personalMessage path. Belt-and-braces
    // against a future regression that adds a field only when one of
    // the slots is empty (e.g. a "no message attached" placeholder).
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  // Belt-and-braces: a personalMessage that collapses to "" after
  // newline-flatten + trim must not render a visible-but-empty
  // `> *""*` blockquote between sender and expiry. The call sites
  // pass `sanitizeMessage(...) || null` so an empty input short-
  // circuits at the outer `if (personalMessage)` today, but a
  // future caller that bypasses that contract would otherwise hit
  // the empty-quote regression.
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
    // EXACTLY 2 lines — same orphan-blank-line guard as the no-
    // personalMessage path above; an empty blockquote line creeping
    // back in would push this to 3 and render visibly in Discord.
    expect(lines).toHaveLength(2);
    expect(lines[0]).toContain('**Vik** opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
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
