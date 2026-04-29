/**
 * Unit tests for src/store/ddb-store.js.
 *
 * Uses `aws-sdk-client-mock` to intercept DocumentClient commands
 * without hitting AWS. Covers each Store contract method with:
 *   - A happy-path case verifying the right DDB command is issued
 *     with the expected arguments.
 *   - Edge cases for methods with non-trivial logic (dedup
 *     conditional failures, legacy branches, pagination).
 *
 * Does NOT cover:
 *   - Real DDB behavior (conditional expressions, GSI consistency).
 *     That's integration-test territory (PR 4b follow-up will run
 *     against a sandbox DDB table).
 *   - Timing-dependent operations (TTL actually expiring rows) —
 *     those are DDB-side guarantees, not our code.
 */

jest.mock('../src/config', () => ({
  PENDING_LINK_EXPIRY_MINUTES: 10,
  DATABASE_PATH: ':memory:',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),}));

// Mock crypto wrapper: pass-through so tests can assert on
// plaintext that flows into the DDB Item. Real encryption is
// exercised by crypto.test.js.
jest.mock('../src/utils/crypto', () => ({
  encrypt: (v) => `enc:v1:IV:TAG:${Buffer.from(v || '').toString('hex')}`,
  decrypt: (v) => {
    if (!v || !v.startsWith('enc:v1:')) return v;
    const parts = v.split(':');
    return Buffer.from(parts[4], 'hex').toString();
  },
}));

const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  GetCommand,
  PutCommand,
  DeleteCommand,
  UpdateCommand,
  QueryCommand,
  ScanCommand,
  BatchWriteCommand,
} = require('@aws-sdk/lib-dynamodb');

// aws-sdk-client-mock intercepts DocumentClient commands globally.
// The DDB store module creates its client at module-load time, so
// the mock must be set up before requiring the store.
const ddbMock = mockClient(DynamoDBDocumentClient);

process.env.DDB_TABLE_PREFIX = 'test-prefix-';
process.env.AWS_REGION = 'us-east-2';

const store = require('../src/store/ddb-store');

beforeEach(() => {
  ddbMock.reset();
});

afterAll(async () => {
  await store.close();
});

// ── Pending links ──

describe('pending links', () => {
  test('createPendingLink: PutItem with discord_id + expires_at TTL', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.createPendingLink('state-abc', 'disc-1');
    const calls = ddbMock.commandCalls(PutCommand);
    expect(calls).toHaveLength(1);
    expect(calls[0].args[0].input.TableName).toBe('test-prefix-pending-links');
    expect(calls[0].args[0].input.Item.state).toBe('state-abc');
    expect(calls[0].args[0].input.Item.discord_id).toBe('disc-1');
    // Number type, epoch seconds, ~10 min out. Upper bound catches
    // the unit-confusion bug where someone writes ms instead of
    // seconds — without the cap that bug passes the lower bound
    // (ms-since-epoch is "much greater than" seconds-since-epoch)
    // but DDB silently ignores TTL values too far in the future.
    const ttl = calls[0].args[0].input.Item.expires_at;
    expect(typeof ttl).toBe('number');
    const nowSec = Math.floor(Date.now() / 1000);
    expect(ttl).toBeGreaterThan(nowSec);
    expect(ttl).toBeLessThan(nowSec + 11 * 60); // PENDING_LINK_EXPIRY_MINUTES = 10 + 1m slack
  });

  test('getPendingLink: GetItem by state, returns shape with discord_id', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { state: 's', discord_id: 'd', created_at: 'now' } });
    const result = await store.getPendingLink('s');
    expect(result).toEqual({ discord_id: 'd' });
  });

  test('getPendingLink: returns undefined when not found', async () => {
    ddbMock.on(GetCommand).resolves({});
    const result = await store.getPendingLink('nope');
    expect(result).toBeUndefined();
  });

  test('consumePendingLink: DeleteItem with ReturnValues ALL_OLD', async () => {
    ddbMock.on(DeleteCommand).resolves({ Attributes: { state: 's', discord_id: 'd' } });
    const result = await store.consumePendingLink('s');
    expect(result).toEqual({ discord_id: 'd' });
    expect(ddbMock.commandCalls(DeleteCommand)[0].args[0].input.ReturnValues).toBe('ALL_OLD');
  });

  test('consumePendingLink: returns undefined when state already deleted (concurrent caller race)', async () => {
    // Two callers race on the same state; the second sees no
    // Attributes back. Caller in routes/oauth.js treats undefined
    // as "this state was already consumed" — the OAuth callback
    // then fails with the standard "invalid state" error rather
    // than crashing on a missing field.
    ddbMock.on(DeleteCommand).resolves({}); // no Attributes
    const result = await store.consumePendingLink('already-gone');
    expect(result).toBeUndefined();
  });
});

// ── GitHub links ──

describe('github links', () => {
  test('createLink: lowercases github_username, sets updated_at, preserves linked_at on re-link', async () => {
    // Now uses UpdateCommand with if_not_exists(linked_at, :now) so
    // the first link sets linked_at; re-links only touch
    // github_username + updated_at. Test by inspecting the
    // UpdateExpression shape.
    ddbMock.on(UpdateCommand).resolves({});
    await store.createLink('disc-1', 'OctoCat');
    const input = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(input.Key).toEqual({ discord_id: 'disc-1' });
    expect(input.ExpressionAttributeValues[':g']).toBe('octocat');
    expect(input.ExpressionAttributeValues[':now']).toBeDefined();
    // Critical: `if_not_exists(linked_at, :now)` — preserves SQLite's
    // ON CONFLICT behavior across re-links.
    expect(input.UpdateExpression).toMatch(/if_not_exists\(linked_at, :now\)/);
    expect(input.UpdateExpression).toMatch(/github_username = :g/);
    expect(input.UpdateExpression).toMatch(/updated_at = :now/);
  });

  test('getLinkByGithub: queries GSI then hops to base table', async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ discord_id: 'd' }] });
    ddbMock.on(GetCommand).resolves({ Item: { discord_id: 'd', github_username: 'u' } });
    const result = await store.getLinkByGithub('U');
    expect(result).toEqual({ discord_id: 'd', github_username: 'u' });
    expect(ddbMock.commandCalls(QueryCommand)[0].args[0].input.IndexName).toBe('github_username-index');
  });

  test('deleteLink: returns { changes } shape matching SqliteStore', async () => {
    ddbMock.on(DeleteCommand).resolves({ Attributes: { discord_id: 'd' } });
    const result = await store.deleteLink('d');
    expect(result).toEqual({ changes: 1 });
  });

  test('deleteLink: changes=0 when row did not exist', async () => {
    ddbMock.on(DeleteCommand).resolves({});
    const result = await store.deleteLink('d');
    expect(result).toEqual({ changes: 0 });
  });
});

// ── Contributions ──

describe('contributions', () => {
  test('recordContribution: composite PK as <repo>#<pr>, condition attribute_not_exists, returns "recorded" on success', async () => {
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(GetCommand).resolves({ Item: null }); // streak doesn't exist yet
    ddbMock.on(PutCommand).resolves({}); // streak insert
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo', 'Title');
    expect(result).toBe('recorded');
    const putCalls = ddbMock.commandCalls(PutCommand);
    const contribPut = putCalls.find(c => c.args[0].input.Item && c.args[0].input.Item.contribution_id);
    expect(contribPut.args[0].input.Item.contribution_id).toBe('owner/repo#42');
    expect(contribPut.args[0].input.ConditionExpression).toMatch(/attribute_not_exists/);
  });

  test('recordContribution: returns "duplicate" on ConditionalCheckFailedException (dedup, NOT a failure)', async () => {
    const err = new Error('dup');
    err.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(err);
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo');
    expect(result).toBe('duplicate');
  });

  test('recordContribution: returns "failed" on transient errors AND skips streak update', async () => {
    // Tri-state: dedup ('duplicate') and transient failure ('failed')
    // are now distinguishable. The historical-backfill loop in
    // routes/oauth.js increments newCount only on 'recorded', and
    // logs a loud warn when 'failed' count is non-zero so a
    // sustained transient blip during onboarding is visible to ops
    // instead of silently undercounting.
    const err = new Error('throttled');
    err.name = 'ProvisionedThroughputExceededException';
    ddbMock.on(PutCommand).rejects(err);
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo');
    expect(result).toBe('failed');
    // Streak update would fire a second PutCommand if attempted —
    // pinning that it doesn't run when the contribution Put failed
    // protects against the silent "streak rolls forward without an
    // anchored contribution" bug.
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);
  });

  test('getContributions: queries GSI with ScanIndexForward=false', async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ contribution_id: 'r#1', merged_at: 'now' }] });
    const result = await store.getContributions('d', 5);
    expect(result).toHaveLength(1);
    const call = ddbMock.commandCalls(QueryCommand)[0].args[0].input;
    expect(call.IndexName).toBe('discord_id-merged_at-index');
    expect(call.ScanIndexForward).toBe(false);
    expect(call.Limit).toBe(5);
  });

  test('getContributionCount: default (no justWrote) issues exactly one Query, NO retry on count=0', async () => {
    // Default behavior: legitimate-zero callers (e.g. "is this user
    // a contributor at all?") must NOT pay the ~350ms GSI-lag
    // retry budget. Mock returns Count:0 — function should issue
    // ONE Query and return 0 immediately.
    ddbMock.on(QueryCommand).resolves({ Count: 0 });
    const count = await store.getContributionCount('d');
    expect(count).toBe(0);
    expect(ddbMock.commandCalls(QueryCommand)).toHaveLength(1);
    expect(ddbMock.commandCalls(QueryCommand)[0].args[0].input.Select).toBe('COUNT');
  });

  test('getContributionCount: positive count returns immediately (no retry needed)', async () => {
    ddbMock.on(QueryCommand).resolves({ Count: 42 });
    const count = await store.getContributionCount('d');
    expect(count).toBe(42);
    expect(ddbMock.commandCalls(QueryCommand)).toHaveLength(1);
  });

  test('getContributionCount: { justWrote: true } retries on count=0 (GSI-lag mitigation)', async () => {
    // Post-write call sites (checkAndAwardBadges) opt into the
    // bounded retry loop because count=0 here is almost-always
    // GSI replication lag rather than ground truth.
    ddbMock
      .on(QueryCommand)
      .resolvesOnce({ Count: 0 })
      .resolvesOnce({ Count: 0 })
      .resolves({ Count: 1 });
    const count = await store.getContributionCount('d', { justWrote: true });
    expect(count).toBe(1);
    expect(ddbMock.commandCalls(QueryCommand)).toHaveLength(3);
  });

  test('getContributionCount: { justWrote: true } gives up at MAX_RETRIES + 1 attempts and returns 0', async () => {
    // Bounded retry: 4 total attempts (initial + 3 retries) with
    // 50/100/200ms backoff. After all attempts exhausted, return 0
    // — caller sees "no contributions" rather than hanging.
    ddbMock.on(QueryCommand).resolves({ Count: 0 });
    const count = await store.getContributionCount('d', { justWrote: true });
    expect(count).toBe(0);
    expect(ddbMock.commandCalls(QueryCommand)).toHaveLength(4);
  });

  test('getUniqueRepos: paginates Query + projects only `repo` (regression guard for 1MB silent truncation)', async () => {
    // The naive shape — getContributions(discordId, 10000) — issues a
    // single Query with Limit:10000 but DDB still caps response
    // payload at 1MB per page. A heavy contributor would have the
    // Query truncate at 1MB and getUniqueRepos would silently miss
    // MULTI_REPO eligibility. Pagination via LastEvaluatedKey + a
    // ProjectionExpression on `repo` keeps the read cost minimal AND
    // sees every row regardless of payload size.
    ddbMock
      .on(QueryCommand)
      .resolvesOnce({
        Items: [{ repo: 'owner/repo-a' }, { repo: 'owner/repo-b' }, { repo: 'owner/repo-a' }],
        LastEvaluatedKey: { contribution_id: 'page1-end' },
      })
      .resolvesOnce({
        Items: [{ repo: 'owner/repo-c' }, { repo: 'owner/repo-b' }],
        LastEvaluatedKey: { contribution_id: 'page2-end' },
      })
      .resolves({ Items: [{ repo: 'owner/repo-d' }] }); // last page, no LEK

    const repos = await store.getUniqueRepos('d');
    expect(new Set(repos)).toEqual(new Set(['owner/repo-a', 'owner/repo-b', 'owner/repo-c', 'owner/repo-d']));
    const calls = ddbMock.commandCalls(QueryCommand);
    expect(calls).toHaveLength(3);
    // Pagination correctness: LEK fed back as ExclusiveStartKey.
    expect(calls[0].args[0].input.ExclusiveStartKey).toBeUndefined();
    expect(calls[1].args[0].input.ExclusiveStartKey).toEqual({ contribution_id: 'page1-end' });
    expect(calls[2].args[0].input.ExclusiveStartKey).toEqual({ contribution_id: 'page2-end' });
    // ProjectionExpression cuts read cost to just the repo column.
    expect(calls[0].args[0].input.ProjectionExpression).toBe('repo');
  });
});

// ── Badges ──

describe('badges', () => {
  test('awardBadge: PutItem with composite key, returns true on success', async () => {
    ddbMock.on(PutCommand).resolves({});
    const result = await store.awardBadge('d', 'first_pr');
    expect(result).toBe(true);
    const call = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(call.Item).toMatchObject({ discord_id: 'd', badge_type: 'first_pr' });
  });

  test('awardBadge: returns false on ConditionalCheckFailedException (already had it)', async () => {
    const err = new Error('dup');
    err.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(err);
    const result = await store.awardBadge('d', 'first_pr');
    expect(result).toBe(false);
  });

  test('hasBadge: GetItem with composite key', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { discord_id: 'd', badge_type: 'first_pr' } });
    const result = await store.hasBadge('d', 'first_pr');
    expect(result).toBe(true);
  });

  test('checkAndAwardBadges: FIRST_PR awards on count>=1 + !hasBadge (idempotent — survives GSI lag count jumping 0→2)', async () => {
    // Simulates the exact race the round-5 review flagged: GSI lag
    // means the first getContributionCount call for a brand-new
    // contributor returns count=2 instead of count=1 (because by
    // the time the GSI has propagated, a SECOND legitimate
    // contribution has already landed). Strict count===1 would
    // permanently miss FIRST_PR; idempotent count>=1 + !hasBadge
    // still awards it.
    ddbMock.on(QueryCommand).resolves({ Count: 2, Items: [] }); // GSI returns 2
    ddbMock.on(GetCommand).resolves({}); // hasBadge returns false for everything
    ddbMock.on(PutCommand).resolves({}); // every awardBadge succeeds

    const awarded = await store.checkAndAwardBadges('d', 'Add new feature', 'owner/repo');
    expect(awarded).toContain('first_pr');
  });

  test('checkAndAwardBadges: FIRST_PR not re-awarded when hasBadge already true', async () => {
    // Idempotence guard: even if count is huge, we don't double-award.
    ddbMock.on(QueryCommand).resolves({ Count: 47, Items: [] });
    // GetCommand for hasBadge(FIRST_PR) returns truthy; subsequent
    // hasBadge calls (DOCS_HERO, BUG_HUNTER, etc.) also return truthy
    // — irrelevant for this assertion.
    ddbMock.on(GetCommand).resolves({ Item: { discord_id: 'd', badge_type: 'first_pr' } });
    ddbMock.on(PutCommand).resolves({});

    const awarded = await store.checkAndAwardBadges('d', 'Add feature', 'owner/repo');
    expect(awarded).not.toContain('first_pr');
  });
});

// ── Guild configs (encryption path) ──

describe('guild configs', () => {
  test('setGuildApiKey: encrypts apiKey before write, preserves configured_at on re-key', async () => {
    ddbMock.on(UpdateCommand).resolves({});
    await store.setGuildApiKey('g-1', 'plain-key', 'configurer');
    const input = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(input.Key).toEqual({ guild_id: 'g-1' });
    // Encryption: ciphertext stored, never the plaintext.
    expect(input.ExpressionAttributeValues[':k']).toMatch(/^enc:v1:/);
    expect(input.ExpressionAttributeValues[':k']).not.toContain('plain-key');
    // configured_by + updated_at unconditionally set.
    expect(input.ExpressionAttributeValues[':b']).toBe('configurer');
    expect(input.ExpressionAttributeValues[':u']).toBeDefined();
    // Critical parity-with-SQLite invariant: re-key must NOT
    // reset configured_at. SQLite's ON CONFLICT only touched
    // qurl_api_key / configured_by / updated_at; the DDB
    // equivalent is `if_not_exists(configured_at, :u)` so the
    // first-ever-configured timestamp survives subsequent rotations.
    expect(input.UpdateExpression).toMatch(/if_not_exists\(configured_at, :u\)/);
    // Defensive: the bare 'configured_at = :u' shape (which would
    // clobber on every re-key) MUST NOT appear as a SET assignment.
    // Only acceptable form is the `if_not_exists(configured_at, :u)`
    // wrapper above.
    expect(input.UpdateExpression).not.toMatch(/, configured_at = :u\b/);
    expect(input.UpdateExpression).not.toMatch(/^SET configured_at = :u\b/);
  });

  test('getGuildApiKey: decrypts round-trip', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { guild_id: 'g-1', qurl_api_key: `enc:v1:IV:TAG:${Buffer.from('plain-key').toString('hex')}` },
    });
    const result = await store.getGuildApiKey('g-1');
    expect(result).toBe('plain-key');
  });

  test('getGuildConfig: strips qurl_api_key from returned object', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { guild_id: 'g-1', qurl_api_key: 'secret', configured_by: 'admin' },
    });
    const result = await store.getGuildConfig('g-1');
    expect(result).not.toHaveProperty('qurl_api_key');
    expect(result).toMatchObject({ guild_id: 'g-1', configured_by: 'admin' });
  });
});

// ── Orphaned OAuth tokens (encryption + hash path) ──

describe('orphaned tokens', () => {
  test('recordOrphanedToken: hashes plaintext as PK, encrypts ciphertext as access_token, sets TTL, dedups on token_hash', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.recordOrphanedToken('ghp_abc123');
    const call = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    const item = call.Item;
    // Hash is deterministic sha-256 hex
    expect(item.token_hash).toMatch(/^[a-f0-9]{64}$/);
    expect(item.token_hash).not.toContain('ghp_'); // plaintext never in PK
    // Ciphertext uses enc:v1: prefix from the app-layer envelope
    expect(item.access_token).toMatch(/^enc:v1:/);
    // TTL epoch-seconds
    expect(typeof item.expires_at).toBe('number');
    // Dedup guard: a retry / replay of the same plaintext would
    // otherwise overwrite the existing row and push expires_at out
    // — silently extending the credential's queue lifetime past
    // the operator-stated retention. attribute_not_exists makes
    // the second insert a no-op (CCFE swallowed below).
    expect(call.ConditionExpression).toMatch(/attribute_not_exists\(token_hash\)/);
  });

  test('recordOrphanedToken: idempotent on duplicate plaintext (CCFE swallowed, no throw)', async () => {
    const ccfe = new Error('exists');
    ccfe.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(ccfe);
    // Must NOT throw — the original row's expires_at is preserved,
    // matching the operator-stated retention contract.
    await expect(store.recordOrphanedToken('ghp_abc123')).resolves.toBeUndefined();
  });

  test('recordOrphanedToken: non-CCFE errors propagate (so caller can retry / alert)', async () => {
    const err = new Error('throttled');
    err.name = 'ProvisionedThroughputExceededException';
    ddbMock.on(PutCommand).rejects(err);
    await expect(store.recordOrphanedToken('ghp_abc123')).rejects.toThrow(/throttled/);
  });

  test('decryptOrphanedToken: unwraps via crypto util (now async for contract parity)', async () => {
    const cipher = `enc:v1:IV:TAG:${Buffer.from('the-token').toString('hex')}`;
    expect(await store.decryptOrphanedToken(cipher)).toBe('the-token');
  });

  test('listOrphanedTokens: returns { id, encryptedAccessToken } shape matching SqliteStore', async () => {
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { token_hash: 'abc', access_token: 'enc:v1:...', recorded_at: '2026-04-01' },
        { token_hash: 'def', access_token: 'enc:v1:...', recorded_at: '2026-03-01' },
      ],
    });
    const result = await store.listOrphanedTokens(10);
    expect(result).toHaveLength(2);
    expect(result[0]).toHaveProperty('id');
    expect(result[0]).toHaveProperty('encryptedAccessToken');
    // Oldest-first ordering
    expect(result[0].id).toBe('def');
  });

  test('listOrphanedTokens: returns the actually-oldest N when total > limit (regression guard)', async () => {
    // Critical sweeper invariant: when the orphan queue exceeds
    // `limit`, the returned slice MUST be the oldest-by-recorded_at
    // entries — those are the ones closest to the 7-day TTL purge
    // and most urgent to retry. A naive `ScanCommand({ Limit })`
    // returns the FIRST `limit` items in DDB internal-hash order
    // and then sorts THAT subset; the actually-oldest tokens may
    // never appear in it. scanAll + sort + slice gives true
    // oldest-N, matching SqliteStore's `ORDER BY recorded_at ASC
    // LIMIT ?` shape.
    //
    // Fixture: 100 items with `recorded_at` decreasing by index.
    // The 50 oldest are indices 50..99 (newest sort key = '2026-01-50',
    // oldest = '2026-01-99'). DDB returns them in arbitrary order;
    // the function must surface the bottom 50 regardless of the
    // input ordering.
    const items = Array.from({ length: 100 }, (_, i) => ({
      token_hash: `t${String(i).padStart(3, '0')}`,
      access_token: `enc:v1:...${i}`,
      // Pad with leading zeros so lex sort matches numeric sort
      recorded_at: `2026-01-${String(i).padStart(3, '0')}`,
    }));
    // Shuffle to simulate DDB's hash-key order (not chronological)
    const shuffled = items.slice().sort(() => Math.random() - 0.5);
    ddbMock.on(ScanCommand).resolves({ Items: shuffled });

    const result = await store.listOrphanedTokens(50);
    expect(result).toHaveLength(50);
    // First returned item should be the absolute oldest (index 0
    // in original `items`, recorded_at='2026-01-000').
    expect(result[0].id).toBe('t000');
    // Last returned item should be the 49th-oldest (index 49,
    // recorded_at='2026-01-049').
    expect(result[49].id).toBe('t049');
    // Verify monotonic: every entry must be older-or-equal than
    // the next one.
    for (let i = 1; i < result.length; i++) {
      // Reach into the original shape via the id (token_hash) to
      // recover recorded_at for the comparison.
      const prev = items.find(x => x.token_hash === result[i - 1].id).recorded_at;
      const curr = items.find(x => x.token_hash === result[i].id).recorded_at;
      expect(prev <= curr).toBe(true);
    }
  });

  test('countOrphanedTokens: paginates Select=COUNT scans (does NOT undercount past one page)', async () => {
    // Regression guard against the silent-cliff bug: DDB Scan with
    // Select=COUNT is STILL subject to the 1MB cap. A naive
    // single-call ScanCommand returns Count for ONE page only,
    // with LastEvaluatedKey set when more pages exist —
    // dashboards / metrics would silently undercount once the
    // table size pushes past ~1MB. countAll must accumulate
    // Count across pages until LEK is undefined.
    ddbMock
      .on(ScanCommand)
      .resolvesOnce({ Count: 60, LastEvaluatedKey: { token_hash: 'page1-end' } })
      .resolvesOnce({ Count: 40, LastEvaluatedKey: { token_hash: 'page2-end' } })
      .resolves({ Count: 25 }); // last page, no LEK
    const total = await store.countOrphanedTokens();
    expect(total).toBe(125);
    // Three ScanCommands total — first page, follow-up, terminal page.
    expect(ddbMock.commandCalls(ScanCommand)).toHaveLength(3);
    // Pagination correctness: each follow-up must carry the prior
    // page's LEK as ExclusiveStartKey.
    const calls = ddbMock.commandCalls(ScanCommand).map(c => c.args[0].input);
    expect(calls[0].ExclusiveStartKey).toBeUndefined();
    expect(calls[1].ExclusiveStartKey).toEqual({ token_hash: 'page1-end' });
    expect(calls[2].ExclusiveStartKey).toEqual({ token_hash: 'page2-end' });
  });
});

// ── Stats (paginated COUNT) ──

describe('stats', () => {
  test('getStats: linkedUsers paginates Select=COUNT (regression guard for silent-cliff bug)', async () => {
    // Two scans run in parallel: one Select=COUNT against
    // github_links (paginated via countAll), one full scanAll
    // against contributions. github_links count test asserts the
    // pagination loop accumulates correctly; contributions array
    // is mocked empty for this test (its aggregation logic is
    // exercised separately in other tests).
    let scanIdx = 0;
    ddbMock.on(ScanCommand).callsFake((input) => {
      // The contributions scanAll call has no Select option;
      // distinguish by table name.
      if (input.TableName === 'test-prefix-github-links') {
        scanIdx++;
        if (scanIdx === 1) return Promise.resolve({ Count: 75, LastEvaluatedKey: { discord_id: 'p1' } });
        if (scanIdx === 2) return Promise.resolve({ Count: 50 }); // last page
      }
      // contributions scanAll
      return Promise.resolve({ Items: [] });
    });
    const stats = await store.getStats();
    expect(stats.linkedUsers).toBe(125); // 75 + 50, NOT 75 (single-page bug)
    expect(stats.totalContributions).toBe(0);
  });
});

// ── Streaks ──

describe('streaks', () => {
  test('updateStreak: inserts new row when user has no prior streak', async () => {
    ddbMock.on(GetCommand).resolves({}); // no existing streak
    ddbMock.on(PutCommand).resolves({});
    const result = await store.updateStreak('d');
    expect(result).toEqual({ current: 1, longest: 1, isNew: true });
    const putCall = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(putCall.Item.current_streak).toBe(1);
    // Insert-branch race guard: the Put MUST carry
    // attribute_not_exists(discord_id) so a concurrent first-write
    // for the same user can't clobber the first writer silently.
    expect(putCall.ConditionExpression).toMatch(/attribute_not_exists\(discord_id\)/);
  });

  test('updateStreak: handles insert-branch race (CCFE) by recursing into update path with ConsistentRead', async () => {
    const thisMonth = `${new Date().getUTCFullYear()}-${String(new Date().getUTCMonth() + 1).padStart(2, '0')}`;
    // First Get: no existing row (we're the second writer reading
    // before the race). Second Get (after recurse): row IS present,
    // same month, so the function returns { isNew: false } without
    // an UpdateCommand. Order matters because aws-sdk-client-mock
    // resolves all GetCommands with the same mock; we use the
    // sequential `.resolvesOnce` API for branch-specific responses.
    ddbMock
      .on(GetCommand)
      .resolvesOnce({}) // first call: no existing
      .resolves({
        Item: {
          discord_id: 'd',
          current_streak: 1,
          longest_streak: 1,
          last_contribution_week: thisMonth,
        },
      });
    const ccfe = new Error('exists');
    ccfe.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(ccfe);

    const result = await store.updateStreak('d');
    // Concurrent writer's row was the one we found on recurse;
    // same-month branch returns isNew: false without an update.
    expect(result).toEqual({ current: 1, longest: 1, isNew: false });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1); // only our (failed) attempt
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0); // same-month: no update
    const getCalls = ddbMock.commandCalls(GetCommand);
    expect(getCalls).toHaveLength(2); // initial + recurse
    // Critical consistency guard: the recurse-after-CCFE Get MUST
    // be strongly-consistent. Without ConsistentRead, the second
    // call could still return Item: undefined for a few ms after
    // the concurrent winner's PutItem committed, re-entering the
    // insert branch and throwing a second CCFE with nowhere to
    // recover. The first call stays eventually-consistent (cheaper
    // by half a request unit + lower latency on the happy path).
    expect(getCalls[0].args[0].input.ConsistentRead).toBeUndefined();
    expect(getCalls[1].args[0].input.ConsistentRead).toBe(true);
  });

  test('updateStreak: throws loud if ConsistentRead-after-CCFE STILL sees no row (DDB invariant violation)', async () => {
    // Pathological case: CCFE proves the row exists, but a
    // strongly-consistent GetItem still returns undefined. Per DDB
    // contract this can't happen — but rather than infinite-recurse
    // if it ever does (or if a caller passes _afterRace=true
    // incorrectly), the function throws with a diagnostic message.
    ddbMock.on(GetCommand).resolves({}); // both reads return empty
    const ccfe = new Error('exists');
    ccfe.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(ccfe);
    await expect(store.updateStreak('d')).rejects.toThrow(/consistency violation/);
  });

  test('updateStreak: non-CCFE insert errors propagate to caller', async () => {
    ddbMock.on(GetCommand).resolves({});
    const err = new Error('throttled');
    err.name = 'ProvisionedThroughputExceededException';
    ddbMock.on(PutCommand).rejects(err);
    await expect(store.updateStreak('d')).rejects.toThrow(/throttled/);
  });

  test('updateStreak: preserves streak when contribution is in the same month', async () => {
    const thisMonth = `${new Date().getUTCFullYear()}-${String(new Date().getUTCMonth() + 1).padStart(2, '0')}`;
    ddbMock.on(GetCommand).resolves({
      Item: {
        discord_id: 'd',
        current_streak: 3,
        longest_streak: 5,
        last_contribution_week: thisMonth,
      },
    });
    const result = await store.updateStreak('d');
    expect(result).toEqual({ current: 3, longest: 5, isNew: false });
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0); // no update needed
  });

  test('updateStreak: breaks streak when gap > 1 month', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        discord_id: 'd',
        current_streak: 3,
        longest_streak: 5,
        last_contribution_week: '2020-01', // old
      },
    });
    ddbMock.on(UpdateCommand).resolves({});
    const result = await store.updateStreak('d');
    expect(result.current).toBe(1); // reset
    expect(result.longest).toBe(5); // preserved
  });
});

// ── QURL sends lifecycle ──

describe('qurl sends', () => {
  test('recordQURLSend: PutItem with all required fields + default dm_status=pending', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.recordQURLSend({
      sendId: 's1', senderDiscordId: 'sender', recipientDiscordId: 'rcpt',
      resourceId: 'r1', resourceType: 'file', qurlLink: 'https://…',
      expiresIn: '24h', channelId: 'ch1', targetType: 'user',
    });
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.dm_status).toBe('pending');
    expect(item.send_id).toBe('s1');
    expect(item.recipient_discord_id).toBe('rcpt');
    expect(item.sender_discord_id).toBe('sender');
  });

  test('recordQURLSendBatch: chunks >25 items into multiple BatchWrite calls', async () => {
    ddbMock.on(BatchWriteCommand).resolves({ UnprocessedItems: {} });
    const sends = Array.from({ length: 30 }, (_, i) => ({
      sendId: `s${i}`, senderDiscordId: 'sender', recipientDiscordId: `r${i}`,
      resourceId: 'r', resourceType: 'file', qurlLink: '…',
      expiresIn: '24h', channelId: 'ch', targetType: 'user',
    }));
    await store.recordQURLSendBatch(sends);
    const calls = ddbMock.commandCalls(BatchWriteCommand);
    expect(calls).toHaveLength(2); // 25 + 5
  });

  test('recordQURLSendBatch: retries UnprocessedItems with backoff', async () => {
    let attempt = 0;
    ddbMock.on(BatchWriteCommand).callsFake((input) => {
      attempt++;
      // First call: return some items as unprocessed. Second: clean.
      if (attempt === 1) {
        return Promise.resolve({
          UnprocessedItems: {
            'test-prefix-qurl-sends': input.RequestItems['test-prefix-qurl-sends'].slice(0, 2),
          },
        });
      }
      return Promise.resolve({ UnprocessedItems: {} });
    });
    const sends = Array.from({ length: 5 }, (_, i) => ({
      sendId: `s${i}`, senderDiscordId: 'sender', recipientDiscordId: `r${i}`,
      resourceId: 'r', resourceType: 'file', qurlLink: '…',
      expiresIn: '24h', channelId: 'ch', targetType: 'user',
    }));
    await store.recordQURLSendBatch(sends);
    expect(attempt).toBe(2); // one retry
  });

  test('recordQURLSendBatch: throws after MAX_RETRIES when items remain unprocessed', async () => {
    // Always return same items as unprocessed — simulate persistent throttle.
    ddbMock.on(BatchWriteCommand).callsFake((input) => Promise.resolve({
      UnprocessedItems: { 'test-prefix-qurl-sends': input.RequestItems['test-prefix-qurl-sends'] },
    }));
    const sends = [{
      sendId: 's1', senderDiscordId: 'sender', recipientDiscordId: 'r1',
      resourceId: 'r', resourceType: 'file', qurlLink: '…',
      expiresIn: '24h', channelId: 'ch', targetType: 'user',
    }];
    await expect(store.recordQURLSendBatch(sends)).rejects.toThrow(/unprocessed items after/);
  });

  test('updateSendDMStatus: composite-key UpdateItem sets dm_status', async () => {
    ddbMock.on(UpdateCommand).resolves({});
    await store.updateSendDMStatus('s1', 'rcpt', 'sent');
    const input = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(input.Key).toEqual({ send_id: 's1', recipient_discord_id: 'rcpt' });
    expect(input.ExpressionAttributeValues[':s']).toBe('sent');
  });

  test('getRecentSends: uses base-table Query per send for accurate recipient_count', async () => {
    // GSI returns 2 unique sends. Base-table per-send queries return
    // 3 and 5 recipients respectively. Config fetches return null (no
    // revoke). Result should report accurate counts, not GSI-truncated.
    ddbMock.on(QueryCommand).callsFake((input) => {
      const cmd = input;
      if (cmd.IndexName === 'sender_discord_id-created_at-index') {
        return Promise.resolve({
          Items: [
            { send_id: 'sA', recipient_discord_id: 'r1', dm_status: 'sent', created_at: '2026-04-20T00:00:00Z', resource_type: 'file', target_type: 'user' },
            { send_id: 'sB', recipient_discord_id: 'r1', dm_status: 'pending', created_at: '2026-04-19T00:00:00Z', resource_type: 'file', target_type: 'user' },
          ],
        });
      }
      // Base-table queries for recipient count
      const sendId = cmd.ExpressionAttributeValues[':sid'];
      if (sendId === 'sA') {
        return Promise.resolve({ Items: Array.from({ length: 3 }, (_, i) => ({ send_id: 'sA', recipient_discord_id: `r${i}`, dm_status: i < 2 ? 'sent' : 'pending' })) });
      }
      return Promise.resolve({ Items: Array.from({ length: 5 }, (_, i) => ({ send_id: 'sB', recipient_discord_id: `r${i}`, dm_status: 'sent' })) });
    });
    ddbMock.on(GetCommand).resolves({});
    const result = await store.getRecentSends('sender', 10);
    expect(result).toHaveLength(2);
    const sA = result.find(r => r.send_id === 'sA');
    const sB = result.find(r => r.send_id === 'sB');
    expect(sA.recipient_count).toBe(3);
    expect(sA.delivered_count).toBe(2);
    expect(sB.recipient_count).toBe(5);
    expect(sB.delivered_count).toBe(5);
  });

  test('getRecentSends: per-send recipient Query paginates (LEK threading) so a >1MB fanout doesn\'t silently undercount', async () => {
    // Regression guard: a single send fanning out to thousands of
    // recipients can exceed the 1MB Query response cap. Without
    // queryAll's LEK threading, recipient_count would silently
    // truncate at the first page. Mock returns 60 rows on page 1,
    // 40 on page 2 (no more LEK) for send 'big' — assert the
    // function reports recipient_count = 100, not 60.
    ddbMock.on(QueryCommand).callsFake((input) => {
      if (input.IndexName === 'sender_discord_id-created_at-index') {
        return Promise.resolve({
          Items: [{ send_id: 'big', recipient_discord_id: 'r0', dm_status: 'sent', created_at: 'now', resource_type: 'file', target_type: 'user' }],
        });
      }
      // Per-send recipient queries on the base table — paginated.
      // Distinguish first call (no ExclusiveStartKey) from second
      // (has it) to simulate LEK threading.
      if (!input.ExclusiveStartKey) {
        return Promise.resolve({
          Items: Array.from({ length: 60 }, (_, i) => ({ send_id: 'big', recipient_discord_id: `r${i}`, dm_status: 'sent' })),
          LastEvaluatedKey: { send_id: 'big', recipient_discord_id: 'r59' },
        });
      }
      return Promise.resolve({
        Items: Array.from({ length: 40 }, (_, i) => ({ send_id: 'big', recipient_discord_id: `r${60 + i}`, dm_status: 'sent' })),
      });
    });
    ddbMock.on(GetCommand).resolves({}); // no revoke
    const result = await store.getRecentSends('sender', 10);
    expect(result).toHaveLength(1);
    expect(result[0].send_id).toBe('big');
    expect(result[0].recipient_count).toBe(100); // NOT 60 (single-page bug)
    expect(result[0].delivered_count).toBe(100);
  });

  test('getRecentSends: GSI Query carries Limit so a fat send doesn\'t blow up RCU', async () => {
    // Regression guard: a sender whose latest send fanned out to
    // 1000 recipients would, without a per-page Limit, read all
    // 1000 rows on every /qurl history call to extract one unique
    // send_id. Limit pins the worst-case RCU per page; pagination
    // handles "didn't get enough unique send_ids in one page."
    ddbMock.on(QueryCommand).resolves({ Items: [] });
    ddbMock.on(GetCommand).resolves({});
    await store.getRecentSends('sender', 10);
    const gsiCall = ddbMock.commandCalls(QueryCommand).find(c =>
      c.args[0].input.IndexName === 'sender_discord_id-created_at-index'
    );
    expect(gsiCall).toBeDefined();
    expect(gsiCall.args[0].input.Limit).toBe(100);
  });

  test('getRecentSends: filters out revoked sends', async () => {
    ddbMock.on(QueryCommand).callsFake((input) => {
      if (input.IndexName) {
        return Promise.resolve({
          Items: [
            { send_id: 'sA', recipient_discord_id: 'r1', dm_status: 'sent', created_at: 'now', resource_type: 'file', target_type: 'user' },
            { send_id: 'sB', recipient_discord_id: 'r1', dm_status: 'sent', created_at: 'now', resource_type: 'file', target_type: 'user' },
          ],
        });
      }
      return Promise.resolve({ Items: [{ send_id: input.ExpressionAttributeValues[':sid'], recipient_discord_id: 'r1', dm_status: 'sent' }] });
    });
    ddbMock.on(GetCommand).callsFake((input) => {
      // sA is revoked, sB is not
      if (input.Key.send_id === 'sA') {
        return Promise.resolve({ Item: { send_id: 'sA', revoked_at: 'now' } });
      }
      return Promise.resolve({});
    });
    const result = await store.getRecentSends('sender', 10);
    expect(result).toHaveLength(1);
    expect(result[0].send_id).toBe('sB');
  });

  test('markSendRevoked: primary path includes :s in ExpressionAttributeValues (regression guard)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { send_id: 's1', sender_discord_id: 'sender', resource_type: 'file' },
    });
    ddbMock.on(UpdateCommand).resolves({});
    await store.markSendRevoked('s1', 'sender');
    const input = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    // BOTH :t and :s must be populated — if :s is missing, DDB throws
    // ValidationException and every non-legacy revoke fails.
    expect(input.ExpressionAttributeValues[':t']).toBeDefined();
    expect(input.ExpressionAttributeValues[':s']).toBe('sender');
    expect(input.ConditionExpression).toMatch(/sender_discord_id = :s/);
  });

  test('markSendRevoked: idempotent — no-op if already revoked', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { send_id: 's1', sender_discord_id: 'sender', revoked_at: 'already' },
    });
    await store.markSendRevoked('s1', 'sender');
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  test('markSendRevoked: ownership check — rejects different sender', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { send_id: 's1', sender_discord_id: 'owner', resource_type: 'file' },
    });
    await store.markSendRevoked('s1', 'attacker');
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  test('markSendRevoked: legacy branch — inserts minimal config when no config row exists', async () => {
    ddbMock.on(GetCommand).resolves({}); // no config row
    ddbMock.on(QueryCommand).resolves({
      Items: [{ send_id: 's1', sender_discord_id: 'sender', resource_type: 'file', expires_in: '24h' }],
    });
    ddbMock.on(PutCommand).resolves({});
    await store.markSendRevoked('s1', 'sender');
    const putCall = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(putCall.Item.revoked_at).toBeDefined();
    expect(putCall.Item.resource_type).toBe('file');
    expect(putCall.ConditionExpression).toMatch(/attribute_not_exists/);
    // Hardening: legacy lookup must NOT carry Limit:1 — DDB applies
    // Limit before FilterExpression, so a partition where the first
    // server-side row doesn't pass the sender filter would silently
    // miss. Today's invariant (all rows of one send share a sender)
    // makes Limit:1 safe but also unnecessary; dropping it keeps
    // the lookup robust to future migrations / manual repairs that
    // could break the invariant.
    const queryCall = ddbMock.commandCalls(QueryCommand)[0].args[0].input;
    expect(queryCall.Limit).toBeUndefined();
    expect(queryCall.FilterExpression).toMatch(/sender_discord_id = :s/);
  });

  test('markSendRevoked: legacy CCFE recovers via Update (race vs non-revoke writer no longer loses revoke intent)', async () => {
    // Race scenario:
    //   T0: markSendRevoked GETs config_configs — no row, enters legacy branch.
    //   T1: saveSendConfig (or any non-revoke writer) inserts a config
    //       WITHOUT revoked_at.
    //   T2: legacy Put hits attribute_not_exists(send_id) → CCFE.
    // Old code swallowed the CCFE and returned — silent loss of
    // revoke intent (config exists, send is NOT revoked, user thinks
    // it was). New code falls through to flipRevokedAt which Updates
    // the existing config row's revoked_at with the same owner-scoped
    // + attribute_not_exists guard the primary path uses.
    const ccfe = new Error('exists');
    ccfe.name = 'ConditionalCheckFailedException';

    ddbMock.on(GetCommand).resolves({}); // T0: no config row
    ddbMock.on(QueryCommand).resolves({
      Items: [{ send_id: 's1', sender_discord_id: 'sender', resource_type: 'file', expires_in: '24h' }],
    });
    ddbMock.on(PutCommand).rejects(ccfe); // T2: T1 inserted a row, our Put CCFEs
    ddbMock.on(UpdateCommand).resolves({}); // recovery: Update revoked_at

    await store.markSendRevoked('s1', 'sender');

    // Critical: an UpdateCommand MUST fire after the failed Put.
    // Without flipRevokedAt the Update count would be 0 (silent loss).
    const updateCalls = ddbMock.commandCalls(UpdateCommand);
    expect(updateCalls).toHaveLength(1);
    expect(updateCalls[0].args[0].input.UpdateExpression).toMatch(/SET revoked_at/);
    expect(updateCalls[0].args[0].input.ConditionExpression).toMatch(/sender_discord_id = :s/);
  });

  test('saveSendConfig: encrypts attachmentUrl, stores other fields as-is', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.saveSendConfig({
      sendId: 's1', senderDiscordId: 'sender', resourceType: 'file',
      connectorResourceId: 'conn', actualUrl: 'https://…',
      expiresIn: '24h', personalMessage: 'hi', locationName: null,
      attachmentName: 'doc.pdf', attachmentContentType: 'application/pdf',
      attachmentUrl: 'https://cdn.discord/attachment/xyz',
    });
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.attachment_url).toMatch(/^enc:v1:/);
    expect(item.attachment_url).not.toContain('discord/attachment/xyz');
    expect(item.personal_message).toBe('hi'); // non-sensitive fields unchanged
    expect(item.attachment_name).toBe('doc.pdf');
  });

  test('getSendConfig: decrypts attachment_url + ownership check', async () => {
    const encryptedUrl = `enc:v1:IV:TAG:${Buffer.from('https://cdn.example/attachment').toString('hex')}`;
    ddbMock.on(GetCommand).resolves({
      Item: { send_id: 's1', sender_discord_id: 'sender', attachment_url: encryptedUrl, personal_message: 'hi' },
    });
    const result = await store.getSendConfig('s1', 'sender');
    expect(result.attachment_url).toBe('https://cdn.example/attachment');
  });

  test('getSendConfig: ownership check — returns undefined for wrong sender', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { send_id: 's1', sender_discord_id: 'owner', personal_message: 'secret' },
    });
    const result = await store.getSendConfig('s1', 'attacker');
    expect(result).toBeUndefined();
  });
});

// ── Contribution flow resilience ──

describe('recordContribution error separation', () => {
  test('returns true even if updateStreak throws (insert succeeded, streak errored)', async () => {
    // First PutCommand (contribution) succeeds. Next GetCommand
    // (streak) succeeds with null. Subsequent PutCommand for streak
    // throws — must NOT mask the contribution success.
    let putCount = 0;
    ddbMock.on(PutCommand).callsFake(() => {
      putCount++;
      if (putCount === 2) return Promise.reject(new Error('Streak table throttled'));
      return Promise.resolve({});
    });
    ddbMock.on(GetCommand).resolves({}); // no existing streak
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo');
    expect(result).toBe('recorded'); // contribution recorded, badge eval must still run
  });
});

// ── Milestones (composite-key encoding) ──

describe('milestones', () => {
  test('recordMilestone: encodes PK as <repo>#<type>#<value> when repo is non-null', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.recordMilestone('star', 100, 'owner/repo');
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.milestone_id).toBe('owner/repo#star#100');
  });

  test('recordMilestone: encodes PK with __NONE__ sentinel when repo is null', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.recordMilestone('global', 42, null);
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.milestone_id).toBe('__NONE__#global#42');
  });

  test('recordMilestone: returns false on dup (ConditionalCheckFailed)', async () => {
    const err = new Error('dup');
    err.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(err);
    const result = await store.recordMilestone('star', 100, 'r');
    expect(result).toBe(false);
  });
});

// ── Health check ──

describe('healthCheck', () => {
  test('issues a single GetItem against pending_links sentinel key', async () => {
    ddbMock.on(GetCommand).resolves({}); // sentinel key never exists
    const result = await store.healthCheck();
    expect(result).toEqual({ ok: true });
    const calls = ddbMock.commandCalls(GetCommand);
    expect(calls).toHaveLength(1);
    expect(calls[0].args[0].input.TableName).toBe('test-prefix-pending-links');
    expect(calls[0].args[0].input.Key).toEqual({ state: '__healthcheck__' });
  });

  test('does NOT touch any user-data table (no Scan, no leaderboard read)', async () => {
    ddbMock.on(GetCommand).resolves({});
    await store.healthCheck();
    // Critical contract for /health-cadence callers: zero ScanCommands.
    // If a future contributor "improves" healthCheck by adding a
    // getStats() call, this test fires.
    expect(ddbMock.commandCalls(ScanCommand)).toHaveLength(0);
    expect(ddbMock.commandCalls(QueryCommand)).toHaveLength(0);
  });

  test('throws when DDB is unreachable (so /health returns 503)', async () => {
    const netErr = new Error('ENOTFOUND dynamodb.us-east-2.amazonaws.com');
    netErr.name = 'NetworkingError';
    ddbMock.on(GetCommand).rejects(netErr);
    await expect(store.healthCheck()).rejects.toThrow(/ENOTFOUND/);
  });
});

// ── Contract adherence ──

describe('Store contract adherence', () => {
  const { STORE_METHODS, STORE_CONSTANTS, assertStoreShape } = require('../src/store/contract');

  test('ddb-store passes assertStoreShape', () => {
    expect(() => assertStoreShape(store, 'ddb')).not.toThrow();
  });

  test('ddb-store exports every STORE_METHODS entry as a function', () => {
    const missing = STORE_METHODS.filter(m => typeof store[m] !== 'function');
    expect(missing).toEqual([]);
  });

  test('ddb-store exports every STORE_CONSTANTS entry', () => {
    const missing = STORE_CONSTANTS.filter(c => store[c] === undefined);
    expect(missing).toEqual([]);
  });

  test('ddb-store surfaces the same BADGE_TYPES enum as SqliteStore (cross-backend parity)', () => {
    // These are the canonical values the bot code compares against.
    // Drift between backends would cause a Discord interaction that
    // awards a badge in one env to produce a no-match lookup in another.
    expect(store.BADGE_TYPES).toMatchObject({
      FIRST_PR: 'first_pr',
      FIRST_ISSUE: 'first_issue',
      DOCS_HERO: 'docs_hero',
      BUG_HUNTER: 'bug_hunter',
      ON_FIRE: 'on_fire',
      STREAK_MASTER: 'streak_master',
      MULTI_REPO: 'multi_repo',
    });
  });
});

// ── DDB_TABLE_PREFIX boot-time validation ──
//
// The test file at top sets DDB_TABLE_PREFIX='test-prefix-' before
// requiring the store, so the in-process module already passed
// validation. Exercising the failure paths needs a clean require, so
// each case spawns a child `node -e` that requires ddb-store directly
// with a controlled env. The whole point of the validation is to keep
// a developer's local shell with stray AWS creds from hitting prod
// tables, so coverage on every "treat as unset" branch is worth the
// child-process cost.
describe('ddb-store boot-time DDB_TABLE_PREFIX validation', () => {
  const { spawnSync } = require('child_process');
  const path = require('path');
  const requirePath = JSON.stringify(path.resolve(__dirname, '..', 'src/store/ddb-store'));

  function spawnDdbStoreBoot(prefixValue) {
    const env = { ...process.env, JEST_WORKER_ID: '' };
    if (prefixValue === undefined) {
      delete env.DDB_TABLE_PREFIX;
    } else {
      env.DDB_TABLE_PREFIX = prefixValue;
    }
    return spawnSync(process.execPath, ['-e', `require(${requirePath})`], { env, encoding: 'utf8' });
  }

  test('throws when DDB_TABLE_PREFIX is unset', () => {
    const result = spawnDdbStoreBoot(undefined);
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/DDB_TABLE_PREFIX is required/);
    // Names the concrete next action so the operator knows how to fix.
    expect(result.stderr).toMatch(/sandbox/);
    expect(result.stderr).toMatch(/prod/);
  });

  test('throws when DDB_TABLE_PREFIX is empty-string (container-templating bug)', () => {
    const result = spawnDdbStoreBoot('');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/DDB_TABLE_PREFIX is required/);
  });

  test('throws when DDB_TABLE_PREFIX is whitespace-only (container-templating bug)', () => {
    const result = spawnDdbStoreBoot('   ');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/DDB_TABLE_PREFIX is required/);
  });

  test('boots cleanly when DDB_TABLE_PREFIX is set to a real value', () => {
    const result = spawnDdbStoreBoot('qurl-bot-discord-sandbox-');
    expect(result.status).toBe(0);
    // Negative-match instead of strict-empty so a future Node
    // deprecation warning doesn't flake this test — the contract
    // here is "validation didn't fire", not "stderr is silent".
    expect(result.stderr).not.toMatch(/DDB_TABLE_PREFIX/);
  });

  test('throws when DDB_TABLE_PREFIX is missing the trailing dash', () => {
    // Without the trailing '-', concat with kebab table suffixes
    // produces malformed names like 'qurl-bot-discord-sandboxgithub-links'.
    // First DDB call would return ResourceNotFoundException — clear at
    // the call site but confusing in CloudWatch (looks like a perms or
    // schema problem, not a config typo). Boot-time check points
    // directly at the env var.
    const result = spawnDdbStoreBoot('qurl-bot-discord-sandbox');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/must end with '-'/);
    // Names the offending value AND the example malformed table the
    // operator would've otherwise hit — concrete next action.
    expect(result.stderr).toMatch(/qurl-bot-discord-sandbox/);
    expect(result.stderr).toMatch(/github-links/);
  });
});

describe('ddb-store boot-time AWS_REGION validation', () => {
  // Same shape of footgun the DDB_TABLE_PREFIX guard closes: a dev
  // with stray AWS creds + DDB_TABLE_PREFIX set to a real value
  // (sandbox or prod) but unset AWS_REGION would silently land in
  // whichever region the SDK defaults to. Catch at boot.
  const { spawnSync } = require('child_process');
  const path = require('path');
  const requirePath = JSON.stringify(path.resolve(__dirname, '..', 'src/store/ddb-store'));

  function spawnDdbStoreBootWithRegion(regionValue) {
    const env = {
      ...process.env,
      JEST_WORKER_ID: '',
      DDB_TABLE_PREFIX: 'qurl-bot-discord-sandbox-', // unrelated guard must pass
    };
    if (regionValue === undefined) {
      delete env.AWS_REGION;
    } else {
      env.AWS_REGION = regionValue;
    }
    return spawnSync(process.execPath, ['-e', `require(${requirePath})`], { env, encoding: 'utf8' });
  }

  test('throws when AWS_REGION is unset', () => {
    const result = spawnDdbStoreBootWithRegion(undefined);
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/AWS_REGION is required/);
    // Names a concrete next action.
    expect(result.stderr).toMatch(/us-east-2/);
  });

  test('throws when AWS_REGION is empty-string', () => {
    const result = spawnDdbStoreBootWithRegion('');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/AWS_REGION is required/);
  });

  test('throws when AWS_REGION is whitespace-only', () => {
    const result = spawnDdbStoreBootWithRegion('   ');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/AWS_REGION is required/);
  });

  test('boots cleanly when AWS_REGION is set', () => {
    const result = spawnDdbStoreBootWithRegion('us-west-2');
    expect(result.status).toBe(0);
    expect(result.stderr).not.toMatch(/AWS_REGION/);
  });
});

describe('ddb-store boot-time PENDING_LINK_EXPIRY_MINUTES validation', () => {
  // Defends against a future config.js regression where
  // PENDING_LINK_EXPIRY_MINUTES somehow comes through as
  // missing / non-numeric. Today intEnv() provides a default so
  // this is theoretical; if it ever isn't, we want the failure at
  // module load (clear stack trace, easy to fix) rather than at
  // every PutItem (DDB ValidationException, harder to root-cause).
  //
  // Uses the same spawnSync + Module._load patch pattern as the
  // other boot tests rather than jest.isolateModules — the latter
  // doesn't reliably intercept the relative `require('../config')`
  // path from inside ddb-store.js.
  const { spawnSync } = require('child_process');
  const path = require('path');
  const storePath = path.resolve(__dirname, '..', 'src/store/ddb-store');
  const configPath = path.resolve(__dirname, '..', 'src/config');
  const loggerPath = path.resolve(__dirname, '..', 'src/logger');
  const cryptoPath = path.resolve(__dirname, '..', 'src/utils/crypto');

  function bootWithExpiry(expiryValue) {
    // The child uses Module._load patching to swap the config
    // module's PENDING_LINK_EXPIRY_MINUTES without touching the
    // rest. Real config / logger / crypto stay loaded for
    // everything else; only the field we're stress-testing is
    // overridden.
    const script = `
      const Module = require('module');
      const origLoad = Module._load;
      const configP = ${JSON.stringify(configPath)};
      const loggerP = ${JSON.stringify(loggerPath)};
      const cryptoP = ${JSON.stringify(cryptoPath)};
      const expiry = ${JSON.stringify(expiryValue)};
      Module._load = function(request, parent, isMain) {
        const resolved = Module._resolveFilename(request, parent, isMain);
        if (resolved === configP + '.js' || resolved === configP) {
          return { PENDING_LINK_EXPIRY_MINUTES: expiry };
        }
        if (resolved === loggerP + '.js' || resolved === loggerP) {
          return { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} };
        }
        if (resolved === cryptoP + '.js' || resolved === cryptoP) {
          return { encrypt: v => v, decrypt: v => v };
        }
        return origLoad.apply(this, arguments);
      };
      require(${JSON.stringify(storePath)});
    `;
    const env = {
      ...process.env,
      JEST_WORKER_ID: '',
      DDB_TABLE_PREFIX: 'qurl-bot-discord-sandbox-',
      AWS_REGION: 'us-east-2',
    };
    return spawnSync(process.execPath, ['-e', script], { env, encoding: 'utf8' });
  }

  test('throws when PENDING_LINK_EXPIRY_MINUTES is undefined', () => {
    const result = bootWithExpiry(undefined);
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/PENDING_LINK_EXPIRY_MINUTES/);
    expect(result.stderr).toMatch(/positive number/);
  });

  test('throws when PENDING_LINK_EXPIRY_MINUTES is non-numeric (NaN)', () => {
    const result = bootWithExpiry('abc');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/PENDING_LINK_EXPIRY_MINUTES/);
  });

  test('throws when PENDING_LINK_EXPIRY_MINUTES is zero or negative', () => {
    expect(bootWithExpiry(0).status).not.toBe(0);
    expect(bootWithExpiry(-1).status).not.toBe(0);
  });

  test('boots cleanly when PENDING_LINK_EXPIRY_MINUTES is a positive number', () => {
    const result = bootWithExpiry(10);
    expect(result.status).toBe(0);
    expect(result.stderr).not.toMatch(/PENDING_LINK_EXPIRY_MINUTES/);
  });
});
