const { Client, GatewayIntentBits } = require('discord.js');
const { createDispatchProbe, createProbeStore, consumeProbe, validChunk } = require('../src/dispatch-probe');
const guildId = '123456789012345678';
const userId = '234567890123456789';
function fixture(shipping = true) {
  let time = 0, row;
  const client = new Client({ intents: [GatewayIntentBits.Guilds, GatewayIntentBits.GuildMembers] });
  client.guilds._add({ id: guildId, name: 'probe-fixture', unavailable: false });
  const store = {
    prepare: jest.fn(async item => { row = { ...item }; }),
    read: jest.fn(async () => row),
    acknowledge: jest.fn(async (origin, nonce) => {
      if (row?.origin !== origin || row.nonce !== nonce || time > row.deadline) return false;
      row.ack_at ??= time; return true;
    }),
  };
  const transport = { isReady: jest.fn(() => true),
    getTarget: jest.fn(async () => ({ guildId, userId })), request: jest.fn(async () => {}) };
  const publish = shipping ? jest.fn() : null;
  const logger = { audit: jest.fn() };
  const probe = createDispatchProbe({ store, transport, publish, client, logger, now: () => time });
  const packet = () => ({ op: 0, t: 'GUILD_MEMBERS_CHUNK', s: 9, d: {
    guild_id: guildId, nonce: row.nonce, chunk_index: 0, chunk_count: 1,
    members: [{ user: { id: userId, username: 'probe-bot', discriminator: '0', bot: true }, roles: [], joined_at: '2026-01-01T00:00:00Z' }],
  } });
  return { probe, client, store, transport, publish, logger, packet, now: () => time,
    advance: ms => { time += ms; }, row: () => row,
    close: async () => { probe.stop(); await client.destroy(); } };
}

test('quiet ACK-ready gateway has grace then misses only an expected reply', async () => {
  const f = fixture(); await f.probe.tick();
  expect(f.transport.request).not.toHaveBeenCalled();
  f.advance(60_000); await f.probe.tick();
  const packet = f.transport.request.mock.calls[0][1];
  expect(packet).toEqual({ op: 8, d: { guild_id: guildId, user_ids: [userId], nonce: expect.any(String) } });
  expect(Buffer.byteLength(packet.d.nonce)).toBeLessThanOrEqual(32);
  f.advance(30_000); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_missed');
  expect(f.transport.isReady()).toBe(true); await f.close();
});

test('shipping completion requires the real worker SDK event; duplicate delivery is idempotent', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick();
  f.probe.capture(f.packet()); f.probe.capture(f.packet());
  expect(f.publish).toHaveBeenCalledTimes(1);
  await f.probe.tick();
  expect(f.logger.audit).not.toHaveBeenCalledWith('gateway_dispatch_probe_ok', expect.anything());
  let events = 0; f.client.on('guildMembersChunk', () => { events++; });
  const envelope = { probe_origin: f.row().origin, data: f.packet().d };
  f.advance(100);
  expect(await consumeProbe(f.client, envelope, f.store, f.now)).toBe(true);
  expect(await consumeProbe(f.client, envelope, f.store, f.now)).toBe(true);
  expect(events).toBe(1); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_ok', { latency_ms: 100 });
  await f.close();
});

test('queue wedge and late delivery fail even when gateway receives its Discord reply', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick(); f.probe.capture(f.packet());
  f.advance(30_001); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_missed');
  expect(await consumeProbe(f.client, { probe_origin: f.row().origin, data: f.packet().d }, f.store, f.now)).toBe(false);
  await f.close();
});

test('combined raw packet cannot ACK; normal SDK event can', async () => {
  const f = fixture(false); f.advance(60_000); await f.probe.tick(); f.probe.capture(f.packet());
  expect(f.store.acknowledge).not.toHaveBeenCalled();
  const path = require('node:path');
  const handleChunk = require(path.join(path.dirname(require.resolve('discord.js')), 'client/websocket/handlers/GUILD_MEMBERS_CHUNK.js'));
  handleChunk(f.client, { d: f.packet().d });
  await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_ok', { latency_ms: 0 });
  await f.close(); expect(f.client.listenerCount('guildMembersChunk')).toBe(0);
});

test('no guild and disconnect reset grace without paging for ordinary inactivity', async () => {
  const f = fixture(); f.advance(60_000); f.transport.getTarget.mockResolvedValueOnce(null); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_unavailable');
  f.transport.isReady.mockReturnValue(false); f.advance(60_000); await f.probe.tick();
  f.transport.isReady.mockReturnValue(true); f.advance(59_999); await f.probe.tick();
  expect(f.transport.request).not.toHaveBeenCalled();
  expect(f.logger.audit).not.toHaveBeenCalledWith('gateway_dispatch_probe_missed'); await f.close();
});

test.each(['nonce', 'guild_id', 'user_id'])('foreign %s cannot ACK', async field => {
  const f = fixture(); f.advance(60_000); await f.probe.tick(); const data = f.packet().d;
  if (field === 'user_id') data.members[0].user.id = '345678901234567890';
  else data[field] = field === 'nonce' ? 'lvprobe_AAAAAAAAAAAAAAAAAAAAAA' : '345678901234567890';
  expect(await consumeProbe(f.client, { probe_origin: f.row().origin, data }, f.store, f.now)).toBe(false);
  expect(f.store.acknowledge).not.toHaveBeenCalled(); await f.close();
});

test('DDB read is strong; ACK is conditional, idempotent, and does not extend TTL', async () => {
  const send = jest.fn().mockResolvedValue({ Item: { nonce: 'n' } });
  const store = createProbeStore({ ddbClient: { send }, tableName: 'fixture-dispatch-probe', now: () => 42 });
  await store.prepare({ origin: 'o', expires_at: 120 });
  expect(send.mock.calls[0][0].input.Item).toEqual({ origin: 'o', expires_at: 120 });
  await store.read('o'); expect(send.mock.calls[1][0].input).toMatchObject({ Key: { origin: 'o' }, ConsistentRead: true });
  await store.acknowledge('o', 'n');
  expect(send.mock.calls[2][0].input).toMatchObject({
    UpdateExpression: 'SET ack_at = if_not_exists(ack_at, :now)',
    ConditionExpression: 'nonce = :nonce AND deadline >= :now',
    ExpressionAttributeValues: { ':nonce': 'n', ':now': 42 },
  });
  send.mockRejectedValueOnce(Object.assign(new Error(), { name: 'ConditionalCheckFailedException' }));
  expect(await store.acknowledge('old', 'nonce')).toBe(false);
});

test('SDK failure cannot write an ACK', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick();
  f.client.guilds.cache.clear(); f.client.guilds.fetch = jest.fn().mockResolvedValue(null);
  await expect(consumeProbe(f.client, { probe_origin: f.row().origin, data: f.packet().d }, f.store, f.now)).rejects.toThrow('SDK event missing');
  expect(f.store.acknowledge).not.toHaveBeenCalled(); await f.close();
});

test('shutdown during row creation cannot send a late probe', async () => {
  const f = fixture(); let release;
  f.store.prepare.mockImplementation(() => new Promise(resolve => { release = resolve; }));
  f.advance(60_000); const task = f.probe.tick(); await Promise.resolve();
  f.probe.stop(); release(); await task;
  expect(f.transport.request).not.toHaveBeenCalled(); await f.close();
});

test('stuck WebSocket send is bounded and still reports the expected missed reply', async () => {
  jest.useFakeTimers();
  const f = fixture();
  try {
    f.transport.request.mockImplementation(() => new Promise(() => {}));
    f.advance(60_000); const task = f.probe.tick();
    await jest.advanceTimersByTimeAsync(3000); await task;
    expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_error');
    f.advance(30_000); await f.probe.tick();
    expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_missed');
  } finally { await f.close(); jest.useRealTimers(); }
});

test('only a matching opcode-8 RATE_LIMITED cancels the expected reply and respects retry_after', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick();
  const limited = { op: 0, t: 'RATE_LIMITED', d: { opcode: 8, retry_after: 120,
    meta: { guild_id: guildId, nonce: f.row().nonce } } };
  f.probe.capture({ ...limited, d: { ...limited.d, meta: { ...limited.d.meta, nonce: 'foreign' } } });
  expect(f.logger.audit).not.toHaveBeenCalled();
  f.probe.capture(limited); f.advance(60_000); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_unavailable', { reason: 'rate_limited' });
  expect(f.logger.audit).not.toHaveBeenCalledWith('gateway_dispatch_probe_missed');
  expect(f.transport.request).toHaveBeenCalledTimes(1);
  f.advance(60_000); await f.probe.tick(); expect(f.transport.request).toHaveBeenCalledTimes(2);
  await f.close();
});

test('guild unavailable and a connection flap cancel pending coverage without a false page', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick();
  f.probe.capture({ op: 0, t: 'GUILD_DELETE', d: { id: guildId, unavailable: true } });
  f.advance(30_000); await f.probe.tick();
  expect(f.logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_unavailable', { reason: 'guild_removed_or_unavailable' });
  f.advance(30_000); await f.probe.tick();
  f.transport.isReady.mockReturnValue(false); await f.probe.tick();
  f.transport.isReady.mockReturnValue(true); f.advance(59_999); await f.probe.tick();
  expect(f.transport.request).toHaveBeenCalledTimes(2);
  expect(f.logger.audit).not.toHaveBeenCalledWith('gateway_dispatch_probe_missed'); await f.close();
});

test('combined index transport selects an available guild and returns the pinned real-SDK send Promise', async () => {
  const fs = require('node:fs'); const path = require('node:path'); const vm = require('node:vm');
  const source = fs.readFileSync(path.join(__dirname, '../src/index.js'), 'utf8');
  const start = source.indexOf('    const transport = gatewayShim ?');
  const end = source.indexOf('    dispatchProbe = createDispatchProbe(', start);
  expect(start).toBeGreaterThan(0); expect(end).toBeGreaterThan(start);
  const client = new Client({ intents: [GatewayIntentBits.Guilds] });
  expect('_ws' in client.ws).toBe(true);
  expect(typeof require('@discordjs/ws').WebSocketManager.prototype.send).toBe('function');
  client.guilds._add({ id: '345678901234567890', name: 'unavailable', unavailable: true });
  const guild = client.guilds._add({ id: guildId, name: 'available', unavailable: false, channels: [], shardId: 0 });
  client.user = { id: userId };
  const promise = Promise.resolve(); const send = jest.fn(() => promise); client.ws._ws = { send };
  const globals = { client, gatewayShim: null }; vm.createContext(globals);
  vm.runInContext(source.slice(start, end) + '\nthis.transport = transport;', globals);
  const target = await globals.transport.getTarget(); expect(target).toEqual({ guildId, userId, shardId: guild.shardId });
  const packet = { op: 8, d: { guild_id: guildId, user_ids: [userId], nonce: 'fixture' } };
  expect(globals.transport.request(target, packet)).toBe(promise);
  expect(send).toHaveBeenCalledWith(guild.shardId, packet);
  client.guilds.cache.clear(); expect(await globals.transport.getTarget()).toBeNull();
  client.ws._ws = null; await client.destroy();
});


test('empty optional presence arrays are valid; a presence payload never enters the probe worker', async () => {
  const f = fixture(); f.advance(60_000); await f.probe.tick(); const data = f.packet().d;
  expect(validChunk({ ...data, presences: [] })).toBe(true);
  expect(validChunk({ ...data, presences: [{ user: { id: '345678901234567890' } }] })).toBe(false);
  await f.close();
});
