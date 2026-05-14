/**
 * Unit tests for src/recipient-parser.js — the `recipients:` slash-
 * option mention parser used by /qurl file and /qurl map.
 *
 * Mocks config (just QURL_SEND_MAX_RECIPIENTS) and uses synthetic
 * `interaction` objects with the guild.members.cache / guild.roles.cache
 * shape discord.js produces. No real Discord API.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

// Mock cap chosen for test convenience (prod default is 50 — see
// QURL_SEND_MAX_RECIPIENTS in src/config.js). Tests pin the cap
// behavior at 25 so the boundary cases stay small + readable.
jest.mock('../src/config', () => ({
  QURL_SEND_MAX_RECIPIENTS: 25,
}));

const {
  parseRecipientMentions,
  isVoiceChannelType,
  MAX_INPUT_LENGTH,
  MAX_INVALID_TOKEN_LENGTH,
  VOICE_CHANNEL_TYPE,
  STAGE_VOICE_CHANNEL_TYPE,
  VIEW_CHANNEL_PERMISSION,
} = require('../src/recipient-parser');
const logger = require('../src/logger');

// Build a synthetic interaction with the cache shape the parser reads.
// Pass `users` as { id → { bot } }. Sender defaults to '900000000000000001'.
//
// `roles` accepts two shapes per entry:
//   array form  → `{ '7000': ['101', '102'] }` (members; defaults to
//                 `mentionable: true` so the #326 gate doesn't reject
//                 the existing test corpus — prod-side roles MUST set
//                 `mentionable: true` to bypass the gate without
//                 MENTION_EVERYONE).
//   object form → `{ '7000': { members: [...], mentionable: false } }`
//                 (opt in to non-mentionable for #326-gate tests).
//
// `guildId` is the guild's snowflake (defaults to undefined to match
// existing tests' shape). Set to make `<@&{guildId}>` route through
// the @everyone-role wire-form guard.
//
// IDs are all-numeric to match real Discord snowflakes — the parser's
// regex is `/<@!?(\d+)>/` so letter-prefixed test fixtures would silently
// drop every mention, masking real coverage.
function makeInteraction({ senderId = '900000000000000001', users = {}, roles = {}, channels = {}, guildId } = {}) {
  const memberCache = new Map();
  for (const [id, attrs] of Object.entries(users)) {
    memberCache.set(id, { user: { id, bot: !!attrs.bot } });
  }
  // Discord's role.members is a Collection<id, GuildMember>. We
  // simulate with a Map; the parser iterates with for-of which both
  // honor. Add the member to the cache when it's listed under a role
  // so the bot filter has something to look up.
  const roleCache = new Map();
  for (const [roleId, spec] of Object.entries(roles)) {
    const isObjectSpec = spec && typeof spec === 'object' && !Array.isArray(spec);
    const memberIds = isObjectSpec ? (spec.members || []) : spec;
    const mentionable = isObjectSpec ? spec.mentionable !== false : true;
    const roleMembers = new Map();
    for (const mid of memberIds) {
      const existing = memberCache.get(mid) ?? { user: { id: mid, bot: false } };
      memberCache.set(mid, existing);
      roleMembers.set(mid, existing);
    }
    roleCache.set(roleId, { id: roleId, name: `role-${roleId}`, members: roleMembers, mentionable });
  }
  // Channel shape: { id → { type, members: [id, ...], viewable? } }
  //   type     numeric ChannelType (2 = GuildVoice, 13 = GuildStageVoice;
  //            any other value tests the non-voice rejection path).
  //   members  voice-connected member IDs (added to guild member cache so
  //            the bot filter + post-filter dedupe semantics match
  //            production).
  //   viewable defaults to true; set false to test the ViewChannel-gate
  //            rejection path (private voice channels the bot has cached
  //            but the sender can't see in their client).
  const channelCache = new Map();
  for (const [channelId, attrs] of Object.entries(channels)) {
    const memberIds = attrs.members || [];
    const chMembers = new Map();
    for (const mid of memberIds) {
      const existing = memberCache.get(mid) ?? { user: { id: mid, bot: false } };
      memberCache.set(mid, existing);
      chMembers.set(mid, existing);
    }
    const viewable = attrs.viewable !== false;
    // permissionsFor is the read-of-record for the ViewChannel gate.
    // Real discord.js returns a Readonly<PermissionsBitField>; the
    // mock returns the minimum shape the parser exercises (`.has`).
    // Argument is ignored — viewability is per-channel-fixture, not
    // per-caller, which is sufficient because the parser only ever
    // asks about the invoking user.
    channelCache.set(channelId, {
      id: channelId,
      type: attrs.type,
      members: chMembers,
      permissionsFor: () => ({
        has: (bit) => bit !== VIEW_CHANNEL_PERMISSION || viewable,
      }),
    });
  }
  return {
    user: { id: senderId },
    member: { id: senderId },
    guild: {
      id: guildId,
      members: { cache: memberCache },
      roles: { cache: roleCache },
      channels: { cache: channelCache },
    },
  };
}

describe('parseRecipientMentions — basic shape', () => {
  test('returns empty result for null/undefined/empty input', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(null, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions(undefined, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('', int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('   \t\n', int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('returns empty result when raw is a non-string (defense vs caller bugs)', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(42, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions({}, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions([], int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('extracts a single user mention', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111>', int))
      .toMatchObject({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('accepts both <@id> and <@!id> forms (legacy nickname mention)', () => {
    const int = makeInteraction({
      users: { '111111111111111111': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@111111111111111111> <@!222222222222222222>', int))
      .toMatchObject({ ids: ['111111111111111111', '222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('dedupes repeated mentions', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111> <@111111111111111111> <@!111111111111111111>', int))
      .toMatchObject({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('cappedCount is 0 when input has no mentions (no false-positive cap signal)', () => {
    // Empty-input tests above pin null/undefined/'' paths; this pins
    // the "input had content but no mentions" case explicitly so a
    // future caller reading `cappedCount > 0` to infer "user pasted
    // too many" can't accidentally fire on plain bare-name input.
    const int = makeInteraction();
    const res = parseRecipientMentions('alice bob carol', int);
    expect(res.ids).toEqual([]);
    expect(res.cappedCount).toBe(0);
    expect(res.invalidTokens).toEqual(['alice', 'bob', 'carol']);
  });

  test('handles whitespace + comma + mixed separators', () => {
    const int = makeInteraction({
      users: { '111': {}, '222': {}, '333': {} },
    });
    expect(parseRecipientMentions('<@111>,<@222>  <@333>', int).ids)
      .toEqual(['111', '222', '333']);
  });
});

describe('parseRecipientMentions — filtering', () => {
  test('keeps the sender (self-send is supported)', () => {
    // Self-send: the sender deliberately included themselves via
    // `@me`. Parser does NOT filter — the confirm card surfaces a
    // neutral "Send includes you." notice instead.
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001> <@222222222222222222>', int).ids.sort())
      .toEqual(['222222222222222222', '900000000000000001']);
  });

  test('sender alone is a legitimate single-recipient self-send', () => {
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001>', int))
      .toMatchObject({ ids: ['900000000000000001'], invalidTokens: [], cappedCount: 0 });
  });

  test('excludes bots flagged in the member cache', () => {
    const int = makeInteraction({
      users: { '111': { bot: true }, '222': {} },
    });
    expect(parseRecipientMentions('<@111> <@222>', int))
      .toMatchObject({ ids: ['222'], invalidTokens: [], cappedCount: 0 });
  });

  test('completely empty interaction ({}) does not throw, returns empty result', () => {
    // The `interaction == null` early-return handles null/undefined,
    // but a truthy empty object still has to walk the optional chains
    // (`interaction.guild?.roles?.cache`) without crashing. Pin that
    // `interaction = {}` is tolerated — a future refactor that
    // switched `.guild?.x` to `.guild.x?` on the assumption "we've
    // already null-checked" would surface here.
    expect(parseRecipientMentions('<@111>', {}))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0 });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
    // Bot filter relies on member.cache; a cache miss (e.g. cold cache
    // or member not yet fetched) cannot tell us "is this a bot." The
    // parser keeps the ID and lets the downstream send-pipeline's bot
    // check (existing form's "Cannot send to a bot" path) catch it.
    const int = makeInteraction({});  // empty cache
    expect(parseRecipientMentions('<@555>', int))
      .toMatchObject({ ids: ['555'], invalidTokens: [], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — role mentions', () => {
  test('expands a role to its current members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102', '103'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: ['101', '102', '103'], invalidTokens: [], cappedCount: 0 });
  });

  test('merges role expansion with direct user mentions, deduped', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102'] },
    });
    expect(parseRecipientMentions('<@103> <@&7000> <@101>', int).ids.sort())
      .toEqual(['101', '102', '103']);
  });

  test('role expansion includes sender but excludes bots', () => {
    // Self-send: sender expanded out of a role mention stays in the
    // recipient set. Bots in the role are still filtered.
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true }, '101': {} },
      roles: { '7000': ['900', '801', '101'] },
    });
    expect(parseRecipientMentions('<@&7000>', int).ids.sort())
      .toEqual(['101', '900']);
  });

  test('role unknown to the guild lands in invalidTokens', () => {
    const int = makeInteraction({});
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('role with no usable members (all bots) lands in invalidTokens', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '801': { bot: true } },
      roles: { '7000': ['801'] },
    });
    const res = parseRecipientMentions('<@&7000>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<@&7000>']);
  });

  test('role with only the sender expands to the sender (self-only role is valid)', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {} },
      roles: { '7000': ['900'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: ['900'], invalidTokens: [], cappedCount: 0 });
  });

  test('DM context (guild=undefined) treats role mentions as invalid', () => {
    const int = { user: { id: '900' }, guild: undefined };
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('DM context (guild=null) also treats role mentions as invalid', () => {
    // discord.js returns `null` (not `undefined`) for DM-context
    // guilds. Optional chaining handles both, but pin the runtime
    // shape explicitly so a future refactor that switched
    // `guild?.roles?.cache` to `guild.roles?.cache` would surface
    // on this test, not in prod.
    const int = { user: { id: '900' }, guild: null };
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('repeated residue tokens dedupe in invalidTokens (symmetric with role-error dedup)', () => {
    // `<#456> <#456>` previously surfaced two entries; now dedupes
    // via `residueSeen` (parallel to the role-error path's
    // `invalidRoleIds`). Caller's "couldn't parse: X, X" embed
    // isn't user-hostile.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> alice alice bob alice', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('repeated invalid role mention dedupes in invalidTokens', () => {
    // `<@&999> <@&999>` against a guild missing role 999 must yield
    // ONE invalidTokens entry, not two. Without the role-id dedupe,
    // matchAll iterates each input occurrence and pushes the raw
    // token each time. Symmetric with the residue dedupe above.
    const int = makeInteraction({});  // no role 999
    expect(parseRecipientMentions('<@&999> <@&999> <@&999>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&999>'], cappedCount: 0 });
  });

  test('direct mentions always win the cap over role-expansion members', () => {
    // Pass 1 (user mentions) runs before pass 2 (role expansion), so
    // direct mentions are inserted into `ids` first and survive the
    // cap. A future refactor that swapped pass order — or interleaved
    // mention/role parsing — could silently displace direct mentions
    // by large role expansions. Pin the invariant.
    const roleMembers = [];
    const users = {};
    // 5 direct-mention users (distinct snowflake range so we can
    // assert presence without ambiguity).
    const directIds = ['8000000001', '8000000002', '8000000003', '8000000004', '8000000005'];
    for (const id of directIds) users[id] = {};
    for (let i = 0; i < 50; i++) {
      const id = `${5000000000 + i}`;
      users[id] = {};
      roleMembers.push(id);
    }
    const int = makeInteraction({ users, roles: { '7000': roleMembers } });
    // Role mention FIRST in input order; direct mentions follow.
    // With cap=25, the 5 direct mentions must all survive — the
    // role's overflow goes to cappedCount, not the direct slots.
    const directMentions = directIds.map(id => `<@${id}>`).join(' ');
    const res = parseRecipientMentions(`<@&7000> ${directMentions}`, int);
    expect(res.ids).toHaveLength(25);
    expect(res.ids).toEqual(expect.arrayContaining(directIds));
    // 55 unique candidates - 25 cap = 30 dropped.
    expect(res.cappedCount).toBe(30);
  });

  test('user listed BOTH directly AND via a role mention appears once in ids, role is not flagged useless', () => {
    // The "merges role expansion with direct user mentions" test
    // above exercises this implicitly (user 101 is in role 7000 AND
    // mentioned directly), but a fixture that EXPLICITLY overlaps
    // makes two invariants non-incidental:
    // (a) ids.length === 1 (Set dedup — a regression re-adding role
    //     members without the dedupe guard surfaces here);
    // (b) invalidTokens === [] (the role contributed via dedupe so
    //     `usable++` fires before the seen-check — a refactor
    //     swapping `usable++` to AFTER `seen.has` would flag this
    //     role as useless and push the token to invalidTokens).
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': ['101'] },
    });
    expect(parseRecipientMentions('<@101> <@&7000>', int))
      .toMatchObject({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
  });

  test('role mention repeated in input expands once (no double-counting)', () => {
    // matchAll iterates every regex hit, so a repeated `<@&id>` would
    // re-enter the expansion loop and re-add the same members. ids is
    // a Set so they dedupe; pin that AND that invalidTokens stays empty
    // (the second hit shouldn't trip the "no usable members" path on a
    // role whose members were already added).
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': ['101', '102'] },
    });
    const res = parseRecipientMentions('<@&7000> <@&7000> <@&7000>', int);
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.invalidTokens).toEqual([]);
  });
});

describe('parseRecipientMentions — role-mention permission gate (#326)', () => {
  // Issue #326 parity: <@&roleId> for a non-mentionable role requires
  // MENTION_EVERYONE (allowMassMention). Mirrors Discord's in-chat
  // gate; closes the privilege-escalation surface where a non-admin
  // could `/qurl file recipients:<@&adminRoleId>` to fan-out to an
  // admin-only role.

  test('mentionable: false WITHOUT allowMassMention → roleMentionsDenied entry, NOT expanded', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: false });
    expect(res.ids).toEqual([]);
    expect(res.roleMentionsDenied).toEqual(['7000']);
    // NOT in invalidTokens — distinct surface lets the caller emit
    // permission-specific copy instead of "couldn't parse."
    expect(res.invalidTokens).toEqual([]);
  });

  test('mentionable: false WITH allowMassMention → expands normally, no deny', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.roleMentionsDenied).toEqual([]);
  });

  test('mentionable: true WITHOUT allowMassMention → expands normally (per-role bypass)', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: true } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: false });
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.roleMentionsDenied).toEqual([]);
  });

  test('multiple denied roles surface independently (array, not boolean)', () => {
    // Per issue spec: a single send can attempt MULTIPLE denied roles —
    // surface each ID so the caller renders per-role copy with the name.
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '201': {}, '202': {} },
      roles: {
        '7000': { members: ['101', '102'], mentionable: false },
        '7001': { members: ['201', '202'], mentionable: false },
      },
    });
    const res = parseRecipientMentions('<@&7000> <@&7001>', int, { allowMassMention: false });
    expect(res.roleMentionsDenied.sort()).toEqual(['7000', '7001']);
    expect(res.ids).toEqual([]);
  });

  test('repeated denied role mention dedupes (one entry, not three)', () => {
    // Same dedupe pattern as invalidRoleIds (`<@&999> <@&999>` →
    // one invalidTokens entry). A naive matchAll loop would push the
    // raw ID per occurrence; the parallel Set guards against that.
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': { members: ['101'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000> <@&7000> <@&7000>', int, { allowMassMention: false });
    expect(res.roleMentionsDenied).toEqual(['7000']);
  });

  test('mix of denied + allowed roles in one input → only denied lands in roleMentionsDenied', () => {
    // Cohabitation: a mentionable role still expands while the
    // non-mentionable peer surfaces as denied. Without per-role
    // gating, a single denied role would either zero out the whole
    // send or silently expand alongside the allowed role.
    const int = makeInteraction({
      users: { '101': {}, '201': {} },
      roles: {
        '7000': { members: ['101'], mentionable: true },     // allowed
        '7001': { members: ['201'], mentionable: false },    // denied
      },
    });
    const res = parseRecipientMentions('<@&7000> <@&7001>', int, { allowMassMention: false });
    expect(res.ids).toEqual(['101']);
    expect(res.roleMentionsDenied).toEqual(['7001']);
  });

  test('<@&{guildId}> wire form routes to massMentionDenied (NOT roleMentionsDenied)', () => {
    // The @everyone role's ID equals guild.id. The wire form should
    // land in massMentionDenied so the caller emits "@everyone
    // requires Mention Everyone" copy, NOT the per-role copy. This
    // matches the picker path's `isEveryoneRole` short-circuit.
    const int = makeInteraction({
      guildId: '999',
      users: { '101': {} },
      roles: { '7000': { members: ['101'], mentionable: true } },
    });
    const res = parseRecipientMentions('<@&999>', int, { allowMassMention: false });
    expect(res.massMentionDenied).toBe(true);
    expect(res.roleMentionsDenied).toEqual([]);
    // NOT in invalidTokens either — explicit-route to the @everyone
    // channel means no "couldn't parse" leak.
    expect(res.invalidTokens).toEqual([]);
  });

  test('<@&{guildId}> with allowMassMention → triggers @everyone expansion via guild.members.cache', () => {
    // Parity with the picker path: when allowed, the @everyone-role
    // wire form expands through guild.members.cache (NOT role.members)
    // because discord.js's @everyone role.members is unreliable. Pin
    // the routing: `everyonePresent` flag fires, dedicated expansion
    // pass walks the member cache.
    const int = makeInteraction({
      guildId: '999',
      users: { '101': {}, '102': {}, '801': { bot: true } },
    });
    const res = parseRecipientMentions('<@&999>', int, { allowMassMention: true });
    expect(res.massMentionDenied).toBe(false);
    expect(res.roleMentionsDenied).toEqual([]);
    expect(res.ids.sort()).toEqual(['101', '102']);
  });

  test('denied-role does NOT contaminate cappedCount (skipped before consider())', () => {
    // The gate fires BEFORE the role-loop's `consider()` calls, so
    // a denied role's members never enter `seen`. A regression that
    // hoisted the dedupe-add above the gate would inflate cappedCount
    // by the denied role's size — caller would surface a misleading
    // "you typed 50, we kept 25" warning.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: false });
    expect(res.cappedCount).toBe(0);
  });
});

describe('voice-channel type constants (discord.js ChannelType pin)', () => {
  // Pins the numeric values the parser hard-codes (`channel.type ===
  // 2 || channel.type === 13`). A discord.js bump that renumbers
  // GuildVoice / GuildStageVoice would break the voice-everyone path
  // silently without these assertions — the parser would treat real
  // voice channels as non-voice and reject the mention.
  test('VOICE_CHANNEL_TYPE is 2 (GuildVoice)', () => {
    expect(VOICE_CHANNEL_TYPE).toBe(2);
  });
  test('STAGE_VOICE_CHANNEL_TYPE is 13 (GuildStageVoice)', () => {
    expect(STAGE_VOICE_CHANNEL_TYPE).toBe(13);
  });
  test('isVoiceChannelType matches both voice + stage-voice', () => {
    expect(isVoiceChannelType(2)).toBe(true);
    expect(isVoiceChannelType(13)).toBe(true);
  });
  test('isVoiceChannelType rejects every other channel type (text, category, forum, etc.)', () => {
    expect(isVoiceChannelType(0)).toBe(false);   // GuildText
    expect(isVoiceChannelType(4)).toBe(false);   // GuildCategory
    expect(isVoiceChannelType(5)).toBe(false);   // GuildAnnouncement
    expect(isVoiceChannelType(10)).toBe(false);  // AnnouncementThread
    expect(isVoiceChannelType(15)).toBe(false);  // GuildForum
    expect(isVoiceChannelType(undefined)).toBe(false);
    expect(isVoiceChannelType(null)).toBe(false);
  });

  // ViewChannel permission bit pinned at 1 << 10 (1024). A discord.js
  // bump that renumbers the bit would silently break the private-
  // voice-channel-leak defense — this spec keeps the contract honest.
  test('VIEW_CHANNEL_PERMISSION is 1 << 10', () => {
    expect(VIEW_CHANNEL_PERMISSION).toBe(1024n);
  });
});

describe('parseRecipientMentions — channel mentions (voice / stage-voice)', () => {
  // ChannelType numeric values mirrored from discord.js:
  //   GuildVoice = 2
  //   GuildStageVoice = 13
  //   GuildText = 0 (rejected — non-voice)
  const GUILD_VOICE = 2;
  const GUILD_STAGE_VOICE = 13;
  const GUILD_TEXT = 0;

  test('voice channel expands to currently-connected non-bot members', () => {
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_VOICE, members: ['111', '222', '333'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids.sort()).toEqual(['111', '222', '333']);
    expect(res.invalidTokens).toEqual([]);
  });

  test('stage-voice channel expands the same way as voice', () => {
    const int = makeInteraction({
      channels: {
        '501': { type: GUILD_STAGE_VOICE, members: ['111', '222'] },
      },
    });
    const res = parseRecipientMentions('<#501>', int);
    expect(res.ids.sort()).toEqual(['111', '222']);
    expect(res.invalidTokens).toEqual([]);
  });

  test('voice channel filters bots from the connected set', () => {
    const int = makeInteraction({
      users: { '801': { bot: true } },
      channels: {
        '500': { type: GUILD_VOICE, members: ['111', '801', '222'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids.sort()).toEqual(['111', '222']);
    expect(res.invalidTokens).toEqual([]);
  });

  test('empty voice channel lands in invalidTokens (no silent empty expansion)', () => {
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_VOICE, members: [] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('voice channel with only bots lands in invalidTokens', () => {
    const int = makeInteraction({
      users: { '801': { bot: true }, '802': { bot: true } },
      channels: {
        '500': { type: GUILD_VOICE, members: ['801', '802'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('voice channel the sender CANNOT see (ViewChannel denied) lands in invalidTokens — no private-channel-leak', () => {
    // The bot's channels.cache holds every channel it can see, so
    // without the ViewChannel gate a user could DM-blast members of
    // a private voice channel just by knowing its snowflake. Pin
    // that channels with viewable:false fall through to
    // invalidTokens despite type=GuildVoice + non-empty members.
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_VOICE, members: ['111', '222'], viewable: false },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('missing permissionsFor on the channel cache (degraded shape) fails closed', () => {
    // Defense-in-depth: real discord.js always supplies permissionsFor
    // on GuildChannel, but a future shape change or partially-mocked
    // test fixture would otherwise silently bypass the gate. Pin
    // the fail-closed behavior so the bypass surfaces as "couldn't
    // expand" rather than a silent leak.
    const int = makeInteraction();
    int.guild.channels.cache = new Map([[
      '500',
      // intentionally NO permissionsFor — the parser must treat
      // this as denied, not as "assume allowed".
      { id: '500', type: GUILD_VOICE, members: new Map([['111', { user: { id: '111', bot: false } }]]) },
    ]]);
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('text channel mention is rejected into invalidTokens (no @everyone-in-text-channel regression)', () => {
    // Pins PR #174's fix: text-channel "everyone" used to expand to
    // every ViewChannel-permission holder, which on default Discord
    // is the entire guild. We REJECT non-voice channel mentions
    // outright so a user typing `<#text-channel>` doesn't accidentally
    // blast the whole server.
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_TEXT, members: ['111', '222'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('unknown channel (cache miss) lands in invalidTokens', () => {
    const int = makeInteraction({ channels: {} });
    const res = parseRecipientMentions('<#999>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#999>']);
  });

  test('dedupes repeated channel mentions into one invalidTokens entry', () => {
    // Symmetric with the role-error dedup path (`<@&999> <@&999>`
    // produces one entry, not two). A caller's "couldn't parse: X, X"
    // embed is hostile.
    const int = makeInteraction({ channels: {} });
    const res = parseRecipientMentions('<#999> <#999>', int);
    expect(res.invalidTokens).toEqual(['<#999>']);
  });

  test('voice expansion combines with explicit user mentions (dedupe across paths)', () => {
    const int = makeInteraction({
      users: { '111': {}, '222': {}, '333': {} },
      channels: {
        '500': { type: GUILD_VOICE, members: ['111', '222'] },
      },
    });
    // '111' appears in both the explicit mention AND the voice
    // channel; should dedupe to one entry. '333' is mention-only.
    const res = parseRecipientMentions('<@111> <@333> <#500>', int);
    expect(res.ids.sort()).toEqual(['111', '222', '333']);
    expect(res.invalidTokens).toEqual([]);
  });

  test('channel mention does not show up in invalidTokens twice (channel-expansion + residue-strip)', () => {
    // The residue-strip pass also strips <#id>; without that, a
    // resolved voice channel would surface as both an expansion AND
    // a leftover invalid token. Test pins the strip.
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_VOICE, members: ['111'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.invalidTokens).toEqual([]);
  });

  test('DM-context (no guild) lands channel mention in invalidTokens', () => {
    // Same cold-cache / DM degradation path as user mentions.
    const res = parseRecipientMentions('<#500>', { user: { id: '900000000000000001' } });
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('rejected channel mention is NOT double-reported by the residue-strip pass', () => {
    // Pins the load-bearing CHANNEL_MENTION_RE strip in the residue
    // pass. Without it, a rejected channel mention would surface
    // TWICE in invalidTokens: once from the channel-expansion loop
    // (cache miss / non-voice / view-denied → pushInvalidIfNew) and
    // once from the residue-strip pass (split on whitespace would
    // tokenize the surviving `<#999>` as a leftover invalid token).
    // The .toEqual length-1 assertion is what guards the strip; if
    // the strip were ever removed, this fails loudly rather than
    // surfacing as a "couldn't parse: <#999>, <#999>" user-visible
    // hostile embed.
    const int = makeInteraction({ channels: {} });
    const res = parseRecipientMentions('<#999>', int);
    expect(res.invalidTokens).toEqual(['<#999>']);
    expect(res.invalidTokens.length).toBe(1);
  });

  test('<#voice> succeeds independently when sender lacks MENTION_EVERYONE (massMentionDenied stays orthogonal)', () => {
    // Cross-feature interaction: a sender who can see the voice
    // channel but lacks MENTION_EVERYONE should still get the
    // voice expansion; @everyone should land in massMentionDenied
    // independently. The two paths must not block each other —
    // pinning the orthogonality so a future refactor doesn't
    // entangle them (e.g., short-circuiting all expansions when
    // any one is denied).
    const int = makeInteraction({
      users: { '111': {}, '222': {} },
      channels: { '500': { type: 2, members: ['111', '222'] } },
    });
    const res = parseRecipientMentions('@everyone <#500>', int, { allowMassMention: false });
    expect(res.ids.sort()).toEqual(['111', '222']);
    expect(res.massMentionDenied).toBe(true);
    // @everyone was stripped from invalidTokens (per the
    // allowMassMention contract) AND the voice channel expanded
    // cleanly — neither path interferes with the other.
    expect(res.invalidTokens).toEqual([]);
  });

  test('interaction.member undefined with present guild fails closed (no silent view bypass)', () => {
    // Defense-in-depth: real discord.js always populates member for
    // guild slash commands. A degraded interaction shape (test mock
    // bug, future discord.js refactor) where member is undefined
    // must NOT silently bypass the ViewChannel gate — passing
    // undefined to channel.permissionsFor returns null in real
    // discord.js, which fails closed via the `!viewerPerms` branch.
    // This test pins that contract against a mock that returns null
    // from permissionsFor when called with undefined.
    const channelCache = new Map([[
      '500',
      {
        id: '500',
        type: 2, // GuildVoice
        members: new Map([['111', { user: { id: '111', bot: false } }]]),
        // Returns null when called with undefined member, matching
        // real discord.js behavior.
        permissionsFor: (memberOrId) => memberOrId == null ? null : ({ has: () => true }),
      },
    ]]);
    const int = {
      user: { id: '900000000000000001' },
      // member intentionally absent
      guild: {
        members: { cache: new Map([['111', { user: { id: '111', bot: false } }]]) },
        roles: { cache: new Map() },
        channels: { cache: channelCache },
      },
    };
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('explicit channel mentions claim cap slots BEFORE @everyone (priority ordering)', () => {
    // The parser's channel-expansion pass runs BEFORE @everyone so
    // direct mentions (user / role / channel) claim cap slots first
    // and @everyone fills the remainder. Reversing the order would
    // silently drop explicit mentions when the cache is partial or
    // the cap is tight — pin the contract here.
    //
    // Setup: cap = 25 (test default), 100 unrelated guild members,
    // 3-member voice channel. With voice running BEFORE @everyone,
    // the 3 voice members win cap slots first and @everyone fills
    // the remaining 22.
    const users = {};
    for (let i = 1; i <= 100; i++) {
      users[`9${String(i).padStart(17, '0')}`] = {};
    }
    const voiceMembers = ['111', '222', '333'];
    for (const id of voiceMembers) users[id] = {};
    const int = makeInteraction({
      users,
      channels: { '500': { type: GUILD_VOICE, members: voiceMembers } },
    });
    const res = parseRecipientMentions('@everyone <#500>', int, { allowMassMention: true });
    // All three voice members must appear (proof: they claimed cap
    // slots before the larger @everyone pool overflowed the cap).
    for (const id of voiceMembers) expect(res.ids).toContain(id);
    expect(res.ids).toHaveLength(25);
  });

  test('voice channel after cap-filling user mentions: silently no-ops (NOT marked invalid)', () => {
    // Bug-guard: early break on `ids.size >= cap` must NOT mark the
    // channel invalid when the cap was already filled by upstream
    // expansions. The channel was perfectly valid; it just had no
    // room to contribute. Without `capHit` tracking, the channel
    // would surface as an invalidTokens entry, misleading the user.
    const users = { '111': {}, '222': {}, '333': {}, 'aaa': {}, 'bbb': {} };
    const int = makeInteraction({
      users,
      channels: { '500': { type: GUILD_VOICE, members: ['aaa', 'bbb'] } },
    });
    // Override cap to a small value so the user mentions alone fill it.
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 3;
    try {
      const res = parseRecipientMentions('<@111> <@222> <@333> <#500>', int);
      expect(res.ids.sort()).toEqual(['111', '222', '333']);
      // Channel was valid; it just contributed nothing because the cap was full.
      expect(res.invalidTokens).toEqual([]);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });
});

describe('parseRecipientMentions — @everyone (allowMassMention)', () => {
  test('allowed: expands to all non-bot guild members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '801': { bot: true }, '103': {} },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102', '103']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed: merges with direct mentions, deduped', () => {
    // Same user reached via both `@everyone` and a direct `<@id>` is
    // contributed once (the parser uses a single `consider()` that
    // dedupes via `seen`). The role-expansion test pins the same
    // contract for roles; this pins it for the mass-mention path.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('<@101> @everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
  });

  test('denied (default): surfaces massMentionDenied=true, no expansion, no invalidTokens entry', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyone', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(true);
  });

  test('denied: explicit allowMassMention=false is equivalent to default', () => {
    const int = makeInteraction({ users: { '101': {} } });
    expect(parseRecipientMentions('@everyone <@101>', int, { allowMassMention: false }))
      .toMatchObject({ ids: ['101'], invalidTokens: [], massMentionDenied: true });
  });

  test('no @everyone in input → massMentionDenied=false regardless of permission', () => {
    const int = makeInteraction({ users: { '101': {} } });
    // Allowed but not used.
    expect(parseRecipientMentions('<@101>', int, { allowMassMention: true })
      .massMentionDenied).toBe(false);
    // Denied but not attempted.
    expect(parseRecipientMentions('<@101>', int, { allowMassMention: false })
      .massMentionDenied).toBe(false);
  });

  test('allowed: bot filter applies during expansion', () => {
    const int = makeInteraction({
      users: { '101': {}, '801': { bot: true }, '802': { bot: true } },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual(['101']);
  });

  test('allowed with no guild member cache returns empty (DM-context guard)', () => {
    // DM-context guild=null/undefined: expansion is impossible, but
    // the parser should NOT throw. Returns empty ids, massMentionDenied
    // stays false (the user HAD permission, the cache just had nothing).
    const int = { user: { id: '900' }, guild: undefined };
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed but every cached member is a bot → empty expansion, massMentionDenied still false', () => {
    // Distinguish "expanded to nothing" (cache has only bots) from
    // "denied" (no MENTION_EVERYONE perm). The caller renders these
    // differently: "no valid recipients" generic copy vs. the
    // permission-specific @everyone copy. Pin that the allowed-but-
    // empty path stays on the generic side.
    const int = makeInteraction({
      users: { '801': { bot: true }, '802': { bot: true } },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('explicit `<@id>` mentions take priority over @everyone expansion when cap is hit', () => {
    // Critical ordering invariant: the user explicitly named someone
    // via `<@uncached>`, then also typed `@everyone`. With cache > cap,
    // an order that ran @everyone FIRST would fill the cap with
    // arbitrary cached members and silently drop the explicit
    // `<@uncached>` (since `consider()` adds to `seen` but not `ids`
    // once `ids.size === cap`). Pin that the explicit mention always
    // claims a cap slot — @everyone fills the remainder.
    // Synthetic guild: 30 cached non-bot members at IDs `200000..200029`,
    // PLUS the explicit mentioned ID `300000000000000001` which is NOT in
    // the cache. (The bot filter only consults cache for direct mentions,
    // and a cache miss leaves the ID in per the existing contract.)
    const cachedUsers = {};
    for (let i = 0; i < 30; i++) {
      cachedUsers[`2000000000000${String(i).padStart(5, '0')}`] = {};
    }
    const explicitId = '300000000000000001';
    const int = makeInteraction({ users: cachedUsers });
    const res = parseRecipientMentions(
      `<@${explicitId}> @everyone`,
      int,
      { allowMassMention: true },
    );
    expect(res.ids.length).toBe(25);
    // The explicit mention must be in the result.
    expect(res.ids).toContain(explicitId);
    expect(res.massMentionDenied).toBe(false);
  });

  test('@everyone + <@&role> combined: dedupe across sources, cap applies', () => {
    // Both expansion sources fire. A user in the role AND in
    // `@everyone`'s cache should appear exactly once (consider+seen
    // dedupe). Cap still bounds the total at 25.
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102'] },
    });
    const res = parseRecipientMentions(
      '<@&7000> @everyone',
      int,
      { allowMassMention: true },
    );
    expect(res.ids.sort()).toEqual(['101', '102', '103']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed: cap short-circuits the cache scan (large guild cap behavior)', () => {
    // The @everyone expansion iterates guild.members.cache — a large
    // guild could have 10k+ entries. Once `ids.size === cap`, the
    // loop breaks rather than scanning the remainder. We can't easily
    // assert "break ran" directly without instrumentation, but we
    // can assert the cap-bounded result: 25 (the cap) ids out of 40
    // synthetic non-bot members, with NO cappedCount surfaced for the
    // skipped members (the early break is the tradeoff — we don't
    // count past-cap members in @everyone expansion, unlike the
    // text-mention path).
    const users = {};
    for (let i = 0; i < 40; i++) users[`u${String(i).padStart(18, '0')}`] = {};
    const int = makeInteraction({ users });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids.length).toBe(25);
    // cappedCount reflects only the members the loop actually saw
    // past `ids.size === cap`. Since we break immediately, no members
    // get added to `seen` beyond the cap, so cappedCount stays at 0.
    expect(res.cappedCount).toBe(0);
  });

  test('mixed `@everyone @here` in one input: @everyone gated separately, @here defuses', () => {
    // Both shapes hit different paths simultaneously. With
    // allowMassMention=false: @everyone surfaces massMentionDenied,
    // @here surfaces in invalidTokens via the legacy defuse. Pin
    // the two paths don't interfere.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone @here <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['@​here']);
  });

  test('repeated `@everyone @everyone` triggers expansion once (single-shot dedupe via `seen`)', () => {
    // Even though the parser sees `@everyone` twice, the expansion is
    // a single pass — consider() dedupes via `seen`. Pin that a
    // repeated mass-mention doesn't produce duplicate ids.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyone @everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('Unicode word boundary: `@everyoneé` (Unicode letter trailing) does NOT match', () => {
    // EVERYONE_TOKEN_RE uses `\p{L}\p{N}_` (Unicode-aware) instead of
    // ASCII-only `[A-Za-z0-9_]` so non-ASCII letters following
    // `@everyone` don't break the word boundary. Pin that
    // `@everyoneé` falls through to the residue path (NOT mass-mention
    // expansion). ASCII-only boundary would have over-matched here.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyoneé', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
    expect(res.invalidTokens).toEqual(['@​everyoneé']);
  });
});

describe('parseRecipientMentions — result-shape contract', () => {
  test('result shape pins exactly five keys (ids, invalidTokens, cappedCount, massMentionDenied, roleMentionsDenied)', () => {
    // Empire of `.toMatchObject` in other tests admits new fields by
    // design (the result shape is allowed to grow), but the closed
    // set of CURRENT keys is load-bearing — callers destructure these
    // names. Pin the closed set so a future PR that adds a 6th field
    // surfaces here intentionally rather than slipping past
    // partial-match assertions.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111>', int);
    expect(Object.keys(res).sort()).toEqual([
      'cappedCount',
      'ids',
      'invalidTokens',
      'massMentionDenied',
      'roleMentionsDenied',
    ]);
  });
});

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
  });

  test('custom emoji (static and animated) land in invalidTokens', () => {
    // Both `<:name:id>` (static) and `<a:name:id>` (animated) hit
    // the residue path — neither matches the USER_MENTION_RE
    // (`<@!?(\d+)>`) or ROLE_MENTION_RE (`<@&(\d+)>`) shapes.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <:smile:789>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<:smile:789>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> <a:dance:790>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<a:dance:790>'], cappedCount: 0 });
  });

  test('bare plaintext usernames land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('alice <@111> bob', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('mention with missing closer (truncated input) is treated as invalid', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111 incomplete', int);
    // The truncated `<@111` cannot match the user-mention regex
    // (requires `>`), so it falls into the residual-token bucket.
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.length).toBeGreaterThan(0);
  });

  test('@everyone (denied) surfaces massMentionDenied + does NOT defuse into invalidTokens', () => {
    // With `allowMassMention` defaulting to false, `@everyone` is
    // recognized but the caller-side gate denies expansion. The
    // parser surfaces `massMentionDenied: true` so the caller can
    // emit a permission-specific warning, and strips the token from
    // input so the residue pass doesn't double-surface it as a
    // defused invalidToken. Distinct UX from "couldn't parse."
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0, massMentionDenied: true });
  });

  test('@here still defuses with zero-width-space (no presence intent, not implemented)', () => {
    // `@here` would need GUILD_PRESENCES intent to filter online
    // members — the bot only runs GuildMembers, so @here is left
    // on the legacy defuse path (rewritten with U+200B in
    // invalidTokens). Pin the invariant so a future PR that
    // implements @here flips this test deliberately, not silently.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere'], cappedCount: 0, massMentionDenied: false });
  });

  test('@Everyone / @Here (capitalized) are NOT escaped — Discord parser is lowercase-only', () => {
    // Discord's mass-mention tokenizer is itself case-sensitive
    // (lowercase only). The parser's escape regex is `/@(everyone|here)/g`
    // — INTENTIONALLY no `/i` flag. Widening to case-insensitive
    // would needlessly mangle legitimate `@Everyone` paste artifacts.
    // Pin the invariant so a future "defensive hardening" PR can't
    // silently widen the regex without flipping this test.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@Everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Everyone'], cappedCount: 0 });
    expect(parseRecipientMentions('@Here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Here'], cappedCount: 0 });
  });

  test('@here with trailing punctuation still defuses into invalidTokens', () => {
    // @here remains on the legacy defuse path until presence intent
    // lands — pin that trailing punctuation cases still get U+200B
    // protection regardless of @everyone's separate gated path.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here: <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere:'], cappedCount: 0 });
  });

  test('@everyone with trailing punctuation (denied) still surfaces massMentionDenied + leftover lands in invalidTokens', () => {
    // `@everyone!`, `@everyone.fix`, etc. — the parser's
    // EVERYONE_TOKEN_RE matches `@everyone` with a non-word lookahead,
    // so trailing punctuation isn't consumed but the gate still fires.
    // Any leftover residue (`!`, `.fix`) is a tradeoff worth making —
    // surfacing the @everyone permission notice is the load-bearing
    // signal; the leftover punctuation gets a generic "couldn't
    // parse" treatment. Pin the residue shape so a future regression
    // that double-surfaces (`@everyone!` AS A WHOLE in invalidTokens
    // alongside the massMentionDenied flag) or silently swallows the
    // `!` is caught here.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone! <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['!']);
  });

  test('@everyone.fix (denied) leaves `.fix` in invalidTokens (parallel to `!` case)', () => {
    // `.fix` survives the strip-pass split class `[\s,;|/]+` — same
    // shape as `@everyone!`. Pin the residue so a future split-class
    // refactor that consumed `.` would surface here, not silently
    // change UX.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone.fix <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['.fix']);
  });

  test('here@everyone (embedded) stays on the defuse path (not a standalone @everyone)', () => {
    // Word-boundary semantics: `here@everyone` has `e` before the `@`,
    // so EVERYONE_TOKEN_RE's lookbehind rejects it. Falls into the
    // residue pass and gets U+200B-defused like before.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('here@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['here@\u200beveryone'], cappedCount: 0, massMentionDenied: false });
  });

  test('newline characters separate tokens (split regex includes \\n)', () => {
    // Discord slash-command option strings can contain literal
    // newlines (paste-from-multi-line). Pin that the split regex
    // `/[\s,;|/]+/` includes \n — a regression that switched to
    // `/[ \t,;|/]+/` would silently merge "alice\nbob" into one
    // bare-name token.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111>\n<#456>\n\nstray', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>', 'stray'], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — cap + length safety', () => {
  test('caps the result at QURL_SEND_MAX_RECIPIENTS + reports cappedCount', () => {
    // Build 30 user mentions; cap is 25 (set in the jest.mock above).
    // IDs are numeric to match the real Discord-mention format the
    // parser regex requires.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 30; i++) {
      const id = `${1000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(mentions.join(' '), int);
    expect(res.ids).toHaveLength(25);
    // First-N ordering (set insertion is preserved). Pin that the
    // cap takes the first 25 — keeping order stable means a user
    // can reorder their mentions to influence the kept set.
    expect(res.ids[0]).toBe('1000000000');
    expect(res.ids[24]).toBe('1000000024');
    // cappedCount lets the caller surface "I kept 25 of 30" without
    // re-parsing or re-counting input.
    expect(res.cappedCount).toBe(5);
  });

  test('truncates input above MAX_INPUT_LENGTH before scanning (ReDoS guard)', () => {
    // Build a string longer than MAX_INPUT_LENGTH with a real mention
    // at the END — the parser should NOT find it after truncation. If
    // a future regression dropped the length cap, this mention would
    // surface and the test would flip.
    const padding = 'x'.repeat(MAX_INPUT_LENGTH + 100);
    const int = makeInteraction({ users: { '999': {} } });
    const res = parseRecipientMentions(padding + ' <@999>', int);
    expect(res.ids).toEqual([]);
  });

  test('input of exactly MAX_INPUT_LENGTH is NOT truncated (boundary check)', () => {
    // Pin the inequality direction. The cap fires on `raw.length > MAX_INPUT_LENGTH`,
    // not >=, so an exactly-MAX-length input should be processed in
    // full. Build a string of exactly MAX_INPUT_LENGTH that contains
    // a real mention near the start so the rest is harmless padding.
    const mention = '<@777>';
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - mention.length);
    const input = mention + padding;
    expect(input).toHaveLength(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '777': {} } });
    expect(parseRecipientMentions(input, int).ids).toEqual(['777']);
  });

  test('cappedCount reflects POST-dedupe count, not raw mention count', () => {
    // Cap fires AFTER dedupe (`finalIds = [...ids]`). Pin the order
    // so a future refactor that swapped cap-before-dedupe would
    // surface: paste 60 mentions with heavy duplicates that
    // dedupe to 28 unique → cappedCount should be 3 (= 28 - 25),
    // NOT 35 (= 60 - 25).
    const users = {};
    const mentions = [];
    for (let i = 0; i < 28; i++) {
      const id = `${2000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    // Duplicate every mention to push raw count to 56, which still
    // dedupes to 28 unique. Then add 4 more dupes of the first to
    // push the raw count even higher without growing the unique set.
    const dupedInput = mentions.concat(mentions).concat([
      `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`,
    ]).join(' ');
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(dupedInput, int);
    expect(res.ids).toHaveLength(25);
    expect(res.cappedCount).toBe(3);  // 28 unique - 25 cap
  });

  test('cappedCount accounts for role-expansion overflow (not just direct mentions)', () => {
    // Round-5 cr regression test: a `<@&role1> <@&role2>` where role1
    // fills the cap and role2 has more usable members must NOT
    // silently truncate. Both roles' members count toward
    // `cappedCount`, even though the inner loop short-circuits Set
    // insertions past cap. Without this, callers can't tell users
    // "we kept 25 of N" when the overflow source is role expansion.
    const role1Members = [];
    const role2Members = [];
    const users = {};
    for (let i = 0; i < 50; i++) {
      const id = `${3000000000 + i}`;
      users[id] = {};
      role1Members.push(id);
    }
    for (let i = 0; i < 20; i++) {
      const id = `${4000000000 + i}`;
      users[id] = {};
      role2Members.push(id);
    }
    const int = makeInteraction({
      users,
      roles: { '7000': role1Members, '7001': role2Members },
    });
    const res = parseRecipientMentions('<@&7000> <@&7001>', int);
    expect(res.ids).toHaveLength(25);
    // 50 (role1) + 20 (role2) = 70 unique candidates; 25 kept; 45 dropped.
    expect(res.cappedCount).toBe(45);
    expect(res.invalidTokens).toEqual([]);
  });

  test('cap and invalidTokens are populated independently in the same call', () => {
    // The cap test above exercises 30 valid → 25 ids. The invalid-
    // tokens tests exercise typos in isolation. Pin the cross-cut:
    // 30 valid mentions PLUS a channel-mention typo should produce
    // 25 ids AND keep the typo in invalidTokens. A future bug where
    // the cap pass also accidentally truncated invalidTokens would
    // silently hide user-visible error feedback.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 30; i++) {
      const id = `${1000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(`${mentions.join(' ')} <#456>`, int);
    expect(res.ids).toHaveLength(25);
    expect(res.invalidTokens).toEqual(['<#456>']);
  });

  test('truncation that lands inside `<...>` drops the partial (no manufactured invalid token)', () => {
    // If the slice cuts inside an open mention like `<@123`, a naive
    // strip+split would surface `<@123` as an invalid token the user
    // didn't actually mistype. Pin that we trim back to the last `<`
    // before scanning, so the residual-token bucket stays clean.
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - 5);  // leaves room for partial mention at the end
    const input = padding + '<@123456789';  // open `<@…` with no closing `>`, will land past MAX
    expect(input.length).toBeGreaterThan(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '123456789': {} } });
    const res = parseRecipientMentions(input, int);
    expect(res.ids).toEqual([]);
    // padding is one big run of `x`s with no separators, so it
    // surfaces as one invalid token after the strip. The KEY
    // assertion is the partial-mention residue isn't ALSO there.
    expect(res.invalidTokens.every(t => !t.startsWith('<@'))).toBe(true);
  });

  test('mass-mention escape runs BEFORE per-token truncation', () => {
    // Critical ordering. If truncate ran first and the slice fell
    // before the escape pass, a token like `@everyone` near the
    // start of an oversized residue would land in invalidTokens
    // un-escaped after truncation. Pin: with `@everyone` at the
    // start of a 300-char token, the rendered output starts with
    // the ZWS-escaped form (`@\u200beveryone`) AND ends in the
    // truncation marker. A future refactor flipping to truncate-
    // then-escape would lose the ZWS and fail this test.
    const int = makeInteraction({});
    const overCap = `@everyone${'x'.repeat(300)}`;  // 309 chars total
    const res = parseRecipientMentions(overCap, int);
    expect(res.invalidTokens).toHaveLength(1);
    const rendered = res.invalidTokens[0];
    expect(rendered.endsWith('…')).toBe(true);
    expect(rendered.startsWith('@\u200beveryone')).toBe(true);  // ZWS form
    expect(rendered.startsWith('@e')).toBe(false);  // raw form absent
  });

  test('invalidTokens entries are capped at MAX_INVALID_TOKEN_LENGTH', () => {
    // A single ~4000-char garbage token (one long string with no
    // separators) would otherwise blow Discord's 4096-char embed
    // budget if the caller interpolated it verbatim. Parser caps
    // each token at MAX_INVALID_TOKEN_LENGTH chars and appends `…`.
    // Pin both branches: under-cap stays untrimmed; over-cap gets
    // the marker.
    const int = makeInteraction({});
    const longToken = 'x'.repeat(500);
    const res = parseRecipientMentions(longToken, int);
    expect(res.invalidTokens).toHaveLength(1);
    // 256 chars + the ellipsis marker.
    // length = cap + 1 (the ellipsis).
    expect(res.invalidTokens[0]).toHaveLength(MAX_INVALID_TOKEN_LENGTH + 1);
    expect(res.invalidTokens[0].endsWith('…')).toBe(true);

    // Under-cap token is unchanged.
    const shortToken = 'y'.repeat(100);
    const res2 = parseRecipientMentions(shortToken, int);
    expect(res2.invalidTokens).toEqual([shortToken]);
  });

  test('exactly-cap unique candidates: no cap fires, no log call', () => {
    // Boundary on the OTHER side of the cap. The cap fires when
    // `cappedCount > 0` (i.e. seen.size > ids.size); at exactly
    // `cap` unique inputs, ids absorbs all of them and cappedCount
    // stays 0. Pin that this branch doesn't log — the cap-overshoot
    // signal should ONLY surface when something was actually dropped.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 25; i++) {
      const id = `${7000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    const res = parseRecipientMentions(mentions.join(' '), int);
    expect(res.ids).toHaveLength(25);
    expect(res.cappedCount).toBe(0);
    expect(logger.debug).not.toHaveBeenCalled();
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('cap-overshoot logging escalates to warn past 2x the cap', () => {
    // Modest overshoot (≤ 2× cap) signals "user typed too many" and
    // stays at debug; massive overshoot signals "user pasted an
    // untrimmed list" and surfaces at warn so oncall sees the pattern.
    // Pin the threshold so a future refactor that flipped the
    // comparison or moved the multiplier silently regresses the
    // oncall signal. Also pins the meta payload shape so a future
    // field rename in the log fires this test, not the prod logs.
    const users = {};
    const mentions = [];
    // 26 unique = cap+1 = modest overshoot → debug
    for (let i = 0; i < 26; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    let int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.debug).toHaveBeenCalledTimes(1);
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('capping recipient list'),
      expect.objectContaining({ uniqueCount: 26, cap: 25, cappedCount: 1 }),
    );
    expect(logger.warn).not.toHaveBeenCalled();

    // 50 unique = exactly 2× cap (the boundary) → debug.
    // Pin the strict-greater direction: `seen.size > cap *
    // MASSIVE_OVERSHOOT_MULTIPLIER` means EXACTLY 2× stays at debug.
    // A future refactor flipping `>` to `>=` would surface here.
    for (let i = 26; i < 50; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.debug).toHaveBeenCalledTimes(1);
    expect(logger.warn).not.toHaveBeenCalled();

    // 51 unique = 2× cap + 1 = massive overshoot → warn
    for (let i = 26; i < 51; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('capping recipient list'),
      expect.objectContaining({ uniqueCount: 51, cap: 25, cappedCount: 26 }),
    );
    expect(logger.debug).not.toHaveBeenCalled();
  });
});
