const fs = require('node:fs');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const Module = require('node:module');
const src = path.resolve(__dirname, '../../src');
const mode = process.env.BOOT_RACE_MODE;
const shutdownHelpers = require(path.join(src, 'gateway-shutdown-helpers'));
const mark = event => fs.writeSync(1, `INDEX_RACE ${event}\n`);
const deny = () => { mark('network-blocked'); process.exit(97); };
require('node:net').Socket.prototype.connect = deny;
require('node:net').Server.prototype.listen = deny;
require('node:dns').lookup = deny;
require('node:dns').resolve = deny;
require('node:dns').promises.lookup = deny;
require('node:dns').promises.resolve = deny;
globalThis.fetch = deny;

// Fake listeners need a live handle while the OS queues SIGTERM.
const fixtureTimeout = setTimeout(() => { mark('fixture-timeout'); process.exit(98); }, 4000);
process.on('exit', () => clearTimeout(fixtureTimeout));

let control;
let releaseDrain;
const drain = mode === 'listening' ? new Promise(resolve => { releaseDrain = resolve; }) : Promise.resolve();
process.once('SIGTERM', () => {
  mark('sigterm');
  if (mode === 'listening') setImmediate(() => {
    control.listening = true;
    mark('listening');
    control.emit('listening');
    setImmediate(releaseDrain);
  });
});

const client = new EventEmitter();
client.isReady = () => false;
client.guilds = { cache: new Map() };
client.user = { id: '100000000000000001' };
const shim = { onDispatch: () => {}, hydrate: async () => false, start: async () => {}, stop: async () => mark('shim-stop'), isStarted: () => true, isReady: () => false, isConnected: () => false, isRecovering: () => false, connect: deny };
const leader = { isHoldingLock: () => false, isKnownPeer: () => true, start: () => mark('leader-start'), stop: async () => mark('leader-stop') };
const fakeServer = name => {
  const server = new EventEmitter();
  server.listening = true;
  server.close = callback => { mark(`${name}-close`); server.listening = false; server.emit('close'); callback?.(); };
  return server;
};
const mocks = {
  './gateway-shutdown-helpers': { ...shutdownHelpers, runGracefulShutdown: options => { mark(`shutdown-request-${options.code}`); return shutdownHelpers.runGracefulShutdown(options); } },
  './discord': { client, GATEWAY_INTENTS_BITFIELD: 0, refreshCache: async () => {}, shutdown: async () => {} },
  './commands': { registerCommands: async () => {}, handleCommand: () => {} },
  './flow-dispatch': { handleFlowInteraction: () => {} },
  './server': { startServer: deny, stopIntervals: () => {} },
  './gateway-health': { startGatewayHealthServer: () => fakeServer('health') },
  './gateway-metrics': { startGatewayHeartbeat: () => mark('heartbeat-start'), startActiveGuildCount: () => mark('guild-count-start'), noteGatewayActivity: () => {} },
  './store': { close: async () => mark('db-close') },
  './gateway-ws-shim': { createGatewayWsShim: () => shim },
  './gateway-session-store': { createGatewaySessionStore: () => ({}) },
  './gateway-lock': { createGatewayLock: () => ({}) },
  './gateway-peer-heartbeat': { createPeerHeartbeat: () => ({}) },
  './gateway-leader': { createGatewayLeader: () => leader },
  './gateway-connection-watchdog': { createConnectionWatchdog: () => { mark('watchdog-create'); return { start: () => mark('watchdog-start'), stop: async () => mark('watchdog-stop') }; } },
  './gateway-control-channel': { startControlChannelServer: () => {
    control = fakeServer('control'); control.listening = false;
    setImmediate(() => {
      mark('listen-await');
      if (process.platform === 'win32') process.emit('SIGTERM');
      else process.kill(process.pid, 'SIGTERM');
    });
    return control;
  } },
  './event-consumer': { stop: () => { mark('consumer-stop'); return drain; }, trackDispatch: () => {} },
  './event-publisher': { start: () => mark('publisher-start'), stop: async () => mark('publisher-stop'), publish: () => {} },
};
const resolved = new Map(Object.entries(mocks).map(([name, value]) => [require.resolve(path.join(src, name)), value]));
const load = Module._load;
Module._load = function(request, parent, isMain) {
  const filename = Module._resolveFilename(request, parent, isMain);
  return resolved.has(filename) ? resolved.get(filename) : load.apply(this, arguments);
};
