#!/usr/bin/env node
// One-shot, idempotent table provisioner for the local
// `amazon/dynamodb-local` container that `docker-compose.yml` spins up.
//
// The bot's only data backend is DynamoDB (SQLite was stripped in PR
// #434), so a laptop running `npm start` needs every table the
// `ddb-store` module touches to exist before any data-plane call. In
// prod that's handled by terraform; locally we provision via this
// script.
//
// Schemas mirror the live shape `ddb-store.js` expects — keys, GSIs,
// and TTL attributes. Three tables (`qurl_sends`, `qurl_send_configs`,
// `guild_configs`) match the `modules/qurl-bot-ddb/main.tf`
// definitions in `qurl-integrations-infra`; the rest are inferred from
// `ddb-store.js` call sites (key + GSI usage) for the OpenNHP-feature
// tables (`github_links`, `pending_links`, `contributions`, `badges`,
// `streaks`, `milestones`, `orphaned_oauth_tokens`). Those tables are
// intentionally absent from the non-OpenNHP prod deployment per
// `ddb-store.js`'s top-of-file comment about unused tables — local dev
// provisions them so an operator can exercise OpenNHP code paths
// without flipping ENABLE_OPENNHP_FEATURES off first.
//
// Drift caveat: this is a parallel schema source from the terraform
// module. If `ddb-store.js` adds a new GSI / changes a key, this
// script must follow. The contract test in `tests/ddb-store.test.js`
// pins call-site shape against the real bot code, so a drift surfaces
// as a failed local boot — not as a silent prod mismatch.
//
// Usage:
//   docker compose up -d dynamodb-local
//   DDB_TEST_ENDPOINT=http://localhost:8000 \
//   DDB_TABLE_PREFIX=qurl-bot-discord-local- \
//   AWS_REGION=us-east-1 \
//   AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local \
//   node scripts/provision-ddb-local.js
//
// Re-running after a `docker compose up` (which flushes in-memory DDB)
// is fine — every CreateTable call is wrapped in a
// `ResourceInUseException` catch that treats the table as already
// present.

const {
  DynamoDBClient,
  CreateTableCommand,
  DescribeTableCommand,
  UpdateTimeToLiveCommand,
} = require('@aws-sdk/client-dynamodb');

const endpoint = process.env.DDB_TEST_ENDPOINT || 'http://localhost:8000';
const prefix = (process.env.DDB_TABLE_PREFIX ?? '').trim() || 'qurl-bot-discord-local-';
const region = process.env.AWS_REGION || 'us-east-1';

if (!prefix.endsWith('-')) {
  console.error(`DDB_TABLE_PREFIX must end with '-' (got '${prefix}').`);
  process.exit(1);
}

const client = new DynamoDBClient({
  region,
  endpoint,
  // DDB-Local rejects requests with no credentials. The values aren't
  // checked — any non-empty pair works. The Docker README documents
  // the same workaround.
  credentials: {
    accessKeyId: process.env.AWS_ACCESS_KEY_ID || 'local',
    secretAccessKey: process.env.AWS_SECRET_ACCESS_KEY || 'local',
  },
});

// Schemas mirror what `ddb-store.js` expects at runtime. KeySchema is
// PK (HASH) + optional SK (RANGE); GSIs project ALL by default unless
// the call site only reads keys (KEYS_ONLY). TTL attribute name is
// listed separately — DDB requires a follow-up UpdateTimeToLive call.
const tables = [
  {
    name: 'pending-links',
    keySchema: [{ AttributeName: 'state', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'state', AttributeType: 'S' }],
    ttlAttribute: 'expires_at',
  },
  {
    name: 'github-links',
    keySchema: [{ AttributeName: 'discord_id', KeyType: 'HASH' }],
    attributes: [
      { AttributeName: 'discord_id', AttributeType: 'S' },
      { AttributeName: 'github_username', AttributeType: 'S' },
    ],
    gsis: [{
      IndexName: 'github_username-index',
      KeySchema: [{ AttributeName: 'github_username', KeyType: 'HASH' }],
      Projection: { ProjectionType: 'KEYS_ONLY' },
    }],
  },
  {
    name: 'contributions',
    keySchema: [{ AttributeName: 'contribution_id', KeyType: 'HASH' }],
    attributes: [
      { AttributeName: 'contribution_id', AttributeType: 'S' },
      { AttributeName: 'discord_id', AttributeType: 'S' },
      { AttributeName: 'merged_at', AttributeType: 'S' },
    ],
    gsis: [{
      IndexName: 'discord_id-merged_at-index',
      KeySchema: [
        { AttributeName: 'discord_id', KeyType: 'HASH' },
        { AttributeName: 'merged_at', KeyType: 'RANGE' },
      ],
      Projection: { ProjectionType: 'ALL' },
    }],
  },
  {
    name: 'badges',
    keySchema: [
      { AttributeName: 'discord_id', KeyType: 'HASH' },
      { AttributeName: 'badge_type', KeyType: 'RANGE' },
    ],
    attributes: [
      { AttributeName: 'discord_id', AttributeType: 'S' },
      { AttributeName: 'badge_type', AttributeType: 'S' },
    ],
  },
  {
    name: 'streaks',
    keySchema: [{ AttributeName: 'discord_id', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'discord_id', AttributeType: 'S' }],
  },
  {
    name: 'milestones',
    keySchema: [{ AttributeName: 'milestone_id', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'milestone_id', AttributeType: 'S' }],
  },
  {
    name: 'qurl-sends',
    keySchema: [
      { AttributeName: 'send_id', KeyType: 'HASH' },
      { AttributeName: 'recipient_discord_id', KeyType: 'RANGE' },
    ],
    attributes: [
      { AttributeName: 'send_id', AttributeType: 'S' },
      { AttributeName: 'recipient_discord_id', AttributeType: 'S' },
      { AttributeName: 'sender_discord_id', AttributeType: 'S' },
      { AttributeName: 'created_at', AttributeType: 'S' },
    ],
    gsis: [{
      IndexName: 'sender_discord_id-created_at-index',
      KeySchema: [
        { AttributeName: 'sender_discord_id', KeyType: 'HASH' },
        { AttributeName: 'created_at', KeyType: 'RANGE' },
      ],
      Projection: { ProjectionType: 'ALL' },
    }],
  },
  {
    name: 'qurl-send-configs',
    keySchema: [{ AttributeName: 'send_id', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'send_id', AttributeType: 'S' }],
  },
  {
    name: 'orphaned-oauth-tokens',
    keySchema: [{ AttributeName: 'token_hash', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'token_hash', AttributeType: 'S' }],
    ttlAttribute: 'expires_at',
  },
  {
    name: 'guild-configs',
    keySchema: [{ AttributeName: 'guild_id', KeyType: 'HASH' }],
    attributes: [{ AttributeName: 'guild_id', AttributeType: 'S' }],
  },
  // NOTE: `weekly_stats` is listed in `ddb-store.js`'s TABLES map but
  // has no DDB call site today. When the first reader/writer lands,
  // add the schema here.
];

async function ensureTable(spec) {
  const TableName = `${prefix}${spec.name}`;
  try {
    await client.send(new CreateTableCommand({
      TableName,
      KeySchema: spec.keySchema,
      AttributeDefinitions: spec.attributes,
      BillingMode: 'PAY_PER_REQUEST',
      ...(spec.gsis ? { GlobalSecondaryIndexes: spec.gsis } : {}),
    }));
    console.log(`created  ${TableName}`);
  } catch (err) {
    // String-name check rather than `instanceof ResourceInUseException`
    // survives a dual-loaded SDK (two copies of `@aws-sdk/client-dynamodb`
    // in the require graph would each export their own class identity,
    // breaking instanceof). The SDK always sets `err.name` to the
    // service-side exception name, so this is the canonical check.
    if (err.name === 'ResourceInUseException') {
      console.log(`exists   ${TableName}`);
    } else {
      throw err;
    }
  }
  if (spec.ttlAttribute) {
    // DDB-Local accepts repeated `UpdateTimeToLive ENABLED` calls
    // idempotently. A `ValidationException: TimeToLive is already enabled`
    // can fire on some versions — treat as a no-op.
    try {
      await client.send(new UpdateTimeToLiveCommand({
        TableName,
        TimeToLiveSpecification: { Enabled: true, AttributeName: spec.ttlAttribute },
      }));
    } catch (err) {
      if (!/already enabled/i.test(err.message || '')) throw err;
    }
  }
}

async function main() {
  console.log(`Provisioning ${tables.length} tables on ${endpoint} (prefix='${prefix}')…`);
  // Verify DDB-Local is reachable up front — a typo on DDB_TEST_ENDPOINT
  // otherwise surfaces as a generic per-table CreateTable network error.
  try {
    await client.send(new DescribeTableCommand({ TableName: `${prefix}__probe__` }));
  } catch (err) {
    // ResourceNotFoundException is the expected "reachable but no
    // such table" response — keep going. Any other shape (typically
    // ECONNREFUSED) means the endpoint is wrong or DDB-Local isn't up.
    if (err.name !== 'ResourceNotFoundException') {
      console.error(`Cannot reach DynamoDB at ${endpoint}: ${err.message}`);
      console.error('Is the docker-compose dynamodb-local service running? Try `docker compose up -d dynamodb-local`.');
      process.exit(1);
    }
  }
  for (const spec of tables) await ensureTable(spec);
  console.log('Done.');
}

main().catch(err => {
  console.error('Provisioning failed:', err);
  process.exit(1);
});
