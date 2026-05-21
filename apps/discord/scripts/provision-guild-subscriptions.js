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

  // Scan the guild_configs table for every row with a stored API key.
  // We use the raw store helpers (not scanGuildSubscriptions, which
  // filters down to rows that ALREADY have a webhook_id) because the
  // backfill universe is precisely "has key, no subscription yet".
  //
  // listAll-style helper isn't exposed on the contract; getAllGuilds
  // doesn't exist either. We approximate by scanning + filtering
  // through getGuildConfigWithApiKey for each candidate guildId. To
  // discover candidates, we use scanGuildSubscriptions PLUS the
  // contract's awkward fact that DDB scanAll inside ddb-store.js
  // already returns full rows. The cleanest path: import scanAll via
  // an internal scan helper. The pragmatic path: small inline scan
  // here.
  //
  // Pragmatic path: the bot's contract exposes nothing for "list every
  // guildId in guild_configs". Add a helper via DDB SDK directly so
  // the script stays single-purpose and doesn't grow the Store
  // contract just for a one-off operator tool.

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
  const TABLE = `qurl-bot-discord-${config.ENVIRONMENT || process.env.ENVIRONMENT || 'sandbox'}-guild-configs`;

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
      console.log(`[backfill] candidate guild_id=${guildId}`);
      if (DRY_RUN) continue;
      // Per-row try/catch so one corrupt encrypted row doesn't abort
      // the whole backfill.
      try {
        const apiKey = await db.getGuildApiKey(guildId);
        if (!apiKey) {
          console.warn(`[backfill] guild_id=${guildId} qurl_api_key decrypted empty — skipping`);
          skipped += 1;
          continue;
        }
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
