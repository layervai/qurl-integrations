const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const bootEnvironment = require('./helpers/boot-environment');

jest.setTimeout(15000);

test.each(['listening', 'close'])('SIGTERM during control-channel await: %s unwind (#418/#419)', mode => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'discord-index-race-'));
  let output = '';
  try {
    const result = spawnSync(process.execPath, ['--require', path.join(__dirname, 'helpers/index-shutdown-race.cjs'), path.join(__dirname, '../src/index.js')], {
      cwd: directory, encoding: 'utf8', timeout: 5000,
      env: bootEnvironment(directory, {
        LOG_LEVEL: 'info', BOOT_RACE_MODE: mode, AWS_EC2_METADATA_DISABLED: 'true',
        PROCESS_ROLE: 'gateway', ENABLE_EVENT_SHIPPER: 'true', ENABLE_GATEWAY_RESUME: 'true', ENABLE_GATEWAY_HOT_STANDBY: 'true',
        QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123456789012/fixture',
        INSTANCE_ID: 'fixture-instance', INSTANCE_IP: '10.0.1.42',
        GATEWAY_HANDOFF_HMAC: JSON.stringify({ current: 'c'.repeat(64), previous: 'b'.repeat(64) }),
      }),
    });
    output = result.stdout + result.stderr;
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(0);
    expect(output).not.toContain('INDEX_RACE network-blocked');
    expect(output).not.toContain('unhandledRejection');
    expect(output).not.toContain('Shutdown timed out');
    const events = output.split('\n').filter(line => line.startsWith('INDEX_RACE ')).map(line => line.slice(11));
    for (const event of ['listen-await', 'sigterm', 'health-close', 'control-close', 'leader-stop', 'shim-stop', 'consumer-stop', 'publisher-stop', 'db-close']) {
      expect(events.filter(value => value === event)).toEqual([event]);
    }
    for (const event of ['leader-start', 'watchdog-create', 'watchdog-start', 'heartbeat-start', 'guild-count-start']) expect(events).not.toContain(event);
    expect(output).toContain('Shutdown complete');
    expect(events.filter(event => event.startsWith('shutdown-request-'))).toEqual(
      mode === 'close' ? ['shutdown-request-0', 'shutdown-request-1'] : ['shutdown-request-0']
    );
    if (mode === 'close') {
      expect(output).toContain('Failed to start');
      expect(output).toContain('closed before listening');
    } else {
      expect(events).toContain('listening');
      expect(output).not.toContain('Failed to start');
    }
  } catch (error) {
    throw new Error(`${error.message}\nChild output:\n${output}`, { cause: error });
  } finally { fs.rmSync(directory, { recursive: true, force: true }); }
});
