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

// Tighter than `endsWith('-')`: also rejects a bare `'-'` (which
// would produce table names like `-github-links`). Matches the
// pattern terraform's `qurl-bot-ddb` module uses for `local.table_prefix`.
// 64-char cap catches a copy-paste accident before any
// `CreateTable` call surfaces DDB's 255-char `TableName` limit
// (prefix + the longest suffix `orphaned-oauth-tokens` = ~85 chars
// of headroom).
if (!/^[a-z0-9][a-z0-9-]*-$/.test(prefix) || prefix.length > 64) {
  console.error(`DDB_TABLE_PREFIX must match /^[a-z0-9][a-z0-9-]*-$/ and be at most 64 chars (got '${prefix}').`);
  process.exit(1);
}

// Defense-in-depth refusal: this script is a local-dev tool only.
// `AWS_ACCESS_KEY_ID || 'local'` below means an operator with real AWS
// creds in their shell who runs the script with `DDB_TEST_ENDPOINT`
// pointed at a real AWS endpoint would otherwise happily provision
// 10 tables in real DDB (with `PAY_PER_REQUEST` billing — small but
// nonzero blast radius). Refuse anything that doesn't look like a
// local loopback or in-VPC test endpoint.
{
  let parsed;
  try { parsed = new URL(endpoint); } catch {
    console.error(`Invalid DDB_TEST_ENDPOINT URL: '${endpoint}'.`);
    process.exit(1);
  }
  // Exact-match allowlist rather than `.local` / `.internal` suffix
  // matching: an attacker who controls mDNS or hosts-file resolution
  // could otherwise route `attacker.local` through the guard.
  // Realistic local-dev hostnames beyond loopback are `0.0.0.0`
  // (some operators set `DDB_TEST_ENDPOINT=http://0.0.0.0:8000`
  // since docker-compose binds 0.0.0.0:8000 by default — clients
  // technically connect to 127.0.0.1, but the OS accepts the
  // literal 0.0.0.0 as the destination and routes locally) and
  // `host.docker.internal` (bot-in-container reaching a docker-
  // compose service on the host). If a new hostname becomes
  // legitimate, add it here explicitly.
  //
  // Acknowledged thin attack surface on `host.docker.internal`:
  // unlike the loopback addresses, this resolves to the host's
  // routable IP. An operator running this script INSIDE a
  // container that (a) has real AWS creds in env, (b) has a host
  // route to actual AWS DDB, and (c) somehow has DDB_TEST_ENDPOINT
  // pointed at `host.docker.internal` with a working AWS port
  // could in principle provision tables in real AWS. Mitigated by
  // (1) the explicit `local` credential fallback for DDB-Local
  // typical usage, (2) DDB-Local's AWS-incompatible HTTP shape
  // surfacing as a fast-fail on real-AWS reach. If this combo ever
  // looks plausible in practice, add a secondary guard that
  // refuses when `AWS_ACCESS_KEY_ID` looks like a real key (e.g.
  // starts with `AKIA` / `ASIA`).
  const LOCAL_HOSTNAMES = new Set([
    'localhost',
    '127.0.0.1',
    '::1',
    '0.0.0.0',
    'host.docker.internal',
  ]);
  const isLocal = LOCAL_HOSTNAMES.has(parsed.hostname);
  if (!isLocal) {
    console.error(`Refusing to run: DDB_TEST_ENDPOINT='${endpoint}' does not look like a local DDB endpoint.`);
    console.error('This script is for `amazon/dynamodb-local` only — never point it at a real AWS account.');
    console.error(`Allowed hostnames: ${[...LOCAL_HOSTNAMES].join(', ')}.`);
    process.exit(1);
  }
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
  // NOTE: TABLES.weekly_stats is listed in `ddb-store.js`'s TABLES map
  // but has no DDB call site today. When the first reader/writer
  // lands, add the schema here — `git grep 'TABLES.weekly_stats'`
  // will surface both this marker and the new call site so the pair
  // moves together.
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
    await client.send(new DescribeTableCommand({ TableName: `${prefix}reachability-probe` }));
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
