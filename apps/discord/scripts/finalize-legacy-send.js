#!/usr/bin/env node
'use strict';

const { parseArgs } = require('node:util');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand, QueryCommand, PutCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');
const { hasSafeResourceIdShape } = require('../src/utils/resource-id');

const MAX_PAGES = 200;
const PAGE_SIZE = 100;

class LegacyFinalizationError extends Error {}

async function finalizeLegacySend({ client, prefix, sendId, senderId, apply = false, confirmed = false }) {
  if (typeof prefix !== 'string' || prefix.length > 200 || !/^[A-Za-z0-9_.-]+-$/.test(prefix)) {
    throw new LegacyFinalizationError('An explicit DDB_TABLE_PREFIX ending in a dash is required');
  }
  if (typeof sendId !== 'string' || !/^[A-Za-z0-9_:#.-]{1,200}$/.test(sendId)) throw new LegacyFinalizationError('A valid --send-id is required');
  if (typeof senderId !== 'string' || !/^\d{17,20}$/.test(senderId)) throw new LegacyFinalizationError('A Discord --sender-id is required');
  if (typeof apply !== 'boolean' || typeof confirmed !== 'boolean' || apply !== confirmed) {
    throw new LegacyFinalizationError('A write requires both --apply and --confirm-upstream-closed');
  }
  const signal = AbortSignal.timeout(60_000);
  const send = command => client.send(command, { abortSignal: signal });
  const configTable = `${prefix}qurl-send-configs`;
  const config = (await send(new GetCommand({
    TableName: configTable, Key: { send_id: sendId }, ConsistentRead: true,
    ProjectionExpression: 'sender_discord_id, revoking_at, revoked_at',
  }))).Item;
  if (config && config.sender_discord_id !== senderId) throw new LegacyFinalizationError('Send owner does not match');
  if (config?.revoked_at) return { mode: apply ? 'apply' : 'dry-run', status: 'local_record_already_closed', rows: 0 };

  const readRows = async () => {
    const rows = [];
    let cursor;
    for (let page = 0; page < MAX_PAGES; page++) {
      const result = await send(new QueryCommand({
        TableName: `${prefix}qurl-sends`, ConsistentRead: true, Limit: PAGE_SIZE,
        KeyConditionExpression: 'send_id = :send', ExpressionAttributeValues: { ':send': sendId },
        ProjectionExpression: 'recipient_discord_id, sender_discord_id, resource_id, qurl_id',
        ExclusiveStartKey: cursor,
      }));
      for (const row of result.Items || []) {
        if (row.sender_discord_id !== senderId || typeof row.recipient_discord_id !== 'string' || !row.recipient_discord_id
            || !hasSafeResourceIdShape(row.resource_id) || (row.qurl_id != null && row.qurl_id !== '')) {
          throw new LegacyFinalizationError('Send includes foreign, malformed, or identified rows; use the normal revoke/repair path');
        }
        rows.push([row.recipient_discord_id, row.sender_discord_id, row.resource_id, row.qurl_id ?? null]);
      }
      cursor = result.LastEvaluatedKey;
      if (!cursor || Object.keys(cursor).length === 0) {
        if (rows.length === 0) throw new LegacyFinalizationError('No legacy recipient rows were found');
        return rows.sort((a, b) => a[0].localeCompare(b[0]));
      }
    }
    throw new LegacyFinalizationError('Legacy send exceeds the bounded page budget; no finalization was performed');
  };
  const rows = await readRows();
  if (!apply) return { mode: 'dry-run', status: 'eligible_idless_rows', rows: rows.length, upstream_closure: 'not_checked' };

  const now = new Date().toISOString();
  let barrier = config?.revoking_at;
  if (barrier != null && (typeof barrier !== 'string' || !barrier)) throw new LegacyFinalizationError('Invalid existing revocation barrier');
  if (!config) {
    await send(new PutCommand({
      TableName: configTable,
      Item: { send_id: sendId, sender_discord_id: senderId, created_at: now, revoking_at: now },
      ConditionExpression: 'attribute_not_exists(send_id)',
    }));
    barrier = now;
  } else if (!barrier) {
    const result = await send(new UpdateCommand({
      TableName: configTable, Key: { send_id: sendId },
      UpdateExpression: 'SET revoking_at = if_not_exists(revoking_at, :now)',
      ConditionExpression: 'sender_discord_id = :sender AND attribute_not_exists(revoked_at)',
      ExpressionAttributeValues: { ':sender': senderId, ':now': now }, ReturnValues: 'UPDATED_NEW',
    }));
    barrier = result.Attributes?.revoking_at;
    if (typeof barrier !== 'string' || !barrier) throw new LegacyFinalizationError('Revocation barrier was not confirmed');
  }
  // The current Add Recipients transaction checks this barrier before writing.
  if (JSON.stringify(await readRows()) !== JSON.stringify(rows)) {
    throw new LegacyFinalizationError('Recipient rows changed after the barrier; verify again before finalization');
  }
  await send(new UpdateCommand({
    TableName: configTable, Key: { send_id: sendId },
    UpdateExpression: 'SET revoked_at = :now, legacy_close_reason = :reason',
    ConditionExpression: 'sender_discord_id = :sender AND revoking_at = :barrier AND attribute_not_exists(revoked_at)',
    ExpressionAttributeValues: {
      ':sender': senderId, ':barrier': barrier, ':now': now, ':reason': 'operator_attested_upstream_closed',
    },
  }));
  return { mode: 'apply', status: 'local_record_closed', rows: rows.length, upstream_closure: 'operator_attested' };
}

async function main() {
  const { values } = parseArgs({ options: {
    'send-id': { type: 'string' }, 'sender-id': { type: 'string' },
    apply: { type: 'boolean' }, 'confirm-upstream-closed': { type: 'boolean' }, help: { type: 'boolean' },
  } });
  if (values.help) {
    console.log('Usage: finalize-legacy-send.js --send-id <send> --sender-id <sender> [--apply --confirm-upstream-closed]\nRequired env: AWS_REGION, DDB_TABLE_PREFIX. Default: dry-run.\nApply only after independent confirmation that every upstream link is closed. This tool closes local bookkeeping; it does not verify or revoke links.');
    return;
  }
  const region = process.env.AWS_REGION?.trim();
  if (!region) throw new LegacyFinalizationError('AWS_REGION is required');
  const prefix = process.env.DDB_TABLE_PREFIX?.trim();
  const client = DynamoDBDocumentClient.from(new DynamoDBClient({ region }));
  try {
    const result = await finalizeLegacySend({
      client, prefix, sendId: values['send-id'], senderId: values['sender-id'],
      apply: values.apply ?? false, confirmed: values['confirm-upstream-closed'] ?? false,
    });
    console.log(JSON.stringify({ prefix, region, ...result }));
  } finally {
    client.destroy();
  }
}

if (require.main === module) main().catch(error => {
  const message = error instanceof LegacyFinalizationError
    ? error.message : 'AWS request or argument parsing failed; check the approved account/role and use --help.';
  console.error(`Legacy finalization failed: ${message}`);
  console.error('An intent barrier may remain. Re-run dry-run; use normal revoke/repair if needed.');
  process.exitCode = 1;
});

module.exports = { finalizeLegacySend };
