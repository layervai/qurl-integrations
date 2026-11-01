// #1383: one expected Discord reply, through the same SDK and queue as real work.
const { randomUUID, randomBytes } = require('node:crypto');
const path = require('node:path');
const { AUDIT_EVENTS } = require('./constants');
const { PutCommand, GetCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');
// TODO(upstream-contract): pinned discord.js handler; real-SDK queue test guards this internal seam.
const chunkHandler = require(path.join(path.dirname(require.resolve('discord.js')), 'client/websocket/handlers/GUILD_MEMBERS_CHUNK.js'));

const CADENCE_MS = 60_000;
const DEADLINE_MS = 30_000;
const NONCE = /^lvprobe_[A-Za-z0-9_-]{22}$/;
const ORIGIN = /^[a-f0-9-]{36}$/;

function createProbeStore({ ddbClient, tableName, now = Date.now }) {
  const send = command => ddbClient.send(command, { abortSignal: AbortSignal.timeout(3000) });
  return {
    prepare: row => send(new PutCommand({ TableName: tableName, Item: row })),
    async read(origin) {
      const result = await send(new GetCommand({ TableName: tableName, Key: { origin }, ConsistentRead: true }));
      return result.Item;
    },
    async acknowledge(origin, nonce) {
      try {
        await send(new UpdateCommand({
          TableName: tableName, Key: { origin },
          UpdateExpression: 'SET ack_at = if_not_exists(ack_at, :now)',
          ConditionExpression: 'nonce = :nonce AND deadline >= :now',
          ExpressionAttributeValues: { ':nonce': nonce, ':now': now() },
        }));
        return true;
      } catch (err) {
        if (err.name === 'ConditionalCheckFailedException') return false;
        throw err;
      }
    },
  };
}

function validChunk(data) {
  return typeof data?.nonce === 'string' && NONCE.test(data.nonce)
    && typeof data.guild_id === 'string' && /^\d{17,20}$/.test(data.guild_id)
    && data.chunk_index === 0 && data.chunk_count === 1
    && Array.isArray(data.members) && data.members.length === 1
    && typeof data.members[0]?.user?.id === 'string' && /^\d{17,20}$/.test(data.members[0].user.id)
    && (!data.not_found || data.not_found.length === 0)
    && !data.presences;
}

// Do not ACK a raw packet. The SDK must emit its public event first.
function dispatchChunk(client, data) {
  let observed = false;
  const listener = (members, guild, chunk) => {
    if (guild.id === data.guild_id && chunk.nonce === data.nonce
      && members.size === 1 && members.has(data.members[0].user.id)) observed = true;
  };
  client.on('guildMembersChunk', listener);
  try {
    chunkHandler(client, { d: data });
  } finally {
    client.off('guildMembersChunk', listener);
  }
  if (!observed) throw new Error('Dispatch probe SDK event missing');
}

async function consumeProbe(client, envelope, store, now = Date.now) {
  const { probe_origin: origin, data } = envelope;
  if (typeof origin !== 'string' || !ORIGIN.test(origin) || !validChunk(data)) return false;
  const row = await store.read(origin);
  if (!row || row.nonce !== data.nonce || row.guild_id !== data.guild_id
    || row.user_id !== data.members[0].user.id || row.deadline < now()) return false;
  if (Number.isFinite(row.ack_at)) return true;
  if (!client.guilds.cache.has(data.guild_id)) await client.guilds.fetch(data.guild_id);
  dispatchChunk(client, data);
  return store.acknowledge(origin, data.nonce);
}

function createDispatchProbe({ store, transport, publish, client, logger, now = Date.now }) {
  const origin = randomUUID();
  let pending = null;
  let nextAt = now() + CADENCE_MS;
  let stopped = false;
  let busy = false;
  let timer;
  const onChunk = (members, guild, chunk) => {
    if (stopped || !pending || now() > pending.deadline || chunk.nonce !== pending.nonce
      || guild.id !== pending.guild_id || members.size !== 1 || !members.has(pending.user_id)
      || chunk.index !== 0 || chunk.count !== 1) return;
    store.acknowledge(origin, pending.nonce)
      .catch(() => logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_ERROR));
  };
  // The combined client's normal WebSocket handler owns SDK reconstruction.
  if (!publish) client.on('guildMembersChunk', onChunk);

  async function tick() {
    if (stopped || busy) return;
    busy = true;
    try {
      if (!transport.isReady()) {
        pending = null;
        nextAt = now() + CADENCE_MS;
        return;
      }
      if (pending) {
        const row = await store.read(origin);
        if (stopped) return;
        if (row?.nonce === pending.nonce && Number.isFinite(row.ack_at)
          && row.ack_at >= pending.started_at && row.ack_at <= pending.deadline) {
          logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_OK, { latency_ms: row.ack_at - pending.started_at });
          pending = null;
        } else if (now() >= pending.deadline) {
          logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_MISSED);
          pending = null;
        }
        return;
      }
      if (now() < nextAt) return;
      nextAt = now() + CADENCE_MS;
      const target = await transport.getTarget();
      if (stopped || !transport.isReady()) return;
      if (!target) {
        logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_UNAVAILABLE);
        return;
      }
      const startedAt = now();
      const row = {
        origin, nonce: `lvprobe_${randomBytes(16).toString('base64url')}`,
        guild_id: target.guildId, user_id: target.userId,
        started_at: startedAt, deadline: startedAt + DEADLINE_MS,
        expires_at: Math.floor(startedAt / 1000) + 120,
      };
      await store.prepare(row);
      if (stopped || !transport.isReady()) return;
      pending = row;
      let sendTimer;
      try {
        await Promise.race([
          transport.request(target, { op: 8, d: {
            guild_id: row.guild_id, user_ids: [row.user_id], nonce: row.nonce,
          } }),
          new Promise((_, reject) => {
            sendTimer = setTimeout(() => reject(new Error('Dispatch probe send timed out')), 3000);
            sendTimer.unref();
          }),
        ]);
      } finally {
        clearTimeout(sendTimer);
      }
    } catch (_err) {
      // No IDs, tokens, nonce, or SDK response bodies in operational metrics.
      logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_ERROR);
      if (pending && now() >= pending.deadline) {
        logger.audit(AUDIT_EVENTS.GATEWAY_DISPATCH_PROBE_MISSED);
        pending = null;
      }
    } finally {
      busy = false;
    }
  }

  function capture(packet) {
    if (stopped || packet?.op !== 0 || packet.t !== 'GUILD_MEMBERS_CHUNK'
      || !pending || !validChunk(packet.d) || packet.d.nonce !== pending.nonce
      || packet.d.guild_id !== pending.guild_id
      || packet.d.members[0].user.id !== pending.user_id || now() > pending.deadline) return;
    if (publish && !pending.forwarded) {
      pending.forwarded = true;
      publish(packet, origin);
    }
  }

  return {
    tick, capture,
    start() {
      if (!timer && !stopped) {
        timer = setInterval(tick, 5000);
        timer.unref();
      }
    },
    stop() {
      stopped = true; clearInterval(timer); pending = null;
      if (!publish) client.off('guildMembersChunk', onChunk);
    },
  };
}

let runtimeStore;
function getRuntimeProbeStore() {
  if (!runtimeStore) {
    const config = require('./config');
    const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
    const { DynamoDBDocumentClient } = require('@aws-sdk/lib-dynamodb');
    if (!config.DDB_TABLE_PREFIX || !process.env.AWS_REGION) throw new Error('Dispatch probe requires DDB prefix and AWS region');
    const ddbClient = DynamoDBDocumentClient.from(new DynamoDBClient({
      region: process.env.AWS_REGION,
      ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
    }));
    runtimeStore = createProbeStore({ ddbClient, tableName: `${config.DDB_TABLE_PREFIX}dispatch-probe` });
  }
  return runtimeStore;
}

module.exports = { createProbeStore, createDispatchProbe, consumeProbe, validChunk, getRuntimeProbeStore };
