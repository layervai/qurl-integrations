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
// src/config.js:279). Tests pin the cap behavior at 25 so the
// boundary cases stay small + readable.
jest.mock('../src/config', () => ({
  QURL_SEND_MAX_RECIPIENTS: 25,
}));

const { parseRecipientMentions, MAX_INPUT_LENGTH } = require('../src/recipient-parser');

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
    expect(parseRecipientMentions(null, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions(undefined, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('', int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('   \t\n', int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('returns empty result when raw is a non-string (defense vs caller bugs)', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(42, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions({}, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions([], int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('extracts a single user mention', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('accepts both <@id> and <@!id> forms (legacy nickname mention)', () => {
    const int = makeInteraction({
      users: { '111111111111111111': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@111111111111111111> <@!222222222222222222>', int))
      .toEqual({ ids: ['111111111111111111', '222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('dedupes repeated mentions', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111> <@111111111111111111> <@!111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
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
      .toEqual({ ids: ['222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('excludes bots flagged in the member cache', () => {
    const int = makeInteraction({
      users: { '111': { bot: true }, '222': {} },
    });
    expect(parseRecipientMentions('<@111> <@222>', int))
      .toEqual({ ids: ['222'], invalidTokens: [], cappedCount: 0 });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
    // Bot filter relies on member.cache; a cache miss (e.g. cold cache
    // or member not yet fetched) cannot tell us "is this a bot." The
    // parser keeps the ID and lets the downstream send-pipeline's bot
    // check (existing form's "Cannot send to a bot" path) catch it.
    const int = makeInteraction({});  // empty cache
    expect(parseRecipientMentions('<@555>', int))
      .toEqual({ ids: ['555'], invalidTokens: [], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — role mentions', () => {
  test('expands a role to its current members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102', '103'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: ['101', '102', '103'], invalidTokens: [], cappedCount: 0 });
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
      .toEqual({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
  });

  test('role unknown to the guild lands in invalidTokens', () => {
    const int = makeInteraction({});
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('role with no usable members (all sender/bots) lands in invalidTokens', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true } },
      roles: { '7000': ['900', '801'] },
    });
    const res = parseRecipientMentions('<@&7000>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<@&7000>']);
  });

  test('DM context (guild=undefined) treats role mentions as invalid', () => {
    const int = { user: { id: '900' }, guild: undefined };
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('user listed BOTH directly AND via a role mention appears once in ids', () => {
    // The "merges role expansion with direct user mentions" test
    // above exercises this implicitly (user 101 is in role 7000 AND
    // mentioned directly), but a fixture that EXPLICITLY overlaps
    // makes the dedupe invariant non-incidental — a regression that
    // re-added role members without the Set guard would surface
    // here as ids.length === 2 instead of 1.
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': ['101'] },
    });
    expect(parseRecipientMentions('<@101> <@&7000>', int))
      .toEqual({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
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

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
  });

  test('custom emoji land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <:smile:789>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<:smile:789>'], cappedCount: 0 });
  });

  test('bare plaintext usernames land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('alice <@111> bob', int))
      .toEqual({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('mention with missing closer (truncated input) is treated as invalid', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111 incomplete', int);
    // The truncated `<@111` cannot match the user-mention regex
    // (requires `>`), so it falls into the residual-token bucket.
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.length).toBeGreaterThan(0);
  });

  test('@everyone / @here are pre-escaped with zero-width-space in invalidTokens', () => {
    // Discord's @everyone / @here are NOT `<@id>` mentions — they're
    // bare tokens that the API expands at message-send time. A caller
    // naively interpolating `invalidTokens` into a user-visible
    // message (`` `Couldn't parse: ${invalidTokens.join(', ')}` ``)
    // would fan-out-ping the channel. To make the boundary safe by
    // default, the parser inserts a zero-width space (U+200B) after
    // the `@` — visually identical, but Discord's tokenizer no longer
    // recognizes the mass-mention shape.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200beveryone'], cappedCount: 0 });
    expect(parseRecipientMentions('@here <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200bhere'], cappedCount: 0 });
  });

  test('newline characters separate tokens (split regex includes \\n)', () => {
    // Discord slash-command option strings can contain literal
    // newlines (paste-from-multi-line). Pin that the split regex
    // `/[\s,]+/` includes \n — a regression that switched to
    // `/[ \t,]+/` would silently merge "alice\nbob" into one
    // bare-name token.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111>\n<#456>\n\nstray', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<#456>', 'stray'], cappedCount: 0 });
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
});
