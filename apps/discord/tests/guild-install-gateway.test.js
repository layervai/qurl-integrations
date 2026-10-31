const path = require('node:path');
const { Client } = require('discord.js');
// Intentionally use the exact pinned library's internals; recheck #195 on upgrades.
const discordSource = path.dirname(require.resolve('discord.js'));
const handleReady = require(path.join(discordSource, 'client/websocket/handlers/READY'));
const handleGuildCreate = require(path.join(discordSource, 'client/websocket/handlers/GUILD_CREATE'));
const Status = require(path.join(discordSource, 'util/Status'));

test('real gateway handlers distinguish re-IDENTIFY from a new guild install (#195)', async () => {
  const client = new Client({ intents: [] });
  try {
    client.user = { _patch() {} };
    client.application = { _patch() {} };
    client.ws.status = Status.Ready;
    client.readyTimestamp = 1; // Simulate an already-ready client during reconnect.
    const installed = jest.fn();
    const available = jest.fn();
    client.on('guildCreate', installed);
    client.on('guildAvailable', available);
    const shard = { id: 0, checkReady() {} };
    const existingID = '100000000000000001';
    const packet = { d: { user: {}, application: {}, guilds: [{ id: existingID, unavailable: true }] } };
    const guildPacket = id => ({ d: {
      id, name: 'synthetic', unavailable: false,
      roles: [], emojis: [], features: [], channels: [], members: [],
    } });
    // Real READY seeds the cache even while isReady remains true on reconnect.
    expect(client.isReady()).toBe(true);
    for (const clearCache of [false, false, true]) {
      if (clearCache) client.guilds.cache.clear();
      handleReady(client, packet, shard);
      handleGuildCreate(client, guildPacket(existingID), shard);
    }
    expect(installed).not.toHaveBeenCalled();
    expect(available).toHaveBeenCalledTimes(3);
    handleGuildCreate(client, guildPacket('100000000000000002'), shard);
    expect(installed).toHaveBeenCalledTimes(1);
  } finally {
    await client.destroy();
  }
});
