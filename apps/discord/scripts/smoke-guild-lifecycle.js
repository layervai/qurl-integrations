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
process.env.CREDENTIAL_CLEANUP_SCOPE = 'migrated-local-fleet';
process.env.DISCORD_CLIENT_ID = '123456789012345678';
process.env.QURL_API_KEY = 'global-key';
process.env.QURL_WEBHOOK_SECRET = 'global-secret';
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
    const replayId = await store.enqueueGuildRemoval({ guildId: 'guild', joinedAt: joinC });
    const replayJob = (await lifecycleRows()).find(row => row.job_id === replayId);
    const replaySnapshot = await store.getGuildCleanupSnapshot('guild');
    assert.equal(await store.restoreGuildInstallation(replayJob, replaySnapshot), true);
    assert.equal(await store.getGuildApiKey('guild'), 'secret-c', 'Fresh membership proof can restore a stale removal marker');
    assert.equal(await store.restoreGuildInstallation(replayJob, replaySnapshot), false, 'An old removal cannot clear a later marker');
    // Claim fencing, crash recovery, retained capability and monotonic installation.
    assert((await store.migrateGuildWebhookReferences('migrated-local-fleet', { allWritersAdopted: true })).registered >= 2);
    const removalId = await store.enqueueGuildRemoval({ guildId: 'guild' });
    const removal = (await lifecycleRows()).find(row => row.job_id === removalId);
    await ddb.send(new UpdateCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`,
      Key: { pk: removal.pk, sk: removal.sk }, UpdateExpression: 'SET due_at = :due', ExpressionAttributeValues: { ':due': 0 } }));
    const claimed = await store.claimCredentialCleanup(removal.due_bucket, 1);
    assert.equal(claimed.job_id, removalId);
    assert.equal(await store.claimCredentialCleanup(removal.due_bucket, 1), null, 'Claim lease excludes other workers');
    const oldSnapshot = await store.getGuildCleanupSnapshot('guild');
    await store.setGuildApiKey('guild', 'secret-c-new', 'admin-c', 'oauth', {
      ownerId: 'owner-c', expectedApiKey: 'secret-c', keyId: 'key-c-new', keyKind: 'dedicated', installationJoinedAt: joinC,
    });
    await assert.rejects(store.retireGuildCredential(claimed, oldSnapshot));
    const freshSnapshot = await store.getGuildCleanupSnapshot('guild');
    await store.noteGuildInstallation('guild', '2026-10-04T00:00:00.000000000Z');
    const captured = await store.retireGuildCredential(claimed, freshSnapshot);
    assert.equal(require('../src/utils/crypto').decrypt(captured.qurl_api_key), 'secret-c-new');
    const tombstone = await store.getGuildCleanupSnapshot('guild');
    assert.equal(tombstone.qurl_api_key, undefined);
    assert.equal(tombstone.observed_installation_joined_at, '2026-10-04T00:00:00.000000000Z');
    assert.equal(await store.finishCredentialCleanup(captured, 'retry', 'test_retry'), 'pending');
    assert.equal((await lifecycleRows()).find(row => row.pk === captured.pk).qurl_api_key, captured.qurl_api_key);
    await assert.rejects(store.finishCredentialCleanup(captured, 'complete', 'stale_lease'));
    await ddb.send(new UpdateCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`,
      Key: { pk: captured.pk, sk: captured.sk }, UpdateExpression: 'SET due_at = :due', ExpressionAttributeValues: { ':due': 0 } }));
    const reclaimed = await store.claimCredentialCleanup(removal.due_bucket, 1);
    assert.equal(reclaimed.attempts, 2);
    assert.equal(await store.claimWebhookCleanup({ ...reclaimed, webhook_id: 'never-linked', webhook_owner_id: 'owner-c' }, 'local-fleet'), false);
    await ddb.send(new (require('@aws-sdk/lib-dynamodb').PutCommand)({ TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`,
      Item: { pk: 'MIGRATION#WEBHOOK_REFERENCES', sk: 'local-fleet', cleanup_state: 'ready', bot_application_id: process.env.DISCORD_CLIENT_ID,
        writer_region: process.env.AWS_REGION, table_prefix: process.env.DDB_TABLE_PREFIX, all_writers_adopted: true } }));
    assert.equal(await store.claimWebhookCleanup({ ...reclaimed, webhook_id: 'migrated-wh', webhook_owner_id: 'owner-b' }, 'local-fleet'), 'referenced');
    assert.equal(await store.claimWebhookCleanup({ ...reclaimed, webhook_id: 'global-wh', webhook_owner_id: 'owner-d' }, 'local-fleet'), 'referenced');
    const webhookJob = { ...reclaimed, webhook_id: 'never-linked', webhook_owner_id: 'owner-c' };
    assert.equal(await store.claimWebhookCleanup(webhookJob, 'local-fleet'), true);
    await store.completeWebhookCleanup(webhookJob);
    assert.equal(await store.claimWebhookCleanup(webhookJob, 'local-fleet'), 'deleted', 'Lost completion reply is recoverable');
    assert.equal(await store.finishCredentialCleanup(reclaimed, 'complete', 'test_done'), 'complete');
    assert.equal((await lifecycleRows()).find(row => row.pk === reclaimed.pk).qurl_api_key, undefined);
    // Real HTTP transport plus real DynamoDB; only Discord membership is stubbed.
    const requests = [];
    const server = require('node:http').createServer((req, res) => {
      requests.push(`${req.method} ${req.url}`);
      assert(['Bearer worker-key', 'Bearer failed-key', 'Bearer uncertain-key'].includes(req.headers.authorization));
      if (req.method === 'GET' && req.url === '/v1/me') {
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify({ data: { owner_id: 'worker-owner', api_key: { key_id: 'worker-key-id', kind: 'dedicated' } } }));
      } else if (req.method === 'DELETE' && ['/v1/webhooks/worker-wh', '/v1/me/api-key'].includes(req.url)) {
        res.writeHead(204); res.end();
      } else { res.writeHead(500); res.end(); }
    });
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    try {
      require('../src/config').QURL_ENDPOINT = `http://127.0.0.1:${server.address().port}`;
      const restPath = require.resolve('../src/discord-rest');
      require.cache[restPath] = { id: restPath, filename: restPath, loaded: true,
        exports: { getBotGuildInstallation: async () => ({ ok: false, status: 404, code: 10007 }) } };
      await store.setGuildApiKey('worker', 'worker-key', 'admin', 'oauth', {
        ownerId: 'worker-owner', expectedApiKey: null, keyId: 'worker-key-id', keyKind: 'dedicated', installationJoinedAt: joinB,
      });
      await store.setGuildWebhookSubscription('worker', { webhookId: 'worker-wh', webhookSecret: 'worker-secret', webhookOwnerId: 'worker-owner' });
      const workerJobId = await store.enqueueGuildRemoval({ guildId: 'worker', joinedAt: joinB });
      const pending = (await lifecycleRows()).find(row => row.job_id === workerJobId);
      await ddb.send(new UpdateCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`,
        Key: { pk: pending.pk, sk: pending.sk }, UpdateExpression: 'SET due_at = :due', ExpressionAttributeValues: { ':due': 0 } }));
      const work = await store.claimCredentialCleanup(pending.due_bucket, 1);
      assert.equal(work.job_id, workerJobId);
      assert.equal(await require('../src/credential-cleanup').processJob(work), 'complete');
      assert.deepEqual(requests, ['DELETE /v1/webhooks/worker-wh', 'DELETE /v1/me/api-key']);
      assert.equal((await store.getGuildCleanupSnapshot('worker')).qurl_api_key, undefined);
      assert.equal((await store.getGuildCleanupSnapshot('worker')).observed_installation_joined_at, joinB);
      assert.equal((await lifecycleRows()).find(row => row.job_id === workerJobId).qurl_api_key, undefined);
      for (const uncertain of [false, true]) {
        requests.length = 0;
        const apiKey = uncertain ? 'uncertain-key' : 'failed-key';
        const id = await store.enqueueUnstoredGuildCredential({ guildId: 'never-saved', apiKey,
          keyId: 'failed-id', ownerId: 'worker-owner', uncertain });
        const compensation = (await lifecycleRows()).find(row => row.job_id === id);
        assert.notEqual(compensation.qurl_api_key, apiKey);
        assert.equal(require('../src/utils/crypto').decrypt(compensation.qurl_api_key), apiKey);
        assert.equal(compensation.reason, uncertain ? 'uncertain_setup' : 'failed_setup');
        assert(!('expires_at' in compensation));
        await ddb.send(new UpdateCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`,
          Key: { pk: compensation.pk, sk: compensation.sk }, UpdateExpression: 'SET due_at = :due', ExpressionAttributeValues: { ':due': 0 } }));
        const claimedCompensation = await store.claimCredentialCleanup(compensation.due_bucket, 1);
        assert.equal(claimedCompensation.job_id, id);
        assert.equal(await require('../src/credential-cleanup').processJob(claimedCompensation), uncertain ? 'manual' : 'complete');
        assert.deepEqual(requests, uncertain ? [] : ['DELETE /v1/me/api-key']);
        const final = (await lifecycleRows()).find(row => row.job_id === id);
        if (uncertain) assert.equal(final.qurl_api_key, compensation.qurl_api_key);
        else assert.equal(final.qurl_api_key, undefined);
      }

    } finally { await new Promise(resolve => server.close(resolve)); }
  } finally {
    try {
      for (const TableName of created.reverse()) await client.send(new DeleteTableCommand({ TableName }));
    } finally { client.destroy(); await store.close(); }
  }
  console.log('PASS: removal, replay, reinstall, owner replacement, retained credential and stale-write guards; local tables deleted.');
}
main().catch(err => { console.error(err); process.exitCode = 1; });
