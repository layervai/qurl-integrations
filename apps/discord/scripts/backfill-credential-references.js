// Read-only by default. Applying requires an explicit fleet adoption receipt.
const config = require('../src/config');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, ScanCommand } = require('@aws-sdk/lib-dynamodb');
const store = require('../src/store');
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
    let cursor;
    let references = 0;
    for (let page = 0; page < 10000; page++) {
      const result = await ddb.send(new ScanCommand({ TableName: `${config.DDB_TABLE_PREFIX}guild-configs`,
        ConsistentRead: true, Limit: 100, ProjectionExpression: 'guild_id, webhook_id, webhook_owner_id',
        ...(cursor ? { ExclusiveStartKey: cursor } : {}) }));
      references += (result.Items ?? []).filter(row => row.webhook_id && row.webhook_owner_id).length;
      cursor = result.LastEvaluatedKey;
      if (!cursor) { console.log({ dry_run: true, references }); return; }
    }
    throw new Error('Dry-run page limit reached');
  } finally { client.destroy(); }
}
main().catch(() => { console.error('Reference backfill failed; inspect the migration state before retrying. No cleanup authority was granted by this failure.'); process.exitCode = 1; })
  .finally(() => store.close());
