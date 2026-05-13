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

jest.mock('../src/config', () => ({
  QURL_SEND_MAX_RECIPIENTS: 25,
}));

const { parseRecipientMentions, __MAX_INPUT_LENGTH } = require('../src/recipient-parser');

// Build a synthetic interaction with the cache shape the parser reads.
// Pass `users` as { id → { bot } } and `roles` as
// { id → [member_id, member_id, ...] }. Sender defaults to '900000000000000001'.
// IDs are all-numeric to match real Discord snowflakes — the parser's
// regex is `/<@!?(\d+)>/` so letter-prefixed test fixtures would silently
// drop every mention, masking real coverage.
function makeInteraction({ senderId = '900000000000000001', users = {}, roles = {} } = {}) {
  const memberCache = new Map();
  for (const [id, attrs] of Object.entries(users)) {
    memberCache.set(id, { user: { id, bot: !!attrs.bot } });
  }
  // Discord's role.members is a Collection<id, GuildMember>. We
  // simulate with a Map; the parser iterates with for-of which both
  // honor. Add the member to the cache when it's listed under a role
  // so the bot filter has something to look up.
  const roleCache = new Map();
  for (const [roleId, memberIds] of Object.entries(roles)) {
    const roleMembers = new Map();
    for (const mid of memberIds) {
      const existing = memberCache.get(mid) ?? { user: { id: mid, bot: false } };
      memberCache.set(mid, existing);
      roleMembers.set(mid, existing);
    }
    roleCache.set(roleId, { members: roleMembers });
  }
  return {
    user: { id: senderId },
    guild: {
      members: { cache: memberCache },
      roles: { cache: roleCache },
    },
  };
}

describe('parseRecipientMentions — basic shape', () => {
  test('returns empty result for null/undefined/empty input', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(null, int)).toEqual({ ids: [], invalid_tokens: [] });
    expect(parseRecipientMentions(undefined, int)).toEqual({ ids: [], invalid_tokens: [] });
    expect(parseRecipientMentions('', int)).toEqual({ ids: [], invalid_tokens: [] });
    expect(parseRecipientMentions('   \t\n', int)).toEqual({ ids: [], invalid_tokens: [] });
  });

  test('returns empty result when raw is a non-string (defense vs caller bugs)', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(42, int)).toEqual({ ids: [], invalid_tokens: [] });
    expect(parseRecipientMentions({}, int)).toEqual({ ids: [], invalid_tokens: [] });
    expect(parseRecipientMentions([], int)).toEqual({ ids: [], invalid_tokens: [] });
  });

  test('extracts a single user mention', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalid_tokens: [] });
  });

  test('accepts both <@id> and <@!id> forms (legacy nickname mention)', () => {
    const int = makeInteraction({
      users: { '111111111111111111': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@111111111111111111> <@!222222222222222222>', int))
      .toEqual({ ids: ['111111111111111111', '222222222222222222'], invalid_tokens: [] });
  });

  test('dedupes repeated mentions', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111> <@111111111111111111> <@!111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalid_tokens: [] });
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
  test('excludes the sender (no self-sends)', () => {
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001> <@222222222222222222>', int))
      .toEqual({ ids: ['222222222222222222'], invalid_tokens: [] });
  });

  test('excludes bots flagged in the member cache', () => {
    const int = makeInteraction({
      users: { '111': { bot: true }, '222': {} },
    });
    expect(parseRecipientMentions('<@111> <@222>', int))
      .toEqual({ ids: ['222'], invalid_tokens: [] });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
    // Bot filter relies on member.cache; a cache miss (e.g. cold cache
    // or member not yet fetched) cannot tell us "is this a bot." The
    // parser keeps the ID and lets the downstream send-pipeline's bot
    // check (existing form's "Cannot send to a bot" path) catch it.
    const int = makeInteraction({});  // empty cache
    expect(parseRecipientMentions('<@555>', int))
      .toEqual({ ids: ['555'], invalid_tokens: [] });
  });
});

describe('parseRecipientMentions — role mentions', () => {
  test('expands a role to its current members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102', '103'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: ['101', '102', '103'], invalid_tokens: [] });
  });

  test('merges role expansion with direct user mentions, deduped', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102'] },
    });
    expect(parseRecipientMentions('<@103> <@&7000> <@101>', int).ids.sort())
      .toEqual(['101', '102', '103']);
  });

  test('role expansion excludes sender and bots', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true }, '101': {} },
      roles: { '7000': ['900', '801', '101'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: ['101'], invalid_tokens: [] });
  });

  test('role unknown to the guild lands in invalid_tokens', () => {
    const int = makeInteraction({});
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalid_tokens: ['<@&7000>'] });
  });

  test('role with no usable members (all sender/bots) lands in invalid_tokens', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true } },
      roles: { '7000': ['900', '801'] },
    });
    const res = parseRecipientMentions('<@&7000>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalid_tokens).toEqual(['<@&7000>']);
  });

  test('DM context (guild=undefined) treats role mentions as invalid', () => {
    const int = { user: { id: '900' }, guild: undefined };
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalid_tokens: ['<@&7000>'] });
  });
});

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalid_tokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toEqual({ ids: ['111'], invalid_tokens: ['<#456>'] });
  });

  test('custom emoji land in invalid_tokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <:smile:789>', int))
      .toEqual({ ids: ['111'], invalid_tokens: ['<:smile:789>'] });
  });

  test('bare plaintext usernames land in invalid_tokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('alice <@111> bob', int))
      .toEqual({ ids: ['111'], invalid_tokens: ['alice', 'bob'] });
  });

  test('mention with missing closer (truncated input) is treated as invalid', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111 incomplete', int);
    // The truncated `<@111` cannot match the user-mention regex
    // (requires `>`), so it falls into the residual-token bucket.
    expect(res.ids).toEqual([]);
    expect(res.invalid_tokens.length).toBeGreaterThan(0);
  });
});

describe('parseRecipientMentions — cap + length safety', () => {
  test('caps the result at QURL_SEND_MAX_RECIPIENTS', () => {
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
  });

  test('truncates input above MAX_INPUT_LENGTH before scanning (ReDoS guard)', () => {
    // Build a string longer than MAX_INPUT_LENGTH with a real mention
    // at the END — the parser should NOT find it after truncation. If
    // a future regression dropped the length cap, this mention would
    // surface and the test would flip.
    const padding = 'x'.repeat(__MAX_INPUT_LENGTH + 100);
    const int = makeInteraction({ users: { '999': {} } });
    const res = parseRecipientMentions(padding + ' <@999>', int);
    expect(res.ids).toEqual([]);
  });
});
