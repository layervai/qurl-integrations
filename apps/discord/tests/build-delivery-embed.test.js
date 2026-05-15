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
const { buildDeliveryPayload, buildRevokedDMPayload, resolveSenderAlias } = _test;

const baseArgs = {
  qurlLink: 'https://qurl.link/#at_test',
  // Unix seconds (matches what production computes via expiryToMs at the
  // call site). buildDeliveryPayload renders this as <t:N:R> so Discord
  // shows the recipient a live "in 24 hours" / "in 16 hours" / etc.
  expiresAt: 1735689600,  // arbitrary fixed timestamp; tests assert it survives into the embed
  personalMessage: null,
  // Author-row provenance defaults. The senderAlias-sanitization tests
  // below leave these as the default and focus assertions on the author
  // row's `name`; dedicated tests further down exercise guildName /
  // guildIconUrl edge cases (missing icon, hostile guildName, null
  // guild). Keep the default benign so a sender-side regression isn't
  // masked by a server-name surface.
  guildName: 'Acme Discord',
  guildIconUrl: 'https://cdn.discordapp.com/icons/g/icon.png',
};

beforeEach(() => { capturedEmbeds.length = 0; capturedButtons.length = 0; });

describe('buildDeliveryPayload — senderAlias sanitization (author row)', () => {
  // Sender provenance now lives in setAuthor's plaintext `name` slot
  // (the embed's "address bar"), so these assertions target the author
  // row, not the description. Description-injection is unreachable here
  // by construction — the author row doesn't render markdown — but the
  // bidi/zero-width spoof defense still applies (an RLO-prefixed name
  // would flip the visible direction of the author line just as it
  // would have inside the old `**Vik**` description slot).
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

  // Author row is plaintext (Discord doesn't render markdown in setAuthor's
  // name slot), so a `[click](https://evil.com)` alias renders as literal
  // characters rather than a clickable masked link. The plain sanitization
  // path is therefore the correct one — backslash-escapes would appear
  // visibly. Pin both halves: characters appear verbatim in the author
  // name (no escape pass), AND no clickable link sneaks into the
  // description (sender no longer renders there).
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

  // The 64-char cap is codepoint-aware (Array.from + slice + join) — a
  // surrogate pair (e.g. 🎉 / U+1F389) sitting on the boundary must not
  // be split into a lone high surrogate, which Discord renders as tofu.
  // Regression net: 63 ASCII chars + 1 emoji = 64 codepoints, all kept.
  it('does not split surrogate pairs at the 64-char boundary', () => {
    const alias = 'A'.repeat(63) + '🎉';
    buildDeliveryPayload({ ...baseArgs, senderAlias: alias });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toContain('A'.repeat(63) + '🎉');
    // No lone high surrogate (\uD83C is the high half of 🎉)
    expect(authorName).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
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
    expect(desc).toMatch(/🕐 Closes <t:1735689600:R>/);
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
  // Positive counterpart to the "no clean upper bound" reasoning in
  // the adjacent throw test — MAX_SAFE_INTEGER itself is a valid
  // integer that Number.isInteger accepts, so the validator passes it
  // through unchanged.
  it('accepts Number.MAX_SAFE_INTEGER as a positive integer (no synthetic upper bound)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: Number.MAX_SAFE_INTEGER });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain(`🕐 Closes <t:${Number.MAX_SAFE_INTEGER}:R>`);
  });

  // Note on the "beyond MAX_SAFE_INTEGER" boundary: doubles can
  // exactly represent every integer up to 2^53, so 2^53 itself is
  // still `Number.isInteger == true`. Above 2^53, additions of 1
  // round to the nearest representable double (which is also an
  // integer at that magnitude), so `Number.isInteger` keeps returning
  // true. There is no clean "finite integer that Number.isInteger
  // rejects" boundary — the rejection set is exactly: non-finite +
  // non-integer-floats + non-positive.
  //
  // it.each() over for-loop: each input gets its own test name so a
  // regression on one shape doesn't collapse into a single anonymous
  // failure. Failure output reads e.g. "(1735689600.5) throws fail-loud"
  // instead of having to dig into the loop body.
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
    // capturedButtons holds [stepThrough, trustButton] — the trust button
    // joined the row alongside Step Through (the "verify this is real"
    // affordance, brand-address-bar metaphor). Find by label rather than
    // by index so a future reorder doesn't false-pass this test.
    const stepThrough = capturedButtons.find(b => b._label === 'Step Through');
    expect(stepThrough).toBeDefined();
    expect(stepThrough._emoji).toBe('🚪');
    expect(stepThrough._style).toBe(5); // ButtonStyle.Link
    expect(stepThrough._url).toBe('https://qurl.link/#at_unique_token');
    expect(stepThrough.setURL).toHaveBeenCalledWith('https://qurl.link/#at_unique_token');
  });

  // Contract pin: buildDeliveryPayload does NOT escape markdown in
  // personalMessage. By contract (documented at commands.js:631-639),
  // the call sites pipe raw input through sanitizeMessage before
  // passing it here. This test pins that the function renders the
  // string as-is — so a future refactor that adds an internal escape
  // pass (changing the contract) will surface the change loudly via
  // this test, not silently double-escape sanitized input.
  //
  // The senderAlias path is the opposite: sanitizeDisplayName is called
  // inside buildDeliveryPayload and DOES escape, pinned by the
  // 'escapes markdown chars in alias' test above.
  it('renders personalMessage as-is (no internal markdown escape — by contract)', () => {
    const raw = '[click](https://evil.com) **bold**';
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: raw });
    const desc = capturedEmbeds[0]._description;
    // Brackets, parens, and asterisks are NOT backslash-escaped —
    // verifies buildDeliveryPayload passes them through verbatim.
    expect(desc).toContain(`> *"${raw}"*`);
    expect(desc).not.toContain('\\[click');
    expect(desc).not.toContain('\\*\\*bold');
  });

  // Regression net for the codepoint-aware cap (closes #345): a 281-
  // codepoint string ending in a 4-byte emoji at the boundary must
  // NOT split the emoji into a lone high surrogate. Array.from + slice
  // is the codepoint-aware pattern (one array element per codepoint,
  // including surrogate pairs), mirroring sanitizeDisplayName's cap.
  //
  // 🎉 (U+1F389) is a high+low surrogate pair: 2 UTF-16 units, 1
  // codepoint. The string below has 279 ASCII chars + 🎉 = 280
  // codepoints exactly, then an extra ASCII tail at codepoint 281
  // that must NOT make it through the cap.
  it('does not split surrogate pairs at the 280-codepoint personalMessage boundary (#345)', () => {
    const message = 'A'.repeat(279) + '🎉' + 'X';  // 281 codepoints; codepoint 280 = 🎉
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: message });
    const desc = capturedEmbeds[0]._description;
    // The full 🎉 survives at the boundary (intact codepoint, not split).
    expect(desc).toContain('🎉');
    // No lone high surrogate (\uD83C is the high half of 🎉).
    expect(desc).not.toMatch(/\uD83C(?![\uDC00-\uDFFF])/);
    // The trailing 'X' beyond codepoint 280 is dropped.
    expect(desc).not.toMatch(/🎉X/);
  });

  // `> ` line-prefix is the most natural blockquote-injection attempt
  // at this surface — a personalMessage that looks like it embeds its
  // own blockquote should still be wrapped in the outer `> *"..."*`,
  // not "fixed up" by the function. Pinned alongside [](), ** to
  // round out the attack-shape coverage of the no-internal-escape
  // contract. The `\n` inside the input is flattened to a space by
  // the newline-flatten pass, so the second `>` lands inline rather
  // than starting a new blockquote — that's the expected behavior.
  it('renders personalMessage with `> ` prefixes verbatim (no auto-fix of nested blockquote)', () => {
    const raw = '> faux quote\n> still faux';
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', personalMessage: raw });
    const desc = capturedEmbeds[0]._description;
    expect(desc).toContain('> *"> faux quote > still faux"*');
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
  it('renders action → personal message → expiry in that order when all three are present', () => {
    buildDeliveryPayload({
      ...baseArgs,
      senderAlias: 'Vik',
      personalMessage: 'Quarterly numbers — for your eyes only.',
      expiresAt: 1735689600,
    });
    const desc = capturedEmbeds[0]._description;
    // split-then-index-by-line over indexOf-substring-position: an
    // `indexOf` check would false-pass if a future personalMessage
    // happened to contain the literal substring `"Closes <t:"`,
    // ordering both indices the same way. Pin position by line.
    //
    // Sender no longer appears in the description — it's in the
    // author row (asserted separately). Line 0 is now the action
    // statement: "opened a door for you."
    const lines = desc.split('\n');
    expect(lines).toHaveLength(3);
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toContain('Quarterly numbers');
    expect(lines[2]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
    // And no addFields call — folded entirely into description.
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  // When personalMessage is absent, the description still renders
  // action → expiry in order (no orphaned blank line between them).
  it('renders action → expiry with no gap when personalMessage is omitted', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', expiresAt: 1735689600 });
    const desc = capturedEmbeds[0]._description;
    const lines = desc.split('\n');
    // EXACTLY 2 lines — guards against an orphan blank line creeping
    // in between action and expiry when personalMessage is absent.
    // Do NOT loosen to `toBeGreaterThanOrEqual(2)`: a 3-line desc
    // would mean someone re-introduced `descLines.push('')` for
    // padding, which renders as a visible empty row in Discord.
    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
    // Mirror the all-three-pieces ordering test: also assert addFields
    // is never called on the no-personalMessage path. Belt-and-braces
    // against a future regression that adds a field only when one of
    // the slots is empty (e.g. a "no message attached" placeholder).
    expect(capturedEmbeds[0].addFields).not.toHaveBeenCalled();
  });

  // Belt-and-braces: a personalMessage that collapses to "" after
  // newline-flatten + trim must not render a visible-but-empty
  // `> *""*` blockquote between action and expiry. The call sites
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
    expect(lines[0]).toBe('opened a door for you.');
    expect(lines[1]).toMatch(/^🕐 Closes <t:1735689600:R>$/);
  });
});

describe('buildDeliveryPayload — author row provenance', () => {
  // The author row is the embed's "address bar" — anchored top, visually
  // distinct from the description, the closest analog Discord offers
  // to a browser's origin display. These tests pin the composition rule
  // (`${sender} · ${guildName}`) and the iconURL contract (only set when
  // the guild has one — bare `undefined`, not `null`, on omission).
  it('composes author name as "sender · guildName" when both are present', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: 'Acme Discord' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik · Acme Discord');
  });

  it('falls back to sender-only when guildName is missing/empty', () => {
    for (const guildName of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName });
      // No trailing ` · ` separator when no guild is known. Defended for
      // the edge case where interaction.guild is null (commands are
      // guild-only so this shouldn't fire in production).
      expect(capturedEmbeds[0]._author.name).toBe('Vik');
    }
  });

  // The exact hostile input the bidi-strip exists to defend against:
  // a truthy guild name composed ENTIRELY of strip-eligible chars
  // (RLO, ZWSP, soft-hyphen, etc.). sanitizeDisplayName* helpers
  // substitute the "Someone" display-name fallback on all-strip
  // input, which would produce the nonsense author row `Vik · Someone`
  // — degrading the trust signal on the exact input the strip exists
  // to neutralize. The implementation routes guildName through
  // stripBidiAndControls (no fallback) so all-strip collapses to ''
  // and the author row falls back to sender-only.
  it('falls back to sender-only when guildName is entirely strip-eligible chars (no "Someone" leak)', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: '‮​­' });
    expect(capturedEmbeds[0]._author.name).toBe('Vik');
    expect(capturedEmbeds[0]._author.name).not.toContain('Someone');
  });

  // Mirrors the senderAlias 64-codepoint cap. The same defensive upper
  // bound applies to the guild name — Discord caps guild names at 100
  // chars natively, but a forged interaction / future API shape change
  // could exceed that. 64 codepoints keeps the combined `sender · guild`
  // author line well under Discord's 256-char author.name limit even
  // when both halves max out.
  it('caps long guildName at 64 codepoints', () => {
    const long = 'G'.repeat(200);
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildName: long });
    const authorName = capturedEmbeds[0]._author.name;
    expect(authorName).toBe('Vik · ' + 'G'.repeat(64));
  });

  it('applies plain (non-markdown-escaping) sanitization to guildName', () => {
    // Same bidi/zero-width spoof defense the senderAlias path gets — an
    // attacker controlling guild name could RLO-flip the author row.
    // Author surface is plaintext, so the markdown-escape pass doesn't
    // apply; sanitizeDisplayNamePlain is the right tool here.
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
    // discord.js wants bare `undefined` (NOT explicit `null`) for "no
    // icon" — some versions stringify null into the URL slot. Pin that
    // the key is absent rather than present-and-null.
    for (const noIcon of [null, undefined, '']) {
      capturedEmbeds.length = 0;
      buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik', guildIconUrl: noIcon });
      expect(capturedEmbeds[0]._author).not.toHaveProperty('iconURL');
    }
  });
});

describe('buildDeliveryPayload — footer + trust button', () => {
  // Footer reinforces the destination domain the way a browser shows
  // where a link points before you click. Literal string (not derived
  // from qurlLink) so a future minted-link subdomain doesn't drift the
  // recipient-visible domain.
  it('sets a footer naming the destination domain', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(capturedEmbeds[0]._footer).toEqual({ text: 'opens qurl.link' });
  });

  // The trust button is the "click the lock to verify" affordance —
  // a first-time recipient can hit qURL's public landing to confirm
  // the brand exists before clicking Step Through. Locks the URL +
  // Link style so a future refactor that swaps to a Primary-style
  // (custom_id-only) button — which would silently break the click-
  // to-landing flow — surfaces here.
  it('builds a Link-style "What is qURL?" trust button pointing at the brand landing', () => {
    buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    const trust = capturedButtons.find(b => b._label === 'What is qURL?');
    expect(trust).toBeDefined();
    expect(trust._emoji).toBe('🛡');
    expect(trust._style).toBe(5); // ButtonStyle.Link
    expect(trust._url).toBe('https://layerv.ai/qurl/');
  });

  // Both buttons live in a single ActionRow next to each other — the
  // verify path is co-located with the primary action (matches the
  // brand-address-bar metaphor) rather than tucked into a separate
  // row below.
  it('ships Step Through and What is qURL? in the same ActionRow', () => {
    const { components } = buildDeliveryPayload({ ...baseArgs, senderAlias: 'Vik' });
    expect(components).toHaveLength(1);
    expect(components[0].addComponents).toHaveBeenCalledTimes(1);
    // addComponents called with both buttons; pull them out and pin
    // the order (step-through first, trust second).
    const args = components[0].addComponents.mock.calls[0];
    expect(args).toHaveLength(2);
    expect(args[0]._label).toBe('Step Through');
    expect(args[1]._label).toBe('What is qURL?');
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
    // Discord PATCH /messages does NOT clear unset fields. If this assertion
    // ever flips to `undefined`, the original Step Through button would
    // remain live in the recipient's DM after revoke — pointing at a
    // dead qurl resource.
    const payload = buildRevokedDMPayload({ senderAlias: 'Vik' });
    expect(payload.components).toEqual([]);
  });

  it('strips bidi / zero-width spoof chars from the alias before rendering', () => {
    capturedEmbeds.length = 0;
    buildRevokedDMPayload({ senderAlias: '‮Admin' });
    expect(capturedEmbeds[0]._description).not.toContain('‮');
  });
});
