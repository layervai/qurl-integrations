#!/usr/bin/env node
// One-shot backfill: register a qurl.accessed webhook subscription
// for every linked guild that doesn't have one yet.
//
// Run AFTER deploying the BYOK view-counter change (the migration
// that introduced setGuildApiKey-time subscription registration).
// Already-linked guilds wouldn't otherwise pick up a subscription
// until they manually re-link via `/qurl setup`; this script catches
// them in one pass.
//
// Idempotent: rows that already have a `webhook_id` are skipped.
// Safe to re-run on partial failures (e.g. transient qurl-service
// 5xx mid-batch) — only the rows that didn't complete on the first
// pass will be touched on the second.
//
// Usage:
//   node apps/discord/scripts/provision-guild-subscriptions.js [--dry-run]
//
// Required env: BASE_URL, QURL_ENDPOINT, AWS credentials with read +
// UpdateItem on the guild_configs table, KEY_ENCRYPTION_KEY (for
// decrypting stored qurl_api_keys), and any other env the bot's
// config.js validates at boot. Easiest invocation is `npm run
// provision-guild-subscriptions` after sourcing the bot's task-def
// env (e.g. via ECS Exec or a sandbox shell with the same envs).
//
// SCOPE: only operates on the qurl_api_key + webhook_* attributes of
// the `guild_configs` DDB table. Does not touch any other table.

'use strict';

const db = require('../src/store');
const config = require('../src/config');
const logger = require('../src/logger');
const { linkGuildWebhookSubscription } = require('../src/guild-webhook-link');

const DRY_RUN = process.argv.includes('--dry-run');

async function main() {
  if (!config.QURL_ENDPOINT || !config.BASE_URL) {
    console.error('FATAL: QURL_ENDPOINT and BASE_URL must be set');
    process.exit(1);
  }
  if (!config.DDB_TABLE_PREFIX || !config.DDB_TABLE_PREFIX.endsWith('-')) {
    console.error(`FATAL: DDB_TABLE_PREFIX must be set and end with "-" (got ${JSON.stringify(config.DDB_TABLE_PREFIX)})`);
    process.exit(1);
  }

  // Contract has no "list every guild_configs row" helper, so the
  // script issues its own ScanCommand. Table name MUST come from the
  // bot's own config.DDB_TABLE_PREFIX so the script can't target a
  // different table than the running bot.
  const {
    DynamoDBClient,
  } = require('@aws-sdk/client-dynamodb');
  const {
    DynamoDBDocumentClient,
    ScanCommand,
  } = require('@aws-sdk/lib-dynamodb');
  const ddb = DynamoDBDocumentClient.from(new DynamoDBClient({
    region: process.env.AWS_REGION || 'us-east-2',
  }));
  const TABLE = `${config.DDB_TABLE_PREFIX}guild-configs`;

  let scanned = 0;
  let candidates = 0;
  let skipped = 0;
  let provisioned = 0;
  let failed = 0;
  let ExclusiveStartKey;

  do {
    const res = await ddb.send(new ScanCommand({ TableName: TABLE, ExclusiveStartKey }));
    const rows = res.Items || [];
    for (const row of rows) {
      scanned += 1;
      if (!row.qurl_api_key) { skipped += 1; continue; }
      if (row.webhook_id) { skipped += 1; continue; }
      candidates += 1;
      const guildId = row.guild_id;
      // Per-row try/catch so one corrupt encrypted row doesn't abort
      // the whole backfill. The decrypt is performed even in DRY_RUN
      // mode so operators see decrypt failures in the preview rather
      // than discovering them mid-real-run.
      try {
        const apiKey = await db.getGuildApiKey(guildId);
        if (!apiKey) {
          console.warn(`[backfill] guild_id=${guildId} qurl_api_key decrypted empty — skipping`);
          skipped += 1;
          continue;
        }
        console.log(`[backfill] candidate guild_id=${guildId}`);
        if (DRY_RUN) continue;
        // linkGuildWebhookSubscription also calls subs.upsertGuild
        // on success — that's a no-op in this script context (the
        // registry isn't .start()'d) but DDB is the source of truth,
        // so running bots pick the new row up on their next 30s tick.
        const result = await linkGuildWebhookSubscription({
          guildId, apiKey, descriptionContext: 'via=backfill-script',
        });
        if (result.ok) {
          provisioned += 1;
          console.log(`[backfill] provisioned guild_id=${guildId} action=${result.action}`);
        } else {
          failed += 1;
          console.error(`[backfill] FAILED guild_id=${guildId} reason=${result.reason}`);
        }
      } catch (rowErr) {
        failed += 1;
        console.error(`[backfill] FAILED guild_id=${guildId} threw: ${rowErr?.message}`);
      }
    }
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);

  console.log('\n=== Backfill summary ===');
  console.log(`Table:        ${TABLE}`);
  console.log(`Dry run:      ${DRY_RUN}`);
  console.log(`Scanned:      ${scanned}`);
  console.log(`Candidates:   ${candidates}`);
  console.log(`Skipped:      ${skipped}`);
  if (!DRY_RUN) {
    console.log(`Provisioned:  ${provisioned}`);
    console.log(`Failed:       ${failed}`);
  }
  // Exit non-zero if any candidate failed so operator CI / shell
  // chains catch partial-failure runs.
  process.exit(failed > 0 ? 2 : 0);
}

main().catch((err) => {
  logger.error('provision-guild-subscriptions crashed', { error: err.message, stack: err.stack });
  console.error(err);
  process.exit(1);
});
