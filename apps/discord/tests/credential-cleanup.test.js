jest.mock('../src/config', () => ({ ENABLE_CREDENTIAL_LIFECYCLE: true, ENABLE_CREDENTIAL_CLEANUP_WORKER: true,
  CREDENTIAL_CLEANUP_SCOPE: 'test-fleet', QURL_ENDPOINT: 'https://api.test', QURL_API_KEY: 'global-key' }));
jest.mock('../src/store', () => ({ claimCredentialCleanup: jest.fn(), finishCredentialCleanup: jest.fn(async (_job, state) => state),
  getGuildCleanupSnapshot: jest.fn(), restoreGuildInstallation: jest.fn(), retireGuildCredential: jest.fn(), claimWebhookCleanup: jest.fn(), completeWebhookCleanup: jest.fn() }));
jest.mock('../src/utils/crypto', () => ({ decrypt: value => value.replace(/^encrypted:/, '') }));
jest.mock('../src/logger', () => ({ error: jest.fn(), audit: jest.fn() }));
jest.mock('../src/discord-rest', () => ({ getBotGuildInstallation: jest.fn() }));
jest.mock('../src/qurl', () => ({ getIdentity: jest.fn() }));
jest.mock('../src/qurl-webhook-registrar', () => ({ callQurlService: jest.fn(), deleteSubscription: jest.fn() }));
jest.mock('../src/webhook-subscriptions', () => ({ removeGuild: jest.fn(), getMutationRevision: jest.fn(() => 1) }));
const db = require('../src/store');
const rest = require('../src/discord-rest');
const qurl = require('../src/qurl');
const remote = require('../src/qurl-webhook-registrar');
const worker = require('../src/credential-cleanup');
const base = { pk: 'JOB#1', sk: 'META', job_id: '1', guild_id: 'g', reason: 'rekey', attempts: 1,
  lease_token: 'lease', qurl_api_key: 'encrypted:old-key', qurl_key_id: 'key1', qurl_key_kind: 'dedicated' };
beforeEach(() => { jest.clearAllMocks(); db.getGuildCleanupSnapshot.mockResolvedValue(null);
  db.claimWebhookCleanup.mockResolvedValue(true); qurl.getIdentity.mockResolvedValue({ owner_id: 'owner' }); });
afterEach(async () => { await worker.stop(); });
test('dedicated key uses the exact self route without an identity dependency', async () => {
  await worker.processJob(base);
  expect(qurl.getIdentity).not.toHaveBeenCalled();
  expect(remote.callQurlService).toHaveBeenCalledWith(expect.objectContaining({ method: 'DELETE', path: '/v1/me/api-key', apiKey: 'old-key', expectedStatus: 204 }));
});
test.each(['shared'])('%s keys are preserved', async kind => {
  await worker.processJob({ ...base, qurl_key_kind: kind });
  expect(remote.callQurlService).not.toHaveBeenCalled();
});
test('a reused active key cannot be revoked', async () => {
  db.getGuildCleanupSnapshot.mockResolvedValue({ qurl_api_key: 'encrypted:old-key' });
  await worker.processJob(base); expect(remote.callQurlService).not.toHaveBeenCalled();
});
test('global keys require operator review', async () => {
  await worker.processJob({ ...base, qurl_api_key: 'encrypted:global-key' });
  expect(db.finishCredentialCleanup).toHaveBeenCalledWith(expect.anything(), 'manual', 'protected_or_missing_capability');
  expect(remote.callQurlService).not.toHaveBeenCalled();
});
test.each([401, 403, 429, 503])('uncertain Discord status %s cannot retire a credential', async status => {
  db.getGuildCleanupSnapshot.mockResolvedValue({ qurl_api_key: 'encrypted:old-key', qurl_installation_joined_at: 'A' });
  rest.getBotGuildInstallation.mockResolvedValue({ ok: false, status });
  await worker.processJob({ ...base, reason: 'verify_removal' });
  expect(db.retireGuildCredential).not.toHaveBeenCalled();
  expect(db.finishCredentialCleanup).toHaveBeenCalledWith(expect.anything(), 'retry', 'membership_unknown');
});
test('a late removal cannot retire a newer installation', async () => {
  db.getGuildCleanupSnapshot.mockResolvedValue({ qurl_api_key: 'encrypted:new-key', qurl_installation_joined_at: 'B' });
  await worker.processJob({ ...base, reason: 'verify_removal', expected_installation_joined_at: 'A' });
  expect(rest.getBotGuildInstallation).not.toHaveBeenCalled(); expect(db.retireGuildCredential).not.toHaveBeenCalled();
});
test('current membership keeps the installed credential', async () => {
  db.getGuildCleanupSnapshot.mockResolvedValue({ qurl_api_key: 'encrypted:old-key', qurl_installation_joined_at: 'A' });
  rest.getBotGuildInstallation.mockResolvedValue({ ok: true, joinedAt: 'A' });
  await worker.processJob({ ...base, reason: 'verify_removal' }); expect(db.retireGuildCredential).not.toHaveBeenCalled();
});
test('confirmed absence retires the exact snapshot before self-revocation', async () => {
  const row = { guild_id: 'g', qurl_api_key: 'encrypted:old-key', qurl_installation_joined_at: 'A' };
  db.getGuildCleanupSnapshot.mockResolvedValueOnce(row).mockResolvedValue(null);
  rest.getBotGuildInstallation.mockResolvedValue({ ok: false, status: 404, code: 10007 });
  db.retireGuildCredential.mockResolvedValue({ ...base, reason: 'removed_credential' });
  await worker.processJob({ ...base, reason: 'verify_removal' });
  expect(db.retireGuildCredential).toHaveBeenCalledWith(expect.anything(), row);
  expect(remote.callQurlService).toHaveBeenCalledTimes(1);
});
test('references retain the shared webhook while the old dedicated key is revoked', async () => {
  db.claimWebhookCleanup.mockResolvedValue('referenced');
  await worker.processJob({ ...base, webhook_id: 'wh', webhook_owner_id: 'owner' });
  expect(remote.deleteSubscription).not.toHaveBeenCalled(); expect(remote.callQurlService).toHaveBeenCalledTimes(1);
});
test('a migration hold retains the capability', async () => {
  db.claimWebhookCleanup.mockResolvedValue(false);
  await worker.processJob({ ...base, webhook_id: 'wh', webhook_owner_id: 'owner' });
  expect(remote.deleteSubscription).not.toHaveBeenCalled(); expect(remote.callQurlService).not.toHaveBeenCalled();
});
test('wrong owner never claims or deletes a webhook', async () => {
  await worker.processJob({ ...base, webhook_id: 'wh', webhook_owner_id: 'other' });
  expect(db.claimWebhookCleanup).not.toHaveBeenCalled(); expect(remote.deleteSubscription).not.toHaveBeenCalled();
});
test('webhook confirmation precedes key revocation', async () => {
  await worker.processJob({ ...base, webhook_id: 'wh', webhook_owner_id: 'owner' });
  expect(remote.deleteSubscription.mock.invocationCallOrder[0]).toBeLessThan(remote.callQurlService.mock.invocationCallOrder[0]);
});
test('an unconfirmed self-revocation never marks the job complete', async () => {
  remote.callQurlService.mockRejectedValueOnce(new Error('cold 401'));
  await expect(worker.processJob(base)).rejects.toThrow('cold 401'); expect(db.finishCredentialCleanup).not.toHaveBeenCalled();
});
test('a reclaimed exhausted job is sent to operator review without remote calls', async () => {
  await worker.processJob({ ...base, attempts: 7 }); expect(remote.callQurlService).not.toHaveBeenCalled();
  expect(db.finishCredentialCleanup).toHaveBeenCalledWith(expect.anything(), 'manual', 'retry_limit');
});

test.each([{ qurl_key_kind: 'unknown' }, { qurl_key_kind: 'dedicated', qurl_key_id: null }])('unknown provenance retains capability for operator recovery: %j', async fields => {
  await worker.processJob({ ...base, ...fields });
  expect(db.finishCredentialCleanup).toHaveBeenCalledWith(expect.anything(), 'manual', 'key_provenance_unknown');
  expect(remote.callQurlService).not.toHaveBeenCalled();
});

test('retained verified owner binding avoids a frozen-account identity dependency', async () => {
  qurl.getIdentity.mockRejectedValue(new Error('account frozen'));
  await worker.processJob({ ...base, qurl_owner_id: 'owner', webhook_owner_id: 'owner', webhook_id: 'wh' });
  expect(qurl.getIdentity).not.toHaveBeenCalled();
  expect(remote.deleteSubscription).toHaveBeenCalledTimes(1);
  expect(remote.callQurlService).toHaveBeenCalledTimes(1);
});
