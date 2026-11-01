const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../scripts/backfill-credential-references.js'), 'utf8');
const { AUDIT_EVENTS } = require('../src/constants');
async function run(args = []) {
  class ScanCommand { constructor(input) { this.input = input; } }
  const send = jest.fn(async command => {
    expect(command).toBeInstanceOf(ScanCommand);
    return command.input.TableName.endsWith('guild-configs') ? { Items: [{ guild_id: 'private-guild', webhook_id: 'wh', webhook_owner_id: 'private-owner' }] }
      : { Items: [{ pk: 'WH#1', sk: 'HEAD', reference_count: 0, cleanup_state: 'active' },
        { pk: 'WH#2', sk: 'HEAD', reference_count: 0, cleanup_state: 'active', system_reference: true },
        { pk: 'JOB#private', sk: 'META', cleanup_state: 'pending' }, { pk: 'JOB#manual', sk: 'META', cleanup_state: 'manual' }] };
  });
  const audit = jest.fn();
  const migrate = jest.fn(async () => ({ registered: 1 }));
  const modules = {
    '../src/config': { DDB_TABLE_PREFIX: 'test-', CREDENTIAL_CLEANUP_SCOPE: 'fleet' },
    '@aws-sdk/client-dynamodb': { DynamoDBClient: class { destroy() {} } },
    '@aws-sdk/lib-dynamodb': { DynamoDBDocumentClient: { from: () => ({ send }) }, ScanCommand },
    '../src/store': { migrateGuildWebhookReferences: migrate, close: jest.fn() },
    '../src/logger': { audit }, '../src/constants': { AUDIT_EVENTS },
  };
  await vm.runInNewContext(source, { require: name => {
    if (!(name in modules)) throw new Error(`Unexpected import ${name}`);
    return modules[name];
  }, process: { argv: ['node', 'script', ...args], env: { AWS_REGION: 'test-region' } }, console: { log: jest.fn(), error: jest.fn() } });
  return { send, audit, migrate };
}
test('default dry-run only scans and emits aggregate results', async () => {
  const { send, audit, migrate } = await run();
  expect(send).toHaveBeenCalledTimes(2); expect(migrate).not.toHaveBeenCalled();
  expect(audit).toHaveBeenCalledWith(AUDIT_EVENTS.CREDENTIAL_CLEANUP_RESULT, {
    stage: 'dry-run', references: 1, zero_reference_heads: 1, protected_heads: 1, pending_jobs: 1, manual_jobs: 1,
  });
  expect(JSON.stringify(audit.mock.calls)).not.toContain('private');
});
test('explicit apply forwards the adoption attestation', async () => {
  const { send, migrate } = await run(['--apply', '--all-writers-adopted']);
  expect(send).not.toHaveBeenCalled(); expect(migrate).toHaveBeenCalledWith('fleet', { allWritersAdopted: true });
});
