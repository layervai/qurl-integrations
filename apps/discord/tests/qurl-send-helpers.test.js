const {
  CONFIRM_CANCEL_CUSTOM_ID,
  CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
  CONFIRM_NOTE_BUTTON_CUSTOM_ID,
  CONFIRM_NOTE_MODAL_CUSTOM_ID,
  CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
  CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
  CONFIRM_SEND_CUSTOM_ID,
  CONFIRM_USER_SELECT_CUSTOM_ID,
  CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
  RECIPIENT_MODE_EVERYONE,
  RECIPIENT_MODE_PICKER,
  RECIPIENT_MODE_VOICE,
  RESOLVE_REASON,
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  SENDER_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  VALID_ATTACHMENT,
  clearCooldown,
  commands,
  detectCooldowns,
  executeSendPipeline,
  handleConfirmExpirySelect,
  handleConfirmSendClick,
  handleQurlSend,
  isOnCooldown,
  isOnDetectCooldown,
  logger,
  makeInteraction,
  makeUser,
  mockDb,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
  mockSearchPlaces,
  mockSupersedeOrCreate,
  normalizeRecipientMode,
  parseLocationInput,
  partitionRecipients,
  renderConfirmCardContent,
  renderRecipientWarnings,
  resolveLocation,
  resolveMentionableSelection,
  resolveRecipientUsers,
  resolveRoleNames,
  safeDecodeURIComponent,
  selfDestructOptionToSeconds,
  sendCooldowns,
  setCooldown,
  sweepCooldowns,
} = require('./helpers/qurl-send-fixtures');

describe('selfDestructOptionToSeconds', () => {
  test.each([
    ['none', null],
    [SELF_DESTRUCT_NO_TIMER_CHOICE, null],
    [null, null],
    [undefined, null],
    ['', null],
    ['0.5', 0.5],  // "1/2 second" preset — Math.floor would have downgraded this to "no timer"
    ['1', 1],
    ['5', 5],
    ['30', 30],
    ['300', 300],
    ['1800', 1800],
    ['3600', 3600],
    ['60', null],
    ['7200', null],
    ['999999999', null],
    ['0', null],
    ['-5', null],
    ['NaN', null],
    ['bogus', null],
    ['1.5', null],   // 1.5 is not in the preset set — no downgrade-to-floor
    ['0.25', null],  // off-preset fractional
  ])('value=%j → seconds=%j', (input, expected) => {
    expect(selfDestructOptionToSeconds(input)).toBe(expected);
  });
});

describe('partitionRecipients', () => {
  test('drops bots, keeps sender, flags selfIncluded=true', () => {
    const users = [
      makeUser('100000000000000001'),
      makeUser('100000000000000002', { bot: true }),
      makeUser(SENDER_ID),
      makeUser('100000000000000003'),
    ];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid.map((u) => u.id))
      .toEqual(['100000000000000001', SENDER_ID, '100000000000000003']);
    expect(r.droppedBots).toBe(1);
    expect(r.selfIncluded).toBe(true);
  });

  test('all bots returns valid=[]', () => {
    const users = [makeUser('100000000000000001', { bot: true }), makeUser('100000000000000002', { bot: true })];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid).toEqual([]);
    expect(r.droppedBots).toBe(2);
    expect(r.selfIncluded).toBe(false);
  });

  test('only sender is a legitimate single-recipient self-send', () => {
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual([SENDER_ID]);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(true);
  });

  test('empty input', () => {
    expect(partitionRecipients([], SENDER_ID))
      .toEqual({ valid: [], droppedBots: 0, selfIncluded: false });
  });

  test('sender NOT in input → selfIncluded=false', () => {
    const r = partitionRecipients(
      [makeUser('100000000000000001'), makeUser('100000000000000002')],
      SENDER_ID,
    );
    expect(r.selfIncluded).toBe(false);
    expect(r.valid.length).toBe(2);
  });

  test('contract: does NOT re-dedup — dedup is upstream (parseRecipientMentions Set + Discord picker gateway-event)', () => {
    const dup = makeUser('100000000000000001');
    const r = partitionRecipients([dup, dup, makeUser('100000000000000002')], SENDER_ID);
    expect(r.valid.length).toBe(3);
  });

  test('excludeSender:true drops the sender pre-validity (selfIncluded stays false)', () => {
    const users = [
      makeUser('100000000000000001'),
      makeUser(SENDER_ID),
      makeUser('100000000000000002'),
    ];
    const r = partitionRecipients(users, SENDER_ID, { excludeSender: true });
    expect(r.valid.map((u) => u.id))
      .toEqual(['100000000000000001', '100000000000000002']);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(false);
  });

  test('excludeSender:true + sender-only input → valid=[] (caller handles fallback copy)', () => {
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID, { excludeSender: true });
    expect(r.valid).toEqual([]);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(false);
  });

  test('excludeSender default (false) preserves legacy self-send behavior', () => {
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual([SENDER_ID]);
    expect(r.selfIncluded).toBe(true);
  });
});

describe('resolveMentionableSelection', () => {
  const GUILD_ID = 'guild-1';

  function makeRole({ id, members = [], mentionable = false, name }) {
    const memberMap = new Map(members.map((m) => [m.user.id, m]));
    return [id, { id, name: name ?? `role-${id}`, members: memberMap, mentionable }];
  }

  function makeMentionableInteraction({
    pickedUsers = [],
    pickedRoles = [],
    guildMemberCache = new Map(),
    inDM = false,
  } = {}) {
    const roleCache = new Map();
    for (const [id, role] of pickedRoles) {
      roleCache.set(id, role);
    }
    const guild = inDM ? null : {
      id: GUILD_ID,
      members: { cache: guildMemberCache },
      roles: { cache: roleCache },
    };
    return {
      guild,
      users: new Map(pickedUsers.map((u) => [u.id, u])),
      roles: new Map(pickedRoles),
    };
  }

  test('role fixture defaults to non-mentionable and exercises the permission gate', () => {
    const user = makeUser('100000000000000001');
    const role = makeRole({ id: 'role-private', members: [{ user }] });
    expect(role[1].mentionable).toBe(false);
    const interaction = makeMentionableInteraction({ pickedRoles: [role] });
    const result = resolveMentionableSelection({ interaction, canMentionEveryone: false });
    expect(result.users).toEqual([]);
    expect(result.roleMentionsDenied).toEqual(['role-private']);
  });

  test('users only (no roles) → returns those users verbatim, no denial', () => {
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const int = makeMentionableInteraction({ pickedUsers: [u1, u2] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id, u2.id]);
    expect(r.massMentionDenied).toBe(false);
  });

  test('filters out malformed user entries lacking .id (defense against partial User payload)', () => {
    const u1 = makeUser('100000000000000001');
    const partial = { id: undefined, username: 'partial' };
    const int = makeMentionableInteraction({ pickedUsers: [u1, partial] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
  });

  test('named role expands its members onto the user list, filters bots', () => {
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000099', { bot: true });
    const u2 = makeUser('100000000000000002');
    const int = makeMentionableInteraction({
      pickedRoles: [
        makeRole({
          mentionable: true,
          id: 'role-eng',
          members: [{ user: u1 }, { user: bot1 }, { user: u2 }],
        }),
      ],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id).sort()).toEqual([u1.id, u2.id].sort());
    expect(r.massMentionDenied).toBe(false);
  });

  test('user + role overlap dedupes (same id appears once)', () => {
    const u1 = makeUser('100000000000000001');
    const int = makeMentionableInteraction({
      pickedUsers: [u1],
      pickedRoles: [makeRole({ mentionable: true, id: 'role-eng', members: [{ user: u1 }] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
  });

  test('@everyone role (role.id === guild.id) WITHOUT MENTION_EVERYONE → denied, no expansion', () => {
    const u1 = makeUser('100000000000000001');
    const guildMembers = new Map([[u1.id, { user: u1 }]]);
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
      guildMemberCache: guildMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.massMentionDenied).toBe(true);
  });

  test('@everyone role WITH MENTION_EVERYONE → expands via guild.members.cache (NOT role.members)', () => {
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const bot1 = makeUser('100000000000000099', { bot: true });
    const guildMembers = new Map([
      [u1.id, { user: u1 }],
      [u2.id, { user: u2 }],
      [bot1.id, { user: bot1 }],
    ]);
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
      guildMemberCache: guildMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users.map((u) => u.id).sort()).toEqual([u1.id, u2.id].sort());
    expect(r.massMentionDenied).toBe(false);
  });

  test('cap short-circuits role expansion at QURL_SEND_MAX_RECIPIENTS (25) — does not over-collect 10k members', () => {
    const members = Array.from({ length: 60 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: 'role-eng', members })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.length).toBe(25);
  });

  test('cap priority: explicit user picks survive when role expansion would otherwise evict them', () => {
    const explicitIds = Array.from({ length: 5 }, (_, i) => `300000000000000${String(i).padStart(3, '0')}`);
    const explicitUsers = explicitIds.map((id) => makeUser(id));
    const cacheMembers = new Map(
      Array.from({ length: 30 }, (_, i) => {
        const id = `100000000000000${String(i).padStart(3, '0')}`;
        return [id, { user: makeUser(id) }];
      }),
    );
    const int = makeMentionableInteraction({
      pickedUsers: explicitUsers,
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
      guildMemberCache: cacheMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    const resultIds = r.users.map((u) => u.id);
    expect(resultIds.length).toBe(25);
    for (let i = 0; i < explicitUsers.length; i++) {
      expect(r.users.find((u) => u.id === explicitIds[i])).toBe(explicitUsers[i]);
    }
  });

  test('DM context (no guild) → empty users, no denial flag (no roles surface at all)', () => {
    const int = makeMentionableInteraction({ inDM: true });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.massMentionDenied).toBe(false);
  });

  test('pathological all-bot role iteration bounded at exactly 4× cap (=100, pins the multiplier)', () => {
    const config = require('../src/config');
    const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
    const bots = Array.from({ length: 300 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: 'role-bots', members: bots })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('ITER_BOUND accumulates ACROSS roles (function-scoped, not per-role)', () => {
    const config = require('../src/config');
    const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
    const botsA = Array.from({ length: 100 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    const humansB = Array.from({ length: 10 }, (_, i) => ({
      user: makeUser(`200000000000000${String(i).padStart(3, '0')}`),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [
        makeRole({ mentionable: true, id: 'role-a-bots', members: botsA }),
        makeRole({ mentionable: true, id: 'role-b-humans', members: humansB }),
      ],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('iteration cost vs count semantic: counter-before-dedupe causes overlap to consume an iter slot, blocking a later human', () => {
    const roleABots = Array.from({ length: 99 }, (_, i) => ({
      user: makeUser(`200000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    const overlapBot = roleABots[0].user; // same User ref as role A's first bot
    const human = makeUser('100000000000000001');
    const roleA = makeRole({ mentionable: true, id: 'role-a', members: roleABots });
    const roleB = makeRole({
      mentionable: true,
      id: 'role-b',
      members: [{ user: overlapBot }, { user: human }],
    });
    const int = makeMentionableInteraction({ pickedRoles: [roleA, roleB] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(99);
  });

  test('overlap dedup: same bot in two picked roles counted once in droppedFromRoles', () => {
    const bot1 = makeUser('100000000000000099', { bot: true });
    const u1 = makeUser('100000000000000001');
    const roleA = makeRole({
      mentionable: true,
      id: 'role-a',
      members: [{ user: bot1 }, { user: u1 }],
    });
    const roleB = makeRole({
      mentionable: true,
      id: 'role-b',
      members: [{ user: bot1 }],
    });
    const int = makeMentionableInteraction({
      pickedRoles: [roleA, roleB],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(1);
  });

  test('named-role overlap: directly-picked user object identity preserved (cap-priority parity with @everyone)', () => {
    const u1Picked = makeUser('100000000000000001');
    const u1FromRole = { ...makeUser('100000000000000001'), tag: 'from-role-view' };
    const role = makeRole({
      mentionable: true,
      id: 'role-eng',
      members: [{ user: u1FromRole }],
    });
    const int = makeMentionableInteraction({
      pickedUsers: [u1Picked],
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.length).toBe(1);
    expect(r.users[0]).toBe(u1Picked);
    expect(r.users[0]).not.toBe(u1FromRole);
  });

  test('directly-picked bot + same bot in role → droppedFromRoles 0 (partition reports it via droppedBots)', () => {
    const bot1 = makeUser('100000000000000099', { bot: true });
    const role = makeRole({
      mentionable: true,
      id: 'role-with-bot',
      members: [{ user: bot1 }],
    });
    const int = makeMentionableInteraction({
      pickedUsers: [bot1],
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([bot1.id]);
    expect(r.droppedFromRoles).toBe(0);
  });

  test('bot-only role pick → droppedFromRoles counts the filtered bots', () => {
    const bot1 = makeUser('100000000000000091', { bot: true });
    const bot2 = makeUser('100000000000000092', { bot: true });
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true,
        id: 'role-bots',
        members: [{ user: bot1 }, { user: bot2 }],
      })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(2);
    expect(r.massMentionDenied).toBe(false);
  });

  test('mixed role: non-bots survive, bots increment droppedFromRoles', () => {
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000091', { bot: true });
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true,
        id: 'role-eng',
        members: [{ user: u1 }, { user: bot1 }],
      })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(1);
  });

  test.each([undefined, null, {}])('named role with unavailable members is reported without inventing recipients (%s)', (members) => {
    const int = makeMentionableInteraction({ pickedRoles: [['role-cold', { id: 'role-cold', mentionable: true, members }]] });
    const result = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(result.users).toEqual([]);
    expect([...result.namedRoleMembersUnavailable]).toEqual(['role-cold']);
    expect(result.everyoneCacheCold).toBe(false);
  });

  test('mixed roles retain valid humans and distinct bot filtering while reporting unavailable members', () => {
    const human = makeUser('100000000000000001');
    const bot = { ...makeUser('100000000000000099'), bot: true };
    const int = makeMentionableInteraction({ pickedRoles: [
      ['role-cold', { id: 'role-cold', mentionable: true }],
      makeRole({ id: 'role-live', members: [{ user: human }, { user: bot }] }),
    ] });
    const result = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(result.users).toEqual([human]);
    expect(result.droppedFromRoles).toBe(1);
    expect([...result.namedRoleMembersUnavailable]).toEqual(['role-cold']);
  });

  test('everyoneCacheCold: @everyone WITH perm but missing guild.members.cache → flag set, no expansion', () => {
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
    expect(r.massMentionDenied).toBe(false);
  });

  test('everyoneCacheCold: @everyone WITH perm but EMPTY guild.members.cache → flag set (cache defined but no entries)', () => {
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
      guildMemberCache: new Map(), // defined but size === 0
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
  });

  test('everyoneCacheCold stays false when @everyone is DENIED (cache state irrelevant)', () => {
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ mentionable: true, id: GUILD_ID, members: [] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.massMentionDenied).toBe(true);
    expect(r.everyoneCacheCold).toBe(false);
  });

  test('defense: role member with undefined .user is skipped (partial GuildMember from sparse fetch)', () => {
    const u1 = makeUser('100000000000000001');
    const role = ['role-eng', {
      id: 'role-eng',
      mentionable: true,
      members: new Map([
        ['100000000000000091', { user: undefined }],
        [u1.id, { user: u1 }],
        ['100000000000000092', { /* no .user property at all */ }],
      ]),
    }];
    const int = makeMentionableInteraction({
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(0);
  });

  test('returns the documented shape: { users, massMentionDenied, droppedFromRoles, everyoneCacheCold, namedRoleMembersUnavailable, roleMentionsDenied }', () => {
    const int = makeMentionableInteraction({});
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(Object.keys(r).sort()).toEqual(['droppedFromRoles', 'everyoneCacheCold', 'massMentionDenied', 'namedRoleMembersUnavailable', 'roleMentionsDenied', 'users']);
  });

  describe('role-mention permission gate (#326)', () => {

    test('mentionable: false WITHOUT canMentionEveryone → roleMentionsDenied entry, members NOT expanded', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const role = makeRole({
        id: 'role-admin',
        members: [{ user: u1 }, { user: u2 }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users).toEqual([]);
      expect(r.roleMentionsDenied).toEqual(['role-admin']);
    });

    test('mentionable: false WITH canMentionEveryone → expands normally, no deny', () => {
      const u1 = makeUser('100000000000000001');
      const role = makeRole({
        id: 'role-admin',
        members: [{ user: u1 }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual([]);
    });

    test('mentionable: true WITHOUT canMentionEveryone → expands normally (per-role bypass)', () => {
      const u1 = makeUser('100000000000000001');
      const role = makeRole({
        id: 'role-public',
        members: [{ user: u1 }],
        mentionable: true,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual([]);
    });

    test('multiple denied roles surface independently (array, not boolean)', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const roleA = makeRole({ id: 'role-a', members: [{ user: u1 }], mentionable: false });
      const roleB = makeRole({ id: 'role-b', members: [{ user: u2 }], mentionable: false });
      const int = makeMentionableInteraction({ pickedRoles: [roleA, roleB] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.roleMentionsDenied.sort()).toEqual(['role-a', 'role-b']);
      expect(r.users).toEqual([]);
    });

    test('mix of denied + allowed roles → only denied lands in roleMentionsDenied', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const allowed = makeRole({ id: 'role-allowed', members: [{ user: u1 }], mentionable: true });
      const denied = makeRole({ id: 'role-denied', members: [{ user: u2 }], mentionable: false });
      const int = makeMentionableInteraction({ pickedRoles: [allowed, denied] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual(['role-denied']);
    });

    test('denied role does NOT increment droppedFromRoles (gate fires before bot filter)', () => {
      const bot = makeUser('100000000000000091', { bot: true });
      const role = makeRole({
        id: 'role-denied-bot',
        members: [{ user: bot }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.droppedFromRoles).toBe(0);
      expect(r.roleMentionsDenied).toEqual(['role-denied-bot']);
    });

    test('undefined role object (partial-fetch edge) → skipped, NOT routed through deny path', () => {
      const int = makeMentionableInteraction({
        pickedRoles: [['orphan-id', undefined]],
      });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users).toEqual([]);
      expect(r.roleMentionsDenied).toEqual([]);
      expect(r.massMentionDenied).toBe(false);
    });

    test('@everyone-role (role.id === guild.id) NOT routed to roleMentionsDenied — uses massMentionDenied', () => {
      const int = makeMentionableInteraction({
        pickedRoles: [[GUILD_ID, { id: GUILD_ID, members: new Map(), mentionable: false }]],
      });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.massMentionDenied).toBe(true);
      expect(r.roleMentionsDenied).toEqual([]);
    });
  });
});

describe('resolveRoleNames (#326 helper)', () => {

  function makeGuild(rolesById = {}) {
    const cache = new Map(Object.entries(rolesById));
    return { roles: { cache } };
  }

  test('returns [] for null / undefined / empty ids (defensive contract)', () => {
    const guild = makeGuild({});
    expect(resolveRoleNames(guild, null)).toEqual([]);
    expect(resolveRoleNames(guild, undefined)).toEqual([]);
    expect(resolveRoleNames(guild, [])).toEqual([]);
  });

  test('guild=null/undefined with non-empty ids → unknown-role fallback per entry (DM context shouldn\'t reach here, but optional chains carry through)', () => {
    expect(resolveRoleNames(null, ['7000'])).toEqual(['unknown-role']);
    expect(resolveRoleNames(undefined, ['7000'])).toEqual(['unknown-role']);
  });

  test('resolves cached role IDs to their names', () => {
    const guild = makeGuild({
      '7000': { id: '7000', name: 'admin' },
      '7001': { id: '7001', name: 'mods' },
    });
    expect(resolveRoleNames(guild, ['7000', '7001'])).toEqual(['admin', 'mods']);
  });

  test('cache miss → `unknown-role` fallback (deleted-mid-flow race)', () => {
    const guild = makeGuild({});  // role 7000 not in cache
    expect(resolveRoleNames(guild, ['7000'])).toEqual(['unknown-role']);
  });

  test('empty-string role name → `unknown-role` fallback (pins `||` vs `??` rationale)', () => {
    const guild = makeGuild({
      '7000': { id: '7000', name: '' },
    });
    expect(resolveRoleNames(guild, ['7000'])).toEqual(['unknown-role']);
  });

  test('mixed cache-hit / cache-miss / empty-name in one batch → fallback applies per-entry', () => {
    const guild = makeGuild({
      '7000': { id: '7000', name: 'admin' },
      '7002': { id: '7002', name: '' },
    });
    expect(resolveRoleNames(guild, ['7000', '7001', '7002'])).toEqual([
      'admin',
      'unknown-role',  // cache miss
      'unknown-role',  // empty name
    ]);
  });
});

describe('clearCooldown', () => {
  beforeEach(() => sendCooldowns.clear());

  test('removes the cooldown entry entirely', () => {
    sendCooldowns.set('u1', Date.now());
    clearCooldown('u1');
    expect(sendCooldowns.has('u1')).toBe(false);
    expect(isOnCooldown('u1')).toBe(false);
  });

  test('no-op when no cooldown is set', () => {
    clearCooldown('u1');
    expect(sendCooldowns.has('u1')).toBe(false);
  });
});

describe('setCooldown LRU iteration order', () => {
  beforeEach(() => sendCooldowns.clear());

  test('re-setting an existing key moves it to the end of iteration order', () => {
    setCooldown('uA');
    setCooldown('uB');
    setCooldown('uC');
    expect(Array.from(sendCooldowns.keys())).toEqual(['uA', 'uB', 'uC']);
    setCooldown('uA');
    expect(Array.from(sendCooldowns.keys())).toEqual(['uB', 'uC', 'uA']);
  });
});

describe('parseLocationInput', () => {
  test('Google Maps short URL passes through verbatim', () => {
    const r = parseLocationInput('https://goo.gl/maps/abc123');
    expect(r.locationUrl).toBe('https://goo.gl/maps/abc123');
    expect(r.placeId).toBeUndefined();
    expect(r.text).toBeUndefined();
  });

  test('Google Maps place URL passes through with derived name', () => {
    const r = parseLocationInput('https://www.google.com/maps/place/Eiffel+Tower/@48.85,2.29,17z');
    expect(r.locationUrl).toContain('google.com/maps/place');
    expect(r.locationName).toBeTruthy();
  });

  test('api=1&query= form extracts the name (round-trip for re-shared qURL map URLs)', () => {
    const url = 'https://www.google.com/maps/search/?api=1&query=Eiffel+Tower&query_place_id=ChIJxxx';
    const r = parseLocationInput(url);
    expect(r.locationUrl).toBe(url);
    expect(r.locationName).toBe('Eiffel Tower');
  });

  test('place_id sentinel parses into a placeId branch (no URL synthesized)', () => {
    const r = parseLocationInput('qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.placeId).toBe('ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.locationUrl).toBeNull();
    expect(r.locationName).toBeNull();
  });

  test('plain place name returns text branch for server-side resolution', () => {
    const r = parseLocationInput('Eiffel Tower, Paris');
    expect(r.locationUrl).toBeNull();
    expect(r.locationName).toBeNull();
    expect(r.text).toBe('Eiffel Tower, Paris');
  });

  test('plain non-URL text returns text branch', () => {
    const r = parseLocationInput('not a url just plain text input');
    expect(r.text).toBe('not a url just plain text input');
    expect(r.locationUrl).toBeNull();
  });

  test('https URL that does NOT match MAPS_URL_PATTERNS falls through to text branch', () => {
    const r = parseLocationInput('https://evil.example.com/maps/place/x');
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe('https://evil.example.com/maps/place/x');
  });

  test('malformed %-encoding in the input does not throw', () => {
    expect(() => parseLocationInput('https://www.google.com/maps/place/%ZZ-broken')).not.toThrow();
  });

  test('spoofed host (google.com.evil.com) fails the regex AND falls through to text branch', () => {
    const spoofed = 'https://google.com.evil.com/maps/place/Eiffel-Tower';
    const r = parseLocationInput(spoofed);
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe(spoofed);
  });
});

describe('resolveLocation', () => {
  beforeEach(() => {
    mockSearchPlaces.mockReset().mockResolvedValue([]);
    mockFindPlaceFromText.mockReset().mockResolvedValue(null);
    mockGetPlaceDetails.mockReset().mockResolvedValue(null);
  });

  test('URL branch passes through without an API call', async () => {
    const r = await resolveLocation({
      locationUrl: 'https://goo.gl/maps/abc123',
      locationName: 'My Place',
    });
    expect(r.ok).toBe(true);
    expect(r.locationUrl).toBe('https://goo.gl/maps/abc123');
    expect(r.locationName).toBe('My Place');
    expect(mockFindPlaceFromText).not.toHaveBeenCalled();
    expect(mockGetPlaceDetails).not.toHaveBeenCalled();
  });

  test('placeId branch calls getPlaceDetails and builds a place_id-pinned URL', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce({
      placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW, Washington, DC',
    });
    const r = await resolveLocation({ placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o' });
    expect(r.ok).toBe(true);
    expect(r.locationName).toBe('The White House');
    expect(r.locationUrl).toContain('query_place_id=ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(mockGetPlaceDetails).toHaveBeenCalledWith('ChIJ37FjGE63t4kRD2_jXSF1F9o');
  });

  test('text branch calls findPlaceFromText and pins to the top result', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJxxx',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
    const r = await resolveLocation({ text: 'the whitehouse' });
    expect(r.ok).toBe(true);
    expect(r.locationUrl).toContain('query_place_id=ChIJxxx');
    expect(r.locationName).toBe('The White House');
  });

  test('text branch returns not_found when Places has no candidates', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce(null);
    const r = await resolveLocation({ text: 'asdfasdfasdf' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.NOT_FOUND);
  });

  test('placeId branch returns not_found when Place Details returns null', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce(null);
    const r = await resolveLocation({ placeId: 'ChIJ-deleted-place' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.NOT_FOUND);
  });

  test('text branch returns error when the Places call throws', async () => {
    mockFindPlaceFromText.mockRejectedValueOnce(new Error('upstream timeout'));
    const r = await resolveLocation({ text: 'somewhere' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.ERROR);
  });

  test('hard-fails with no_api_key when GOOGLE_MAPS_API_KEY is unset', async () => {
    const configMock = require('../src/config');
    const orig = configMock.GOOGLE_MAPS_API_KEY;
    delete configMock.GOOGLE_MAPS_API_KEY;
    try {
      const r = await resolveLocation({ text: 'eiffel tower' });
      expect(r.ok).toBe(false);
      expect(r.reason).toBe(RESOLVE_REASON.NO_API_KEY);
    } finally {
      configMock.GOOGLE_MAPS_API_KEY = orig;
    }
  });
});

describe('safeDecodeURIComponent', () => {
  test('decodes normal percent-encoding', () => {
    expect(safeDecodeURIComponent('Hello%20World')).toBe('Hello World');
  });

  test('returns raw input on malformed encoding (does not throw)', () => {
    expect(safeDecodeURIComponent('%ZZ')).toBe('%ZZ');
    expect(safeDecodeURIComponent('%')).toBe('%');
    expect(safeDecodeURIComponent('valid%20but%incomplete')).toBe('valid%20but%incomplete');
  });

  test('handles control chars passing through', () => {
    expect(safeDecodeURIComponent('plain')).toBe('plain');
  });
});

describe('cross-command cooldown contract', () => {
  beforeEach(() => sendCooldowns.clear());

  test('setCooldown via one user blocks isOnCooldown for the same user across all entry points', () => {
    setCooldown('uA');
    expect(isOnCooldown('uA')).toBe(true);
  });

  test('clearCooldown unlocks all entry points for that user', () => {
    setCooldown('uA');
    clearCooldown('uA');
    expect(isOnCooldown('uA')).toBe(false);
  });

  test('cooldown is per-user, not global', () => {
    setCooldown('uA');
    expect(isOnCooldown('uA')).toBe(true);
    expect(isOnCooldown('uB')).toBe(false);
  });
});

describe('resolveRecipientUsers', () => {
  test('hits guild cache and skips fetch', async () => {
    const int = makeInteraction({
      guildMembers: { '100000000000000001': {}, '100000000000000002': {} },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001', '100000000000000002']);
    expect(r.users.map((u) => u.id)).toEqual(['100000000000000001', '100000000000000002']);
    expect(r.unresolvedIds).toEqual([]);
    expect(int.guild.members.fetch).not.toHaveBeenCalled();
  });

  test('falls through to fetch on cache miss', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': makeUser('100000000000000001') },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.users.map((u) => u.id)).toEqual(['100000000000000001']);
    expect(r.unresolvedIds).toEqual([]);
    expect(int.guild.members.fetch).toHaveBeenCalledWith('100000000000000001');
  });

  test('10007 unknown member → unresolved', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': 'unknown' },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.users).toEqual([]);
    expect(r.unresolvedIds).toEqual(['100000000000000001']);
  });

  test('non-10007 error → transientFailureIds (NOT unresolvedIds) + warn logged', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': 'ratelimit' },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.transientFailureIds).toEqual(['100000000000000001']);
    expect(r.unresolvedIds).toEqual([]);
    expect(logger.warn).toHaveBeenCalledWith(
      'resolveRecipientUsers: members.fetch failed (transient)',
      expect.any(Object),
    );
  });

  test('no guild → everything unresolved', async () => {
    const int = makeInteraction({ guildId: null });
    const r = await resolveRecipientUsers(int, ['100000000000000001', '100000000000000002']);
    expect(r.users).toEqual([]);
    expect(r.unresolvedIds).toEqual(['100000000000000001', '100000000000000002']);
  });

  test('mixed cache + fetch + 10007', async () => {
    const int = makeInteraction({
      guildMembers: { '100000000000000001': {} },
      guildFetchByID: {
        '100000000000000002': makeUser('100000000000000002'),
        '100000000000000003': 'unknown',
      },
    });
    const r = await resolveRecipientUsers(int, [
      '100000000000000001', '100000000000000002', '100000000000000003',
    ]);
    expect(r.users.map((u) => u.id).sort()).toEqual(['100000000000000001', '100000000000000002']);
    expect(r.unresolvedIds).toEqual(['100000000000000003']);
  });
});

describe('renderRecipientWarnings', () => {
  test('returns empty when nothing to surface', () => {
    expect(renderRecipientWarnings({
      invalidTokens: [], cappedCount: 0, unresolvedIds: [],
      droppedBots: 0,
    })).toBe('');
  });

  test('cappedCount line', () => {
    const out = renderRecipientWarnings({
      invalidTokens: [], cappedCount: 3, unresolvedIds: [],
      droppedBots: 0,
    });
    expect(out).toMatch(/Capped at 25/);
    expect(out).toMatch(/3 recipient/);
  });

  test('invalidTokens code-fenced + cap at 10', () => {
    const tokens = Array.from({ length: 15 }, (_, i) => `bogus${i}`);
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0,
    });
    expect(out).toMatch(/```/);
    expect(out).toMatch(/bogus0/);
    expect(out).toMatch(/bogus9/);
    expect(out).not.toMatch(/bogus10/);
    expect(out).toMatch(/\+5 more/);
  });

  test('invalidTokens with embedded backticks are stripped so the code-fence stays intact', () => {
    const tokens = ['```\n[evil](https://phish.example)\n```', '`code`', 'plain'];
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0,
    });
    const fenceContent = out.split('```')[1] || '';
    expect(fenceContent).not.toMatch(/`/);
    expect(out).toContain('plain');
    expect(out).toContain('[evil](https://phish.example)');
  });

  test('combines all signals', () => {
    const out = renderRecipientWarnings({
      invalidTokens: ['<#999>'], cappedCount: 2,
      unresolvedIds: ['100000000000000001'],
      droppedBots: 1,
    });
    expect(out).toMatch(/Capped/);
    expect(out).toMatch(/Could not parse/);
    expect(out).toMatch(/no longer in this server/);
    expect(out).toMatch(/bot/);
    expect(out).not.toMatch(/yourself/);
  });

  test('transientFailureIds rendered with neutral copy (not "left the server")', () => {
    const out = renderRecipientWarnings({
      transientFailureIds: ['100000000000000001', '100000000000000002'],
    });
    expect(out).toMatch(/2 user.*couldn't be looked up.*try again/);
    expect(out).not.toMatch(/no longer in this server/);
  });

  test('renderRecipientWarnings tolerates missing fields via defaults', () => {
    expect(renderRecipientWarnings({})).toBe('');
    expect(renderRecipientWarnings()).toBe('');
  });

  test('caps each shown invalidToken at 80 codepoints with an ellipsis indicator', () => {
    const longToken = 'a'.repeat(200);
    const out = renderRecipientWarnings({ invalidTokens: [longToken] });
    const fence = out.split('```')[1] || '';
    expect(fence).toMatch(/a{80}…/);
    expect(fence).not.toMatch(/a{81}/);
  });

  test('does NOT add ellipsis when the token already fits under the cap', () => {
    const out = renderRecipientWarnings({ invalidTokens: ['shorttoken'] });
    expect(out).toContain('shorttoken');
    const fence = out.split('```')[1] || '';
    expect(fence).not.toContain('…');
  });
});

describe('renderConfirmCardContent', () => {
  const baseProps = {
    resourceType: 'file',
    resourceLabel: 'report.pdf',
    validRecipients: [makeUser('100000000000000001', { username: 'alice' })],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    warningsBlock: '',
    needsPicker: false,
  };

  test('file path shows file glyph + label', () => {
    const out = renderConfirmCardContent(baseProps);
    expect(out).toMatch(/Sending file/);
    expect(out).toMatch(/report\.pdf/);
    expect(out).toMatch(/Expires/);
    expect(out).toMatch(/24 hours/);
  });

  test('map path shows map glyph + label', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      resourceType: 'maps',
      resourceLabel: 'Eiffel Tower',
    });
    expect(out).toMatch(/Sending location/);
    expect(out).toMatch(/Eiffel Tower/);
  });

  test('unknown resourceType throws — forces an explicit branch for future types', () => {
    expect(() => renderConfirmCardContent({
      ...baseProps,
      resourceType: 'audio',
    })).toThrow(/unknown resourceType.*audio/);
  });

  test('shows recipient preview (first 5) + remainder count', () => {
    const users = Array.from({ length: 7 }, (_, i) => makeUser(`10000000000000000${i + 1}`, { username: `u${i}` }));
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: users });
    expect(out).toMatch(/7 users/);
    expect(out).toMatch(/u0/);
    expect(out).toMatch(/u4/);
    expect(out).toMatch(/\+2 more/);
  });

  test('needsPicker hides recipient summary and prompts to pick', () => {
    const out = renderConfirmCardContent({ ...baseProps, needsPicker: true, validRecipients: [] });
    expect(out).toMatch(/Pick recipients/);
    expect(out).not.toMatch(/^To:/m);
  });

  test('self-destruct line shown when set', () => {
    const out = renderConfirmCardContent({ ...baseProps, selfDestructSeconds: 300 });
    expect(out).toMatch(/Self-destruct/);
  });

  test('selfIncluded=true surfaces "Send includes you." neutral notice', () => {
    const out = renderConfirmCardContent({ ...baseProps, selfIncluded: true });
    expect(out).toMatch(/Send includes you/);
    expect(out).not.toMatch(/Some recipients were dropped/);
  });

  test('selfIncluded omitted (default false) → no notice', () => {
    const out = renderConfirmCardContent(baseProps);
    expect(out).not.toMatch(/Send includes/);
  });

  test('voice-mode + selfIncluded=true → notice suppressed (forged/drifted payload defense)', () => {
    const u1 = { id: '100000000000000001', username: 'alice' };
    const out = renderConfirmCardContent({
      ...baseProps,
      validRecipients: [u1],
      selfIncluded: true,
      recipientMode: 'voice',
      voiceChannelId: 'voice-ch',
    });
    expect(out).not.toMatch(/Send includes you/);
    expect(out).toMatch(/<#voice-ch>/);
  });

  test('personal-message preview cap at 80 chars, rendered as blockquote', () => {
    const long = 'x'.repeat(120);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: long });
    expect(out).toMatch(/> x{80}…/);
    expect(out).not.toMatch(/"x{80}…"/);
  });

  test('personal-message preview backs off the cut when it would land on a markdown escape', () => {
    const safePrefix = 'a'.repeat(79);  // 79 chars
    const message = safePrefix + '\\*' + 'rest'.repeat(10);  // 79+2+40 = 121 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    expect(out).toContain(`> ${safePrefix}…`);
    expect(out).not.toMatch(/\\…/);
  });

  test('personal-message preview slices by codepoint, not UTF-16 code units (surrogate-pair safe)', () => {
    const message = 'a'.repeat(79) + '\u{1F600}' + 'rest'.repeat(20);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(out).not.toMatch(lone);
  });

  test('personal-message preview back-off handles odd-count multi-backslash boundary', () => {
    const prefix = 'a'.repeat(77);  // 77 chars
    const message = prefix + '\\\\\\*' + 'rest'.repeat(20);  // 77+4+80 = 161 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    expect(out).toContain(`> ${prefix}\\\\…`);
    expect(out).not.toMatch(/\\\\\\…/);
  });

  test('personal-message renders pre-sanitized input verbatim (no double-escape)', () => {
    const presanitized = '\\*\\*emphasis\\*\\*';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
    expect(out).not.toMatch(/\\\\\*/);
  });

  test('personal-message with mixed markdown + escape sequences renders without re-escaping the escapes', () => {
    const presanitized = '\\*\\*bold\\*\\* \\\\n \\[link\\]\\(https://evil\\)';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
    expect(out).not.toMatch(/\\\\\\\\/);
  });

  test('warningsBlock prepended', () => {
    const out = renderConfirmCardContent({ ...baseProps, warningsBlock: '⚠ warned\n\n' });
    expect(out.startsWith('⚠ warned')).toBe(true);
  });

  test('escapes markdown in recipient username', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      validRecipients: [makeUser('100000000000000001', { username: '**bob**' })],
    });
    expect(out).not.toMatch(/\*\*bob\*\*/);
    expect(out).toMatch(/\\\*\\\*bob\\\*\\\*/);
  });

  test('preview prefers guild member displayName over username when interaction is supplied', () => {
    const u = makeUser('100000000000000001', { username: 'alice' });
    const int = {
      guild: {
        members: {
          cache: new Map([[u.id, { displayName: 'Alice in Wonderland', user: u }]]),
        },
      },
    };
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: [u], interaction: int });
    expect(out).toMatch(/Alice in Wonderland/);
    expect(out).not.toMatch(/\balice\b/);
  });

  test('preview falls back to username when interaction is omitted (no guild member lookup)', () => {
    const u = makeUser('100000000000000001', { username: 'alice' });
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: [u] });
    expect(out).toMatch(/alice/);
  });

  test('personal-message blockquote collapses embedded newlines + unicode line/paragraph separators to spaces', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      personalMessage: 'first line\nsecond\r\nthird fourth fifth',
    });
    expect(out).toContain('> first line second third fourth fifth\n');
  });

  test('caps total rendered content below Discord\'s 2000-char limit + adds truncation indicator', () => {
    const bigWarnings = 'WARN ' + 'x'.repeat(3000) + '\n\n';
    const out = renderConfirmCardContent({
      ...baseProps,
      warningsBlock: bigWarnings,
    });
    expect(out.length).toBeLessThanOrEqual(2000);
    expect(out).toMatch(/…\(truncated\)$/);
  });

  test('does NOT truncate when content fits under the cap (referential-equality fast path)', () => {
    const out = renderConfirmCardContent({ ...baseProps });
    expect(out).not.toMatch(/…\(truncated\)/);
    expect(out.length).toBeLessThan(2000);
  });
});

describe('sweepCooldowns — detect window decoupled from send window (#823 round-5)', () => {
  const config = require('../src/config');
  let savedDetect;
  let savedSend;
  let nowSpy;

  beforeEach(() => {
    savedDetect = config.QURL_DETECT_COOLDOWN_MS;
    savedSend = config.QURL_SEND_COOLDOWN_MS;
    sendCooldowns.clear();
    detectCooldowns.clear();
  });
  afterEach(() => {
    config.QURL_DETECT_COOLDOWN_MS = savedDetect;
    config.QURL_SEND_COOLDOWN_MS = savedSend;
    if (nowSpy) nowSpy.mockRestore();
    sendCooldowns.clear();
    detectCooldowns.clear();
  });

  test('with DETECT(90s) > SEND(30s): a detect entry at ~31s is NOT swept (still on cooldown)', () => {
    config.QURL_SEND_COOLDOWN_MS = 30000;
    config.QURL_DETECT_COOLDOWN_MS = 90000;

    const t0 = 1_000_000_000_000;
    detectCooldowns.set('guild-1:user-1', t0);
    sendCooldowns.set('user-1', t0);

    nowSpy = jest.spyOn(Date, 'now').mockReturnValue(t0 + 31_000);

    sweepCooldowns();

    expect(detectCooldowns.has('guild-1:user-1')).toBe(true);
    expect(isOnDetectCooldown('guild-1', 'user-1')).toBe(true);
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  test('a detect entry past the DETECT window IS swept', () => {
    config.QURL_SEND_COOLDOWN_MS = 30000;
    config.QURL_DETECT_COOLDOWN_MS = 90000;
    const t0 = 1_000_000_000_000;
    detectCooldowns.set('guild-1:user-1', t0);
    nowSpy = jest.spyOn(Date, 'now').mockReturnValue(t0 + 91_000);

    sweepCooldowns();

    expect(detectCooldowns.has('guild-1:user-1')).toBe(false);
  });
});

describe('renderConfirmCardRows', () => {

  test('slash-entry WITHOUT recipients → 4 rows, Send disabled, expiry/self-destruct/note interactable', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },  // no `recipients` → needsPicker
    });
    await handleQurlSend(int);
    const editReplyCalls = int.editReply.mock.calls;
    const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
    expect(lastCall.components).toHaveLength(4);
    const sendBuilder = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_send'
    );
    expect(sendBuilder).toBeDefined();
    expect(sendBuilder.value.setDisabled).toHaveBeenCalledWith(true);
  });

  test('slash-entry WITH recipients → 4 rows (picker still attached so layout is stable across menu interactions)', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const editReplyCalls = int.editReply.mock.calls;
    const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
    expect(lastCall.components).toHaveLength(4);
  });

  test('button row carries Note + Send + Cancel in that order (3 buttons, identifiable customIds)', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(ButtonBuilder).toHaveBeenCalledTimes(3);
    const customIds = ButtonBuilder.mock.results.map(
      (r) => r.value.setCustomId.mock.calls[0][0]
    );
    expect(customIds).toEqual([
      'qurl_confirm_note_btn',
      'qurl_confirm_send',
      'qurl_confirm_cancel',
    ]);
  });

  test('Note button label is "Add a note (optional)" when no personal-message set', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const noteBtn = ButtonBuilder.mock.results[0].value;
    expect(noteBtn.setLabel).toHaveBeenCalledWith(expect.stringMatching(/Add a note/));
  });

  test('Note button label is "Edit note" when personal-message IS set', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001>',
        'personal-message': 'hello',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const noteBtn = ButtonBuilder.mock.results[0].value;
    expect(noteBtn.setLabel).toHaveBeenCalledWith(expect.stringMatching(/Edit note/));
  });

  test('slash-entry WITH recipients → picker pre-checks the text-resolved ids via addDefaultUsers', async () => {
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001> <@100000000000000002>',
      },
      guildMembers: {
        '100000000000000001': {},
        '100000000000000002': {},
      },
    });
    await handleQurlSend(int);
    expect(MentionableSelectMenuBuilder).toHaveBeenCalledTimes(1);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.addDefaultUsers).toHaveBeenCalledWith(
      '100000000000000001',
      '100000000000000002',
    );
  });

  test('slash-entry WITHOUT recipients → picker does NOT call addDefaultUsers', async () => {
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },  // no recipients → needsPicker
    });
    await handleQurlSend(int);
    expect(MentionableSelectMenuBuilder).toHaveBeenCalledTimes(1);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.addDefaultUsers).not.toHaveBeenCalled();
  });

  test('slash-entry with pre-resolved recipients pre-checks all defaults via addDefaultUsers', async () => {
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const ids = Array.from({ length: 12 }, (_, i) => `1000000000000000${String(i + 10)}`);
    const mentionList = ids.map((id) => `<@${id}>`).join(' ');
    const guildMembers = Object.fromEntries(ids.map((id) => [id, {}]));
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: mentionList },
      guildMembers,
    });
    await handleQurlSend(int);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.setMaxValues).toHaveBeenCalledWith(25);
    expect(builder.addDefaultUsers).toHaveBeenCalledWith(...ids);
  });

  test('renderConfirmCardRows pluralizes the placeholder text correctly when QURL_SEND_MAX_RECIPIENTS clamps to 1', async () => {
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(1);
      expect(builder.setPlaceholder).toHaveBeenCalledWith('Pick up to 1 user/role');
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('renderConfirmCardRows clamps maxValues to QURL_SEND_MAX_RECIPIENTS when env override is tighter than the per-pick cap', async () => {
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 15;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },  // no recipients → needsPicker
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(15);
      expect(builder.setPlaceholder).toHaveBeenCalledWith('Pick up to 15 users/roles');
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('pre-resolved defaults beyond the per-pick cap truncate to the first 25 via addDefaultUsers, but the full set persists in payload.recipientIds', async () => {
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 30;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const ids = Array.from({ length: 30 }, (_, i) => `1000000000000000${String(i + 10).padStart(2, '0')}`);
      const mentionList = ids.map((id) => `<@${id}>`).join(' ');
      const guildMembers = Object.fromEntries(ids.map((id) => [id, {}]));
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: mentionList },
        guildMembers,
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(25);
      expect(builder.addDefaultUsers).toHaveBeenCalledWith(...ids.slice(0, 25));
      const persistedPayload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(persistedPayload.recipientIds.sort()).toEqual([...ids].sort());
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('voice button label is the fixed "Everyone on voice" form, independent of channel name', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const adversarialName = '🎉'.repeat(46) + '**bold**<@123>';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000099>' },
      guildMembers: { '100000000000000099': {} },
    });
    int.channel = { id: 'voice-fixed', type: 2 };
    int.guild.channels.cache.set('voice-fixed', {
      id: 'voice-fixed', name: adversarialName, type: 2,
      members: new Map([['111', { user: { id: '111', bot: false } }]]),
    });
    await handleQurlSend(int);
    const voiceBtn = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_voice_everyone'
    );
    expect(voiceBtn).toBeDefined();
    const label = voiceBtn.value.setLabel.mock.calls[0][0];
    expect(label).toBe('\u{1F50A} Everyone on voice');
    expect(label.length).toBeLessThanOrEqual(80);
    expect(label).not.toContain('🎉');
    expect(label).not.toContain('**');
    expect(label).not.toContain('<@');
    expect(label).not.toContain('#');
    expect(label).not.toContain('…');
    expect(label).not.toMatch(/\(\d+\)/);
  });

  describe('voice-mode layout (recipientMode:"voice")', () => {

    const VOICE_CH = 'voice-ch-layout';
    const u1 = '100000000000000031';

    function setupVoice(int) {
      int.channel = { id: VOICE_CH, type: 2 };
      const member = { user: { id: u1, bot: false } };
      int.guild.members.cache.set(u1, member);
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2, name: 'general',
        members: new Map([[u1, member]]),
      });
    }

    test('picker row is NOT rendered (MentionableSelectMenuBuilder never instantiated)', async () => {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });

    test('"Pick people instead" button IS rendered; "Everyone on voice" button is NOT', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
    });

    test('bottom row has exactly 4 buttons in order: Pick-manual, Note, Send, Cancel', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toEqual([
        'qurl_confirm_pick_manual',
        'qurl_confirm_note_btn',
        'qurl_confirm_send',
        'qurl_confirm_cancel',
      ]);
    });

    test('voice-mode "To:" line uses a native channel mention (not raw channel.name) — markdown-injection safe', async () => {
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      int.channel = { id: VOICE_CH, type: 2 };
      const member = { user: { id: u1, bot: false } };
      int.guild.members.cache.set(u1, member);
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2,
        name: '**inject** _under_ ||hide||',
        members: new Map([[u1, member]]),
      });
      await handleQurlSend(int);
      const editReplyCalls = int.editReply.mock.calls;
      const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
      expect(lastCall.content).toContain(`<#${VOICE_CH}>`);
      expect(lastCall.content).not.toContain('**inject**');
      expect(lastCall.content).not.toContain('||hide||');
    });

    test('voice-mode survives an unrelated menu interaction (expiry change) without decaying to picker', async () => {
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      const payload = {
        resourceType: 'file',
        resourceLabel: 'x.png',
        recipientIds: [u1],
        recipientAliases: { [u1]: 'alice' },
        recipientMode: 'voice',
        voiceChannelId: VOICE_CH,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        warningsBlock: '',
      };
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const int = makeInteraction({ guildMembers: { [u1]: {} } });
      int.values = ['7d'];  // change expiry from 24h → 7d
      await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
      const lastCall = int.editReply.mock.calls.slice(-1)[0][0];
      expect(lastCall.content).toContain(`<#${VOICE_CH}>`);
      expect(lastCall.content).not.toMatch(/you not included/);
    });

    test('forged voice-mode without voiceChannelId still renders the escape hatch (defensive)', () => {
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const interaction = {
        guild: {
          id: 'g-forged-voice', members: { cache: new Map() }, memberCount: 1,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => false) },
      };
      const { renderConfirmCardRows } = commands._test;
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,  // ← forged/drifted state
        interaction,
        recipientIds: ['100000000000000001'],
        recipientMode: 'voice',
      });
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });
  });

  describe('everyone-mode layout (recipientMode:"everyone")', () => {
    const renderEveryoneRows = (overrides = {}) => {
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const memberCache = new Map([
        ['100000000000000001', { user: { id: '100000000000000001', bot: false } }],
        ['100000000000000002', { user: { id: '100000000000000002', bot: false } }],
      ]);
      const interaction = {
        guild: {
          id: 'g-everyone-layout',
          members: { cache: memberCache },
          memberCount: 2,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => true) },
      };
      const { renderConfirmCardRows } = commands._test;
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction,
        recipientIds: ['100000000000000001', '100000000000000002'],
        recipientMode: 'everyone',
        ...overrides,
      });
      return { MentionableSelectMenuBuilder, ButtonBuilder };
    };

    test('picker row is NOT rendered', () => {
      const { MentionableSelectMenuBuilder } = renderEveryoneRows();
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });

    test('"Pick people instead" button IS rendered', () => {
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
    });

    test('"@everyone" entry button is NOT rendered (already past the entry point)', () => {
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('"Everyone on voice" entry button is NOT rendered (even with voiceChannelId set)', () => {
      const { ButtonBuilder } = renderEveryoneRows({ voiceChannelId: 'voice-ch-1' });
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
    });

    test('bottom row has exactly 4 buttons in order: Pick-manual, Note, Send, Cancel', () => {
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toEqual([
        'qurl_confirm_pick_manual',
        'qurl_confirm_note_btn',
        'qurl_confirm_send',
        'qurl_confirm_cancel',
      ]);
    });
  });

  describe('@everyone button', () => {
    test('renders when sender has MENTION_EVERYONE in guild (picker mode)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).toContain('qurl_confirm_everyone');
    });

    test('does NOT render without MENTION_EVERYONE', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('label is the fixed "📢 @everyone" form — no live count, no overcap suffix', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: {
          '100000000000000001': {},
          '100000000000000002': {},
          '100000000000000099': { bot: true },
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith('\u{1F4E2} @everyone');
      const label = everyoneBtn.value.setLabel.mock.calls[0][0];
      expect(label).not.toMatch(/\(\d+\)/);
      expect(label).not.toMatch(/exceeds/);
      expect(label).not.toMatch(/\(\?\)/);
    });

    test('disabled when memberCount unavailable AND cache cold (displayCount null)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      delete int.guild.memberCount;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(true);
    });

    test('emits warn-log with branch=displayCount-null when memberCount is unavailable', async () => {
      const logger = require('../src/logger');
      logger.warn.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guildId = 'g-diag-null';
      delete int.guild.memberCount;
      await handleQurlSend(int);
      expect(logger.warn).toHaveBeenCalledWith(
        '@everyone button rendered disabled',
        expect.objectContaining({
          branch: 'displayCount-null',
          guildId: 'g-diag-null',
          guildMemberCount: null,
          displayCount: null,
        }),
      );
    });

    test('emits warn-log with branch=over-cap when warm cache exceeds the recipient cap', async () => {
      const config = require('../src/config');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
        const logger = require('../src/logger');
        logger.warn.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: {
            '100000000000000001': {},
            '100000000000000002': {},
            '100000000000000003': {},
          },
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guildId = 'g-diag-overcap';
        int.guild.memberCount = 3;
        await handleQurlSend(int);
        expect(logger.warn).toHaveBeenCalledWith(
          '@everyone button rendered disabled',
          expect.objectContaining({
            branch: 'over-cap',
            guildId: 'g-diag-overcap',
            displayCount: 3,
            accurate: true,
            cap: 2,
          }),
        );
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('does NOT emit warn-log when @everyone button is enabled (cold-cache happy path)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const logger = require('../src/logger');
      logger.warn.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(false);
      expect(logger.warn).not.toHaveBeenCalledWith(
        '@everyone button rendered disabled',
        expect.anything(),
      );
    });

    test('does NOT render in DM context — direct renderer assertion', () => {
      const { ButtonBuilder } = require('discord.js');
      const commands = require('../src/commands');
      const { renderConfirmCardRows } = commands._test;
      ButtonBuilder.mockClear();
      const dmInteraction = {
        guild: null,
        memberPermissions: { has: jest.fn(() => true) },
      };
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction: dmInteraction,
        recipientIds: [],
        recipientMode: 'picker',
      });
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('non-bot count is memoized across re-renders with stable cache', () => {
      const commands = require('../src/commands');
      const { renderConfirmCardRows, _everyoneCountMemo } = commands._test;
      const guildId = 'guild-memo-iter';
      const memberCache = new Map([
        ['u1', { user: { id: 'u1', bot: false } }],
        ['u2', { user: { id: 'u2', bot: false } }],
        ['b1', { user: { id: 'b1', bot: true } }],
      ]);
      let iterations = 0;
      const iterCountingCache = new Proxy(memberCache, {
        get(target, prop) {
          if (prop === Symbol.iterator) {
            iterations++;
            return target[Symbol.iterator].bind(target);
          }
          return Reflect.get(target, prop);
        },
      });
      const guild = {
        id: guildId,
        members: { cache: iterCountingCache },
        memberCount: 3,
        channels: { cache: new Map() },
      };
      _everyoneCountMemo.delete(guild);
      const interaction = {
        guild,
        memberPermissions: { has: jest.fn(() => true) },
      };
      const args = {
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction,
        recipientIds: [],
        recipientMode: 'picker',
      };
      for (let i = 0; i < 5; i++) renderConfirmCardRows(args);
      expect(iterations).toBe(1);
    });

    test('memo busts on cache.size change', () => {
      const commands = require('../src/commands');
      const { renderConfirmCardRows, _everyoneCountMemo } = commands._test;
      const memberCache = new Map([
        ['u1', { user: { id: 'u1', bot: false } }],
        ['u2', { user: { id: 'u2', bot: false } }],
      ]);
      let iterations = 0;
      const iterCountingCache = new Proxy(memberCache, {
        get(target, prop) {
          if (prop === Symbol.iterator) {
            iterations++;
            return target[Symbol.iterator].bind(target);
          }
          return Reflect.get(target, prop);
        },
      });
      const guild = {
        id: 'guild-memo-bust',
        members: { cache: iterCountingCache },
        memberCount: 2,
        channels: { cache: new Map() },
      };
      _everyoneCountMemo.delete(guild);
      const args = {
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction: { guild, memberPermissions: { has: jest.fn(() => true) } },
        recipientIds: [],
        recipientMode: 'picker',
      };
      renderConfirmCardRows(args);  // memo populated, 1 iteration
      expect(iterations).toBe(1);
      memberCache.set('u3', { user: { id: 'u3', bot: false } });
      guild.memberCount = 3;
      renderConfirmCardRows(args);
      expect(iterations).toBe(2);
    });

    test('disabled when warm-cache non-bot count > cap (no overcap suffix in label)', async () => {
      const config = require('../src/config');
      const { ButtonBuilder } = require('discord.js');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
        ButtonBuilder.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: {  // 4 cached, all non-bot → count=4 > cap=2
            '100000000000000001': {},
            '100000000000000002': {},
            '100000000000000003': {},
            '100000000000000004': {},
          },
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guild.memberCount = 4;  // matches cache.size → warm + accurate
        await handleQurlSend(int);
        const everyoneBtn = ButtonBuilder.mock.results.find(
          (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
        );
        expect(everyoneBtn).toBeDefined();
        expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith('\u{1F4E2} @everyone');
        expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(true);
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('cold-cache memberCount > cap does NOT disable (avoid bot-overcount false-positive)', async () => {
      const config = require('../src/config');
      const { ButtonBuilder } = require('discord.js');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 100, configurable: true, writable: true });
        ButtonBuilder.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: { '100000000000000001': {} },  // cache.size=1
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guild.memberCount = 500;  // cache.size < memberCount → cold
        await handleQurlSend(int);
        const everyoneBtn = ButtonBuilder.mock.results.find(
          (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
        );
        expect(everyoneBtn).toBeDefined();
        expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith(expect.not.stringContaining('exceeds'));
        expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(false);
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('both @everyone AND voice-everyone buttons render together when invoked from voice + MENTION_EVERYONE', async () => {
      const { ButtonBuilder, ChannelType } = require('discord.js');
      ButtonBuilder.mockClear();
      const voiceChannelId = 'voice-room-1';
      const int = makeInteraction({
        channelId: voiceChannelId,
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      int.channel = {
        id: voiceChannelId, name: 'general', type: ChannelType.GuildVoice,
        members: new Map([['100000000000000001', { user: makeUser('100000000000000001') }]]),
      };
      int.guild.channels.cache.set(voiceChannelId, int.channel);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).toContain('qurl_confirm_voice_everyone');
      expect(customIds).toContain('qurl_confirm_everyone');
      expect(customIds).toContain('qurl_confirm_note_btn');
      expect(customIds).toContain('qurl_confirm_send');
      expect(customIds).toContain('qurl_confirm_cancel');
      expect(customIds.length).toBe(5);
    });

    test('does NOT render in voice-mode (recipientMode === RECIPIENT_MODE_VOICE)', () => {
      const { ButtonBuilder } = require('discord.js');
      const { renderConfirmCardRows } = commands._test;
      ButtonBuilder.mockClear();
      const memberCache = new Map([['100000000000000001', { user: makeUser('100000000000000001') }]]);
      const interaction = {
        guild: {
          id: 'g-voice', members: { cache: memberCache }, memberCount: 5,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => true) },
      };
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: 'voice-ch-1',
        interaction,
        recipientIds: ['100000000000000001'],
        recipientMode: 'voice',
      });
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
      expect(customIds).toContain('qurl_confirm_pick_manual');
    });
  });
});

describe('computeEveryoneDisplayCount', () => {
  const { computeEveryoneDisplayCount, _everyoneCountMemo } = commands._test;

  test('warm cache (cache.size === memberCount) → accurate non-bot count', () => {
    const guild = {
      id: 'g-warm',
      memberCount: 3,
      members: {
        cache: new Map([
          ['u1', { user: { id: 'u1', bot: false } }],
          ['u2', { user: { id: 'u2', bot: false } }],
          ['b1', { user: { id: 'b1', bot: true } }],
        ]),
      },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 2, accurate: true });
  });

  test('cold cache (cache.size < memberCount) → memberCount fallback, NOT accurate', () => {
    const guild = {
      id: 'g-cold',
      memberCount: 50,
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 50, accurate: false });
  });

  test('memberCount undefined + warm-shape cache → cold fallback returns {count: null, accurate: false}', () => {
    const guild = {
      id: 'g-no-mc',
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: null, accurate: false });
  });

  test('cache missing → memberCount fallback when present', () => {
    const guild = { id: 'g-no-cache', memberCount: 7, members: undefined };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 7, accurate: false });
  });

  test('cache and memberCount both missing → null', () => {
    const guild = { id: 'g-bare', members: undefined };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: null, accurate: false });
  });

  test('http-tier shape (memberCount undefined + approximateMemberCount set) → falls back to approximate count', () => {
    const guild = {
      id: 'g-http-tier',
      approximateMemberCount: 42,
      members: { cache: new Map([['bot', { user: { id: 'bot', bot: true } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 42, accurate: false });
  });

  test('memberCount wins over approximateMemberCount when both are set (gateway mode preserves precedence)', () => {
    const guild = {
      id: 'g-both',
      memberCount: 10,            // live (from gateway)
      approximateMemberCount: 9,  // stale (from earlier REST fetch)
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 10, accurate: false });
  });

  test('partial-cache row (no .user) does not inflate count', () => {
    const guild = {
      id: 'g-partial',
      memberCount: 2,
      members: {
        cache: new Map([
          ['u1', { user: { id: 'u1', bot: false } }],
          ['degraded', { /* no .user */ }],
        ]),
      },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 1, accurate: true });
  });
});

describe('largeSendThreshold', () => {
  const { largeSendThreshold, LARGE_SEND_RECIPIENT_FLOOR } = commands._test;
  const config = require('../src/config');

  test('default cap (20k): floor wins (1000)', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 20000;
    try {
      expect(largeSendThreshold()).toBe(LARGE_SEND_RECIPIENT_FLOOR);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('small override (cap=500): half-cap wins (250)', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 500;
    try {
      expect(largeSendThreshold()).toBe(250);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('degenerate override (cap=1): floors at 1 (NOT 0 — would fire every send)', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      expect(largeSendThreshold()).toBe(1);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('boundary (cap=2): floor(2/2)=1 wins WITHOUT the substitution — pins discontinuity', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 2;
    try {
      expect(largeSendThreshold()).toBe(1);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('cap exactly at floor (1000): half-cap (500) wins', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1000;
    try {
      expect(largeSendThreshold()).toBe(500);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });
});

describe('constants + exports', () => {
  test('customIds match the wire-protocol values Discord routes against', () => {
    expect(CONFIRM_USER_SELECT_CUSTOM_ID).toBe('qurl_confirm_user_select');
    expect(CONFIRM_SEND_CUSTOM_ID).toBe('qurl_confirm_send');
    expect(CONFIRM_CANCEL_CUSTOM_ID).toBe('qurl_confirm_cancel');
    expect(CONFIRM_EXPIRY_SELECT_CUSTOM_ID).toBe('qurl_confirm_expiry');
    expect(CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID).toBe('qurl_confirm_self_destruct');
    expect(CONFIRM_NOTE_BUTTON_CUSTOM_ID).toBe('qurl_confirm_note_btn');
    expect(CONFIRM_NOTE_MODAL_CUSTOM_ID).toBe('qurl_confirm_note_modal');
    expect(CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID).toBe('qurl_confirm_voice_everyone');
    expect(CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID).toBe('qurl_confirm_pick_manual');
  });

  test('all customIds unique', () => {
    const ids = new Set([
      CONFIRM_USER_SELECT_CUSTOM_ID,
      CONFIRM_SEND_CUSTOM_ID,
      CONFIRM_CANCEL_CUSTOM_ID,
      CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
      CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
      CONFIRM_NOTE_BUTTON_CUSTOM_ID,
      CONFIRM_NOTE_MODAL_CUSTOM_ID,
      CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
      CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
    ]);
    expect(ids.size).toBe(9);
  });

  test('recipientMode tokens are stable wire values (persisted in flow_state rows)', () => {
    expect(RECIPIENT_MODE_PICKER).toBe('picker');
    expect(RECIPIENT_MODE_VOICE).toBe('voice');
    expect(RECIPIENT_MODE_EVERYONE).toBe('everyone');
  });

  test('normalizeRecipientMode: closed set {voice, everyone, picker}; everything else picker', () => {
    expect(normalizeRecipientMode('voice')).toBe('voice');
    expect(normalizeRecipientMode('everyone')).toBe('everyone');
    expect(normalizeRecipientMode('picker')).toBe('picker');
    expect(normalizeRecipientMode(undefined)).toBe('picker');
    expect(normalizeRecipientMode(null)).toBe('picker');
    expect(normalizeRecipientMode('')).toBe('picker');
    expect(normalizeRecipientMode('VOICE')).toBe('picker'); // case-sensitive
    expect(normalizeRecipientMode('EVERYONE')).toBe('picker'); // case-sensitive
    expect(normalizeRecipientMode('unknown')).toBe('picker');
  });

  test('siblingMessage is keyed by stage so any of the three confirm-card customIds surfaces the same message', () => {
    const { siblingMessageForStage } = require('../src/flow-dispatch');
    const msg = siblingMessageForStage(SEND_STAGE_AWAITING_CONFIRM);
    expect(msg).toMatch(/qurl send.*qurl map.*confirm card/i);
    expect(msg).toBeTruthy();
  });

  test('all four new confirm-card menu customIds are registered (duplicate-register throws)', () => {
    const { registerFlow } = require('../src/flow-dispatch');
    const newCustomIds = [
      'qurl_confirm_expiry',
      'qurl_confirm_self_destruct',
      'qurl_confirm_note_btn',
      'qurl_confirm_note_modal',
    ];
    for (const id of newCustomIds) {
      expect(() => registerFlow(id, {
        expectedStage: 'noop_stage_for_collision_check',
        handler: () => undefined,
      })).toThrow(/already registered/);
    }
  });

  test('voice-everyone + pick-manual buttons are registered at the confirm-card stage', () => {
    const { registerFlow } = require('../src/flow-dispatch');
    for (const id of ['qurl_confirm_voice_everyone', 'qurl_confirm_pick_manual']) {
      expect(() => registerFlow(id, {
        expectedStage: 'noop_stage_for_collision_check',
        handler: () => undefined,
      })).toThrow(/already registered/);
    }
  });

  test('executeSendPipeline still exported (back-half hook)', () => {
    expect(typeof executeSendPipeline).toBe('function');
  });

  test('CONTRACT: executeSendPipeline never reads personalMessageRaw', () => {
    const fs = require('fs');
    const path = require('path');
    const src = fs.readFileSync(path.resolve(__dirname, '../src/commands.js'), 'utf8');
    const startMarker = 'async function executeSendPipeline(';
    const startIdx = src.indexOf(startMarker);
    expect(startIdx).toBeGreaterThanOrEqual(0);
    let i = src.indexOf('{', startIdx);
    expect(i).toBeGreaterThanOrEqual(0);
    let depth = 0;
    let end = -1;
    for (; i < src.length; i++) {
      if (src[i] === '{') depth++;
      else if (src[i] === '}') {
        depth--;
        if (depth === 0) { end = i; break; }
      }
    }
    expect(end).toBeGreaterThan(startIdx);
    const fnBody = src.slice(startIdx, end + 1);
    expect(fnBody).not.toContain('personalMessageRaw');
  });

  test('CONTRACT (runtime): handleConfirmSendClick never reads payload.personalMessageRaw on the Send path', async () => {
    const u1 = '100000000000000001';
    const basePayload = {
      resourceType: 'file',
      attachment: VALID_ATTACHMENT,
      locationUrl: null,
      locationName: null,
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: 'Alice' },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: 'safe content',
      personalMessageRaw: '**FORBIDDEN_RAW**',
      warningsBlock: '',
      sendNonce: 'nonce-contract',
    };
    let leaked = false;
    const trappedPayload = new Proxy(basePayload, {
      get(target, prop) {
        if (prop === 'personalMessageRaw') {
          leaked = true;
          throw new Error('CONTRACT VIOLATION: handleConfirmSendClick read payload.personalMessageRaw');
        }
        return target[prop];
      },
    });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: trappedPayload, version: 1 } });
    expect(leaked).toBe(false);
  });

  test('CONTRACT (runtime, pipeline-direct): executeSendPipeline never reads personalMessageRaw from its params', async () => {
    const u1 = '100000000000000001';
    const validParams = {
      apiKey: 'apikey-1',
      resourceType: 'file',
      attachment: VALID_ATTACHMENT,
      locationUrl: null,
      locationName: null,
      recipients: [{ id: u1, username: 'Alice', bot: false }],
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: 'safe content',
      personalMessageRaw: '**FORBIDDEN_RAW**',
      sendNonce: 'nonce-pipeline-contract',
    };
    let leaked = false;
    const trappedParams = new Proxy(validParams, {
      get(target, prop) {
        if (prop === 'personalMessageRaw') {
          leaked = true;
          throw new Error('CONTRACT VIOLATION: executeSendPipeline read params.personalMessageRaw');
        }
        return target[prop];
      },
    });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    try {
      await executeSendPipeline(int, trappedParams);
    } catch (err) {
      if (leaked) throw err;
    }
    expect(leaked).toBe(false);
  });
});
