// Read-only by default. Applying requires an explicit fleet adoption receipt.
const config = require('../src/config');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, ScanCommand } = require('@aws-sdk/lib-dynamodb');
const store = require('../src/store');
const logger = require('../src/logger');
const { AUDIT_EVENTS } = require('../src/constants');
const args = new Set(process.argv.slice(2));
if ([...args].some(arg => !['--apply', '--all-writers-adopted'].includes(arg))) {
  throw new Error('Usage: node scripts/backfill-credential-references.js [--apply --all-writers-adopted]');
}
async function main() {
  if (args.has('--apply')) {
    console.log(await store.migrateGuildWebhookReferences(config.CREDENTIAL_CLEANUP_SCOPE,
      { allWritersAdopted: args.has('--all-writers-adopted') }));
    return;
  }
  const client = new DynamoDBClient({ region: process.env.AWS_REGION,
    ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}) });
  try {
    const ddb = DynamoDBDocumentClient.from(client);
    const counts = { references: 0, zero_reference_heads: 0, protected_heads: 0, pending_jobs: 0, manual_jobs: 0 };
    for (const [table, projection] of [
      ['guild-configs', 'guild_id, webhook_id, webhook_owner_id'],
      ['credential-lifecycle', 'pk, sk, cleanup_state, reference_count, system_reference'],
    ]) {
      let cursor;
      let finished = false;
      for (let page = 0; page < 10000; page++) {
        const result = await ddb.send(new ScanCommand({ TableName: `${config.DDB_TABLE_PREFIX}${table}`,
          ConsistentRead: true, Limit: 100, ProjectionExpression: projection,
          ...(cursor ? { ExclusiveStartKey: cursor } : {}) }));
        for (const row of result.Items ?? []) {
          if (table === 'guild-configs' && row.webhook_id && row.webhook_owner_id) counts.references++;
          if (row.sk === 'HEAD' && row.system_reference) counts.protected_heads++;
          if (row.sk === 'HEAD' && row.cleanup_state === 'active' && row.reference_count === 0 && !row.system_reference) counts.zero_reference_heads++;
          if (row.pk?.startsWith('JOB#') && row.cleanup_state === 'pending') counts.pending_jobs++;
          if (row.pk?.startsWith('JOB#') && row.cleanup_state === 'manual') counts.manual_jobs++;
        }
        cursor = result.LastEvaluatedKey;
        if (!cursor) { finished = true; break; }
      }
      if (!finished) throw new Error('Dry-run page limit reached');
    }
    logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_RESULT, { stage: 'dry-run', ...counts });
  } finally { client.destroy(); }
}
main().catch(() => { console.error('Reference backfill failed; inspect the migration state before retrying. No cleanup authority was granted by this failure.'); process.exitCode = 1; })
  .finally(() => store.close());
