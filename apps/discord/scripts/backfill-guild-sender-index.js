// Offline #83 migration. Dry-run by default; never prints send identities or links.
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, ScanCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');
const { guildSenderKey } = require('../src/utils/guild-sender-key');

async function backfillGuildSenderIndex(ddb, tableName, { apply = false } = {}) {
  const counts = { scanned: 0, indexed: 0, candidates: 0, updated: 0, unscoped: 0, invalid: 0, conflicting: 0, raced: 0 };
  let ExclusiveStartKey;
  do {
    const page = await ddb.send(new ScanCommand({
      TableName: tableName,
      ConsistentRead: true,
      Limit: 100,
      ProjectionExpression: '#pk, #sk, #guild, #sender, #created, #tuple',
      ExpressionAttributeNames: {
        '#pk': 'send_id', '#sk': 'recipient_discord_id', '#guild': 'guild_id',
        '#sender': 'sender_discord_id', '#created': 'created_at', '#tuple': 'guild_sender_id',
      },
      ExclusiveStartKey,
    }));
    for (const row of page.Items || []) {
      counts.scanned++;
      if (row.guild_id == null || row.guild_id === '') {
        counts.unscoped++;
        continue;
      }
      if (![row.send_id, row.recipient_discord_id, row.guild_id, row.sender_discord_id, row.created_at]
        .every(value => typeof value === 'string' && value.length > 0)) {
        counts.invalid++;
        continue;
      }
      const tuple = guildSenderKey(row.guild_id, row.sender_discord_id);
      if (Object.hasOwn(row, 'guild_sender_id')) {
        counts[row.guild_sender_id === tuple ? 'indexed' : 'conflicting']++;
        continue;
      }
      counts.candidates++;
      if (!apply) continue;
      try {
        await ddb.send(new UpdateCommand({
          TableName: tableName,
          Key: { send_id: row.send_id, recipient_discord_id: row.recipient_discord_id },
          UpdateExpression: 'SET #tuple = :tuple',
          ConditionExpression: 'attribute_exists(#pk) AND #guild = :guild AND #sender = :sender AND #created = :created AND attribute_not_exists(#tuple)',
          ExpressionAttributeNames: {
            '#pk': 'send_id', '#guild': 'guild_id', '#sender': 'sender_discord_id',
            '#created': 'created_at', '#tuple': 'guild_sender_id',
          },
          ExpressionAttributeValues: {
            ':guild': row.guild_id, ':sender': row.sender_discord_id,
            ':created': row.created_at, ':tuple': tuple,
          },
        }));
        counts.updated++;
      } catch (error) {
        if (error.name !== 'ConditionalCheckFailedException') throw error;
        counts.raced++;
      }
    }
    if (page.LastEvaluatedKey && JSON.stringify(page.LastEvaluatedKey) === JSON.stringify(ExclusiveStartKey)) {
      throw new Error('Backfill scan cursor did not advance');
    }
    ExclusiveStartKey = page.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return counts;
}

async function main(args = process.argv.slice(2)) {
  const apply = args.includes('--apply');
  const tableArgs = args.filter(arg => arg !== '--apply');
  if (args.length !== tableArgs.length + Number(apply) || tableArgs.length !== 1 ||
      !/^qurl-bot-discord-[a-z0-9-]+-qurl-sends$/.test(tableArgs[0]) || !process.env.AWS_REGION?.trim()) {
    throw new Error('Usage: AWS_REGION=<region> node scripts/backfill-guild-sender-index.js <qurl-bot-discord-env-qurl-sends> [--apply]');
  }
  const raw = new DynamoDBClient({ region: process.env.AWS_REGION.trim() });
  try {
    const counts = await backfillGuildSenderIndex(DynamoDBDocumentClient.from(raw), tableArgs[0], { apply });
    console.log(JSON.stringify({ table: tableArgs[0], apply, ...counts }));
    if (counts.unscoped || counts.invalid || counts.conflicting || counts.raced) process.exitCode = 2;
  } finally {
    raw.destroy();
  }
}

if (require.main === module) {
  main().catch(() => {
    console.error('Guild/sender backfill failed; inspect configuration and AWS permissions, then rerun.');
    process.exitCode = 1;
  });
}

module.exports = { backfillGuildSenderIndex, main };
