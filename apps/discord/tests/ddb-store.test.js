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
}));

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
    // Number type, epoch seconds, ~10 min out
    const ttl = calls[0].args[0].input.Item.expires_at;
    expect(typeof ttl).toBe('number');
    expect(ttl).toBeGreaterThan(Math.floor(Date.now() / 1000));
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
  test('recordContribution: composite PK as <repo>#<pr>, condition attribute_not_exists', async () => {
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(GetCommand).resolves({ Item: null }); // streak doesn't exist yet
    ddbMock.on(PutCommand).resolves({}); // streak insert
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo', 'Title');
    expect(result).toBe(true);
    const putCalls = ddbMock.commandCalls(PutCommand);
    const contribPut = putCalls.find(c => c.args[0].input.Item && c.args[0].input.Item.contribution_id);
    expect(contribPut.args[0].input.Item.contribution_id).toBe('owner/repo#42');
    expect(contribPut.args[0].input.ConditionExpression).toMatch(/attribute_not_exists/);
  });

  test('recordContribution: returns false on ConditionalCheckFailedException (dup)', async () => {
    const err = new Error('dup');
    err.name = 'ConditionalCheckFailedException';
    ddbMock.on(PutCommand).rejects(err);
    const result = await store.recordContribution('d', 'g', 42, 'owner/repo');
    expect(result).toBe(false);
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

  test('getContributionCount: uses Select=COUNT on GSI', async () => {
    ddbMock.on(QueryCommand).resolves({ Count: 42 });
    const count = await store.getContributionCount('d');
    expect(count).toBe(42);
    expect(ddbMock.commandCalls(QueryCommand)[0].args[0].input.Select).toBe('COUNT');
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
});

// ── Guild configs (encryption path) ──

describe('guild configs', () => {
  test('setGuildApiKey: encrypts apiKey before PutItem', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.setGuildApiKey('g-1', 'plain-key', 'configurer');
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.qurl_api_key).toMatch(/^enc:v1:/);
    expect(item.qurl_api_key).not.toContain('plain-key'); // raw plaintext not stored
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
  test('recordOrphanedToken: hashes plaintext as PK, encrypts ciphertext as access_token, sets TTL', async () => {
    ddbMock.on(PutCommand).resolves({});
    await store.recordOrphanedToken('ghp_abc123');
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    // Hash is deterministic sha-256 hex
    expect(item.token_hash).toMatch(/^[a-f0-9]{64}$/);
    expect(item.token_hash).not.toContain('ghp_'); // plaintext never in PK
    // Ciphertext uses enc:v1: prefix from the app-layer envelope
    expect(item.access_token).toMatch(/^enc:v1:/);
    // TTL epoch-seconds
    expect(typeof item.expires_at).toBe('number');
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
});

// ── Streaks ──

describe('streaks', () => {
  test('updateStreak: inserts new row when user has no prior streak', async () => {
    ddbMock.on(GetCommand).resolves({}); // no existing streak
    ddbMock.on(PutCommand).resolves({});
    const result = await store.updateStreak('d');
    expect(result).toEqual({ current: 1, longest: 1, isNew: true });
    const putItem = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(putItem.current_streak).toBe(1);
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
    expect(result).toBe(true); // contribution recorded, badge eval must still run
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
