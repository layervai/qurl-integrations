// Local-only native database check. No Discord or qURL API request is made.
const assert = require('node:assert/strict');
const { randomUUID, randomBytes } = require('node:crypto');
const { DynamoDBClient, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, ScanCommand } = require('@aws-sdk/lib-dynamodb');
const { tables, ensureTable, validateEnv } = require('./provision-ddb-local');
assert(process.env.DDB_TEST_ENDPOINT, 'DDB_TEST_ENDPOINT must select DynamoDB Local');
process.env.AWS_ACCESS_KEY_ID ||= 'localtest';
process.env.AWS_SECRET_ACCESS_KEY ||= 'localtest';
process.env.AWS_REGION ||= 'us-east-2';
process.env.DDB_TABLE_PREFIX = `guild-lifecycle-smoke-${randomUUID()}-`;
process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
process.env.ENABLE_CREDENTIAL_LIFECYCLE = 'true';
validateEnv({ endpoint: process.env.DDB_TEST_ENDPOINT, prefix: process.env.DDB_TABLE_PREFIX });
delete process.env.AWS_PROFILE;
delete process.env.AWS_DEFAULT_PROFILE;
delete process.env.AWS_SESSION_TOKEN;
const store = require('../src/store/ddb-store');
const client = new DynamoDBClient({ region: process.env.AWS_REGION, endpoint: process.env.DDB_TEST_ENDPOINT });
const ddb = DynamoDBDocumentClient.from(client);
const selected = tables.filter(table => ['guild-configs', 'credential-lifecycle'].includes(table.name));
const created = [];
const joinA = '2026-10-01T00:00:00.000000000Z';
const joinB = '2026-10-02T00:00:00.000000000Z';
async function main() {
  try {
    for (const spec of selected) {
      await ensureTable(client, process.env.DDB_TABLE_PREFIX, spec);
      created.push(`${process.env.DDB_TABLE_PREFIX}${spec.name}`);
    }
    const first = { ownerId: 'owner-a', expectedApiKey: null, keyId: 'key-a',
      keyKind: 'dedicated', installationJoinedAt: joinA };
    await store.setGuildApiKey('guild', 'secret-a', 'admin-a', 'oauth', first);
    assert.equal(await store.getGuildApiKey('guild'), 'secret-a');
    await store.enqueueGuildRemoval({ guildId: 'guild', joinedAt: joinA, eventId: '0:10' });
    await assert.rejects(store.getGuildApiKey('guild'), { code: 'GUILD_SETUP_REQUIRED' });
    assert.equal(await store.noteGuildInstallation('guild', joinA), false);
    await assert.rejects(store.getGuildApiKey('guild'), { code: 'GUILD_SETUP_REQUIRED' });
    await store.noteGuildInstallation('guild', joinB);
    await assert.rejects(store.getGuildApiKey('guild'), { code: 'GUILD_SETUP_REQUIRED' });
    await store.setGuildApiKey('guild', 'secret-b', 'admin-b', 'oauth', {
      ownerId: 'owner-b', expectedApiKey: 'secret-a', keyId: 'key-b', keyKind: 'dedicated',
      installationJoinedAt: joinB, previousInstallationJoinedAt: joinA,
    });
    assert.equal(await store.getGuildApiKey('guild'), 'secret-b');
    await store.enqueueGuildRemoval({ guildId: 'guild', joinedAt: joinA, eventId: '0:10-replay' });
    assert.equal(await store.getGuildApiKey('guild'), 'secret-b', 'Old removal must not block a fresh setup');
    await assert.rejects(store.setGuildApiKey('guild', 'stale-secret', 'admin-b', 'oauth', {
      ownerId: 'owner-b', expectedApiKey: 'secret-b', installationJoinedAt: joinA,
    }), { code: 'GUILD_SETUP_CHANGED' });
    assert.equal(await store.getGuildApiKey('guild'), 'secret-b');
    await store.enqueueGuildRemoval({ guildId: 'guild', eventId: 'unknown-anchor' });
    assert.equal(await store.getGuildApiKey('guild'), 'secret-b');
    const jobs = (await ddb.send(new ScanCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`, ConsistentRead: true,
    }))).Items;
    assert.equal(jobs.filter(job => job.reason === 'verify_removal').length, 3);
    const retired = jobs.find(job => job.reason === 'rekey');
    assert.equal(retired.qurl_owner_id, 'owner-a');
    assert.equal(require('../src/utils/crypto').decrypt(retired.qurl_api_key), 'secret-a');
    assert(!jobs.some(job => 'expires_at' in job));
    assert.equal((await store.getGuildConfig('guild')).qurl_owner_id, 'owner-b');
    await store.noteGuildInstallation('previously-unconfigured', joinB);
    await assert.rejects(store.setGuildApiKey('previously-unconfigured', 'stale-key', 'admin-a', 'oauth', {
      ownerId: 'owner-a', expectedApiKey: null, installationJoinedAt: joinA,
    }), { code: 'GUILD_SETUP_CHANGED' });
    await store.setGuildApiKey('previously-unconfigured', 'current-key', 'admin-b', 'oauth', {
      ownerId: 'owner-b', expectedApiKey: null, installationJoinedAt: joinB,
    });
    assert.equal(await store.getGuildApiKey('previously-unconfigured'), 'current-key');
    await store.noteGuildInstallation('no-key-installed', joinA);
    await store.enqueueGuildRemoval({ guildId: 'no-key-installed', joinedAt: joinA });
    await assert.rejects(store.setGuildApiKey('no-key-installed', 'stale-key', 'admin-a', 'oauth', {
      ownerId: 'owner-a', expectedApiKey: null, installationJoinedAt: joinA,
    }), { code: 'GUILD_SETUP_CHANGED' });
    const count = await ddb.send(new ScanCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}guild-configs`,
      Select: 'COUNT', FilterExpression: 'attribute_exists(qurl_api_key)', ConsistentRead: true }));
    assert.equal(count.Count, 2, 'Unconfigured installation observations must not count as configured guilds');

  } finally {
    try {
      for (const TableName of created.reverse()) await client.send(new DeleteTableCommand({ TableName }));
    } finally { client.destroy(); await store.close(); }
  }
  console.log('PASS: removal, replay, reinstall, owner replacement, retained credential and stale-write guards; local tables deleted.');
}
main().catch(err => { console.error(err); process.exitCode = 1; });
