
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

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

function makeInteraction({ senderId = '900000000000000001', users = {}, roles = {}, channels = {}, guildId } = {}) {
  const memberCache = new Map();
  for (const [id, attrs] of Object.entries(users)) {
    memberCache.set(id, { user: { id, bot: !!attrs.bot } });
  }
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
    expect(parseRecipientMentions('<@111>', {}))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0 });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
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
    const int = { user: { id: '900' }, guild: null };
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('repeated residue tokens dedupe in invalidTokens (symmetric with role-error dedup)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> alice alice bob alice', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('repeated invalid role mention dedupes in invalidTokens', () => {
    const int = makeInteraction({});  // no role 999
    expect(parseRecipientMentions('<@&999> <@&999> <@&999>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&999>'], cappedCount: 0 });
  });

  test('direct mentions always win the cap over role-expansion members', () => {
    const roleMembers = [];
    const users = {};
    const directIds = ['8000000001', '8000000002', '8000000003', '8000000004', '8000000005'];
    for (const id of directIds) users[id] = {};
    for (let i = 0; i < 50; i++) {
      const id = `${5000000000 + i}`;
      users[id] = {};
      roleMembers.push(id);
    }
    const int = makeInteraction({ users, roles: { '7000': roleMembers } });
    const directMentions = directIds.map(id => `<@${id}>`).join(' ');
    const res = parseRecipientMentions(`<@&7000> ${directMentions}`, int);
    expect(res.ids).toHaveLength(25);
    expect(res.ids).toEqual(expect.arrayContaining(directIds));
    expect(res.cappedCount).toBe(30);
  });

  test('user listed BOTH directly AND via a role mention appears once in ids, role is not flagged useless', () => {
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': ['101'] },
    });
    expect(parseRecipientMentions('<@101> <@&7000>', int))
      .toMatchObject({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
  });

  test('role mention repeated in input expands once (no double-counting)', () => {
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

  test('mentionable: false WITHOUT allowMassMention → roleMentionsDenied entry, NOT expanded', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: false });
    expect(res.ids).toEqual([]);
    expect(res.roleMentionsDenied).toEqual(['7000']);
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
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': { members: ['101'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000> <@&7000> <@&7000>', int, { allowMassMention: false });
    expect(res.roleMentionsDenied).toEqual(['7000']);
  });

  test('mix of denied + allowed roles in one input → only denied lands in roleMentionsDenied', () => {
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
    const int = makeInteraction({
      guildId: '999',
      users: { '101': {} },
      roles: { '7000': { members: ['101'], mentionable: true } },
    });
    const res = parseRecipientMentions('<@&999>', int, { allowMassMention: false });
    expect(res.massMentionDenied).toBe(true);
    expect(res.roleMentionsDenied).toEqual([]);
    expect(res.invalidTokens).toEqual([]);
  });

  test('`@everyone` text token + <@&{guildId}> wire form together → idempotent (single semantic action, no double-count)', () => {
    const allowedInt = makeInteraction({
      guildId: '999',
      users: { '101': {}, '102': {} },
    });
    const allowedRes = parseRecipientMentions('@everyone <@&999>', allowedInt, { allowMassMention: true });
    expect(allowedRes.ids.sort()).toEqual(['101', '102']);
    expect(allowedRes.massMentionDenied).toBe(false);
    expect(allowedRes.roleMentionsDenied).toEqual([]);

    const deniedInt = makeInteraction({
      guildId: '999',
      users: { '101': {}, '102': {} },
    });
    const deniedRes = parseRecipientMentions('@everyone <@&999>', deniedInt, { allowMassMention: false });
    expect(deniedRes.ids).toEqual([]);
    expect(deniedRes.massMentionDenied).toBe(true);
    expect(deniedRes.roleMentionsDenied).toEqual([]);
    expect(deniedRes.invalidTokens).toEqual([]);
  });

  test('<@&{guildId}> with allowMassMention → triggers @everyone expansion via guild.members.cache', () => {
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
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': { members: ['101', '102'], mentionable: false } },
    });
    const res = parseRecipientMentions('<@&7000>', int, { allowMassMention: false });
    expect(res.cappedCount).toBe(0);
  });
});

describe('voice-channel type constants (discord.js ChannelType pin)', () => {
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

  test('VIEW_CHANNEL_PERMISSION is 1 << 10', () => {
    expect(VIEW_CHANNEL_PERMISSION).toBe(1024n);
  });
});

describe('parseRecipientMentions — channel mentions (voice / stage-voice)', () => {
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
    const int = makeInteraction();
    int.guild.channels.cache = new Map([[
      '500',
      { id: '500', type: GUILD_VOICE, members: new Map([['111', { user: { id: '111', bot: false } }]]) },
    ]]);
    const res = parseRecipientMentions('<#500>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('text channel mention is rejected into invalidTokens (no @everyone-in-text-channel regression)', () => {
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
    const res = parseRecipientMentions('<@111> <@333> <#500>', int);
    expect(res.ids.sort()).toEqual(['111', '222', '333']);
    expect(res.invalidTokens).toEqual([]);
  });

  test('channel mention does not show up in invalidTokens twice (channel-expansion + residue-strip)', () => {
    const int = makeInteraction({
      channels: {
        '500': { type: GUILD_VOICE, members: ['111'] },
      },
    });
    const res = parseRecipientMentions('<#500>', int);
    expect(res.invalidTokens).toEqual([]);
  });

  test('DM-context (no guild) lands channel mention in invalidTokens', () => {
    const res = parseRecipientMentions('<#500>', { user: { id: '900000000000000001' } });
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<#500>']);
  });

  test('rejected channel mention is NOT double-reported by the residue-strip pass', () => {
    const int = makeInteraction({ channels: {} });
    const res = parseRecipientMentions('<#999>', int);
    expect(res.invalidTokens).toEqual(['<#999>']);
    expect(res.invalidTokens.length).toBe(1);
  });

  test('<#voice> succeeds independently when sender lacks MENTION_EVERYONE (massMentionDenied stays orthogonal)', () => {
    const int = makeInteraction({
      users: { '111': {}, '222': {} },
      channels: { '500': { type: 2, members: ['111', '222'] } },
    });
    const res = parseRecipientMentions('@everyone <#500>', int, { allowMassMention: false });
    expect(res.ids.sort()).toEqual(['111', '222']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual([]);
  });

  test('interaction.member undefined with present guild fails closed (no silent view bypass)', () => {
    const channelCache = new Map([[
      '500',
      {
        id: '500',
        type: 2, // GuildVoice
        members: new Map([['111', { user: { id: '111', bot: false } }]]),
        permissionsFor: (memberOrId) => memberOrId == null ? null : ({ has: () => true }),
      },
    ]]);
    const int = {
      user: { id: '900000000000000001' },
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
    for (const id of voiceMembers) expect(res.ids).toContain(id);
    expect(res.ids).toHaveLength(25);
  });

  test('voice channel after cap-filling user mentions: silently no-ops (NOT marked invalid)', () => {
    const users = { '111': {}, '222': {}, '333': {}, 'aaa': {}, 'bbb': {} };
    const int = makeInteraction({
      users,
      channels: { '500': { type: GUILD_VOICE, members: ['aaa', 'bbb'] } },
    });
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 3;
    try {
      const res = parseRecipientMentions('<@111> <@222> <@333> <#500>', int);
      expect(res.ids.sort()).toEqual(['111', '222', '333']);
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
    expect(parseRecipientMentions('<@101>', int, { allowMassMention: true })
      .massMentionDenied).toBe(false);
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
    const int = { user: { id: '900' }, guild: undefined };
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed but every cached member is a bot → empty expansion, massMentionDenied still false', () => {
    const int = makeInteraction({
      users: { '801': { bot: true }, '802': { bot: true } },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('explicit `<@id>` mentions take priority over @everyone expansion when cap is hit', () => {
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
    expect(res.ids).toContain(explicitId);
    expect(res.massMentionDenied).toBe(false);
  });

  test('@everyone + <@&role> combined: dedupe across sources, cap applies', () => {
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
    const users = {};
    for (let i = 0; i < 40; i++) users[`u${String(i).padStart(18, '0')}`] = {};
    const int = makeInteraction({ users });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids.length).toBe(25);
    expect(res.cappedCount).toBe(0);
  });

  test('mixed `@everyone @here` in one input: @everyone gated separately, @here defuses', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone @here <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['@​here']);
  });

  test('repeated `@everyone @everyone` triggers expansion once (single-shot dedupe via `seen`)', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyone @everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('Unicode word boundary: `@everyoneé` (Unicode letter trailing) does NOT match', () => {
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
  test('result shape pins exactly six keys (ids, invalidTokens, cappedCount, massMentionDenied, massMentionExpanded, roleMentionsDenied)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111>', int);
    expect(Object.keys(res).sort()).toEqual([
      'cappedCount',
      'ids',
      'invalidTokens',
      'massMentionDenied',
      'massMentionExpanded',
      'roleMentionsDenied',
    ]);
  });

  test('invariant: ids and roleMentionsDenied are always disjoint (gate fires before consider())', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: {
        '7000': { members: ['102', '103'], mentionable: true },   // allowed
        '7001': { members: ['101', '103'], mentionable: false },  // denied; 103 also in allowed role
      },
    });
    const res = parseRecipientMentions('<@&7000> <@&7001>', int, { allowMassMention: false });
    expect(res.roleMentionsDenied).toEqual(['7001']);
    expect(res.ids).not.toContain('101');
    const deniedExclusive = ['101'];  // 103 is in both roles
    for (const id of deniedExclusive) {
      expect(res.ids).not.toContain(id);
    }
  });
});

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
  });

  test('custom emoji (static and animated) land in invalidTokens', () => {
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
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.length).toBeGreaterThan(0);
  });

  test('@everyone (denied) surfaces massMentionDenied + does NOT defuse into invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0, massMentionDenied: true });
  });

  test('@here still defuses with zero-width-space (no presence intent, not implemented)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere'], cappedCount: 0, massMentionDenied: false });
  });

  test('@Everyone / @Here (capitalized) are NOT escaped — Discord parser is lowercase-only', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@Everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Everyone'], cappedCount: 0 });
    expect(parseRecipientMentions('@Here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Here'], cappedCount: 0 });
  });

  test('@here with trailing punctuation still defuses into invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here: <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere:'], cappedCount: 0 });
  });

  test('@everyone with trailing punctuation (denied) still surfaces massMentionDenied + leftover lands in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone! <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['!']);
  });

  test('@everyone.fix (denied) leaves `.fix` in invalidTokens (parallel to `!` case)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone.fix <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['.fix']);
  });

  test('here@everyone (embedded) stays on the defuse path (not a standalone @everyone)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('here@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['here@\u200beveryone'], cappedCount: 0, massMentionDenied: false });
  });

  test('newline characters separate tokens (split regex includes \\n)', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111>\n<#456>\n\nstray', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>', 'stray'], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — cap + length safety', () => {
  test('caps the result at QURL_SEND_MAX_RECIPIENTS + reports cappedCount', () => {
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
    expect(res.ids[0]).toBe('1000000000');
    expect(res.ids[24]).toBe('1000000024');
    expect(res.cappedCount).toBe(5);
  });

  test('truncates input above MAX_INPUT_LENGTH before scanning (ReDoS guard)', () => {
    const padding = 'x'.repeat(MAX_INPUT_LENGTH + 100);
    const int = makeInteraction({ users: { '999': {} } });
    const res = parseRecipientMentions(padding + ' <@999>', int);
    expect(res.ids).toEqual([]);
  });

  test('input of exactly MAX_INPUT_LENGTH is NOT truncated (boundary check)', () => {
    const mention = '<@777>';
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - mention.length);
    const input = mention + padding;
    expect(input).toHaveLength(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '777': {} } });
    expect(parseRecipientMentions(input, int).ids).toEqual(['777']);
  });

  test('cappedCount reflects POST-dedupe count, not raw mention count', () => {
    const users = {};
    const mentions = [];
    for (let i = 0; i < 28; i++) {
      const id = `${2000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const dupedInput = mentions.concat(mentions).concat([
      `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`,
    ]).join(' ');
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(dupedInput, int);
    expect(res.ids).toHaveLength(25);
    expect(res.cappedCount).toBe(3);  // 28 unique - 25 cap
  });

  test('cappedCount accounts for role-expansion overflow (not just direct mentions)', () => {
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
    expect(res.cappedCount).toBe(45);
    expect(res.invalidTokens).toEqual([]);
  });

  test('cap and invalidTokens are populated independently in the same call', () => {
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
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - 5);  // leaves room for partial mention at the end
    const input = padding + '<@123456789';  // open `<@…` with no closing `>`, will land past MAX
    expect(input.length).toBeGreaterThan(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '123456789': {} } });
    const res = parseRecipientMentions(input, int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.every(t => !t.startsWith('<@'))).toBe(true);
  });

  test('mass-mention escape runs BEFORE per-token truncation', () => {
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
    const int = makeInteraction({});
    const longToken = 'x'.repeat(500);
    const res = parseRecipientMentions(longToken, int);
    expect(res.invalidTokens).toHaveLength(1);
    expect(res.invalidTokens[0]).toHaveLength(MAX_INVALID_TOKEN_LENGTH + 1);
    expect(res.invalidTokens[0].endsWith('…')).toBe(true);

    const shortToken = 'y'.repeat(100);
    const res2 = parseRecipientMentions(shortToken, int);
    expect(res2.invalidTokens).toEqual([shortToken]);
  });

  test('exactly-cap unique candidates: no cap fires, no log call', () => {
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
    const users = {};
    const mentions = [];
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
