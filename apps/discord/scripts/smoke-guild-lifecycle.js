// Local-only native database check. No Discord or qURL API request is made.
const assert = require('node:assert/strict');
const { randomUUID, randomBytes } = require('node:crypto');
const { DynamoDBClient, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, ScanCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');
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
    const lifecycleRows = async () => (await ddb.send(new ScanCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`, ConsistentRead: true,
    }))).Items;
    const head = async id => (await lifecycleRows()).find(row => row.sk === 'HEAD' && row.webhook_id === id);
    await store.setGuildWebhookSubscription('guild', { webhookId: 'shared-wh', webhookSecret: 'shared-secret', webhookOwnerId: 'owner-b' });
    await store.setGuildApiKey('sibling', 'sibling-key', 'admin-b', 'oauth', {
      ownerId: 'owner-b', expectedApiKey: null, installationJoinedAt: joinB,
    });
    await store.setGuildWebhookSubscription('sibling', { webhookId: 'shared-wh', webhookSecret: 'shared-secret', webhookOwnerId: 'owner-b' });
    assert.equal((await head('shared-wh')).reference_count, 2);
    await store.clearGuildWebhookSubscription('sibling');
    assert.equal((await head('shared-wh')).reference_count, 1);
    await store.clearGuildWebhookSubscription('guild');
    const deferred = (await lifecycleRows()).filter(row => row.reason === 'webhook_relink' && row.webhook_id === 'shared-wh');
    assert.equal(deferred.length, 2, 'Each removed mapping must retain its old cleanup capability');
    assert(deferred.every(job => job.revoke_key === false));
    assert.deepEqual(deferred.map(job => require('../src/utils/crypto').decrypt(job.qurl_api_key)).sort(), ['secret-b', 'sibling-key']);
    assert.equal(await store.getGuildApiKey('guild'), 'secret-b', 'Webhook cleanup cannot revoke the active key');
    const unreferenced = await head('shared-wh');
    assert.equal(unreferenced.reference_count, 0);
    await ddb.send(new UpdateCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`, Key: { pk: unreferenced.pk, sk: 'HEAD' },
      UpdateExpression: 'SET cleanup_state = :deleting',
      ConditionExpression: 'reference_count = :zero AND cleanup_state = :active',
      ExpressionAttributeValues: { ':zero': 0, ':active': 'active', ':deleting': 'deleting' },
    }));
    await assert.rejects(store.setGuildWebhookSubscription('guild', {
      webhookId: 'shared-wh', webhookSecret: 'late-secret', webhookOwnerId: 'owner-b',
    }), { name: 'ConditionalCheckFailedException' });
    assert.equal((await store.getGuildConfig('guild')).webhook_id, undefined);
    assert.equal((await head('shared-wh')).reference_count, 0);
    await store.setGuildWebhookSubscription('guild', { webhookId: 'replacement-wh', webhookSecret: 'replacement-secret', webhookOwnerId: 'owner-b' });
    const joinC = '2026-10-03T00:00:00.000000000Z';
    await store.noteGuildInstallation('guild', joinC);
    await store.setGuildApiKey('guild', 'secret-c', 'admin-c', 'oauth', {
      ownerId: 'owner-c', expectedApiKey: 'secret-b', keyId: 'key-c', keyKind: 'dedicated',
      installationJoinedAt: joinC, previousInstallationJoinedAt: joinB,
    });
    assert.equal((await store.getGuildConfig('guild')).webhook_id, undefined, 'New owner must not inherit the former webhook');
    assert.equal((await head('replacement-wh')).reference_count, 0);
    await assert.rejects(store.setGuildWebhookSubscription('guild', {
      webhookId: 'foreign-wh', webhookSecret: 'foreign-secret', webhookOwnerId: 'foreign-owner',
    }), { code: 'GUILD_WEBHOOK_OWNER_MISMATCH' });
    await ddb.send(new UpdateCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}guild-configs`, Key: { guild_id: 'previously-unconfigured' },
      UpdateExpression: 'SET webhook_id = :id, webhook_secret = :secret, webhook_owner_id = :owner',
      ExpressionAttributeValues: { ':id': 'legacy-wh', ':secret': require('../src/utils/crypto').encryptStrict('legacy-secret'), ':owner': 'owner-b' },
    }));
    await store.setGuildApiKey('previously-unconfigured', 'rotated-key', 'admin-b', 'oauth', {
      ownerId: 'owner-b', expectedApiKey: 'current-key', installationJoinedAt: joinB,
    });
    assert.equal((await head('legacy-wh')).reference_count, 1, 'Key rotation must register retained legacy webhook references');
    await store.setGuildWebhookSubscription('sibling', {
      webhookId: 'legacy-wh', webhookSecret: 'legacy-secret', webhookOwnerId: 'owner-b',
    });
    const propagated = await store.propagateGuildWebhookSubscription('owner-b', {
      webhookId: 'migrated-wh', webhookSecret: 'migrated-secret',
    });
    assert.equal(propagated.updated, 2);
    assert.equal((await head('legacy-wh')).reference_count, 0);
    assert.equal((await head('migrated-wh')).reference_count, 2);
    await store.setGuildApiKey('default', 'global-key', 'admin-d', 'paste', {
      ownerId: 'owner-d', expectedApiKey: null, installationJoinedAt: joinB,
    });
    await store.setGuildWebhookSubscription('default', { webhookId: 'global-wh', webhookSecret: 'global-secret', webhookOwnerId: 'owner-d' });
    await store.setGuildDefaultWebhookOwner('default', { webhookOwnerId: 'owner-d',
      expectedApiKey: 'global-key', expectedDefaultWebhookSecret: 'global-secret' });
    assert.equal((await head('global-wh')).reference_count, 0);
    assert.equal((await head('global-wh')).system_reference, true);


    await ddb.send(new UpdateCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}guild-configs`, Key: { guild_id: 'default' },
      UpdateExpression: 'SET webhook_id = :id, webhook_secret = :secret',
      ExpressionAttributeValues: { ':id': 'legacy-global-wh', ':secret': require('../src/utils/crypto').encryptStrict('global-secret') },
    }));
    await store.setGuildDefaultWebhookOwner('default', { webhookOwnerId: 'owner-d',
      expectedApiKey: 'global-key', expectedDefaultWebhookSecret: 'global-secret' });
    assert.equal((await head('legacy-global-wh')).reference_count, 0);
    assert.equal((await head('legacy-global-wh')).system_reference, true);
  } finally {
    try {
      for (const TableName of created.reverse()) await client.send(new DeleteTableCommand({ TableName }));
    } finally { client.destroy(); await store.close(); }
  }
  console.log('PASS: removal, replay, reinstall, owner replacement, retained credential and stale-write guards; local tables deleted.');
}
main().catch(err => { console.error(err); process.exitCode = 1; });
