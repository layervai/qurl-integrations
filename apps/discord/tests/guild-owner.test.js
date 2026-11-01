jest.mock('../src/config', () => ({ ENABLE_CREDENTIAL_LIFECYCLE: false }));
jest.mock('../src/discord-rest', () => ({ getGuildSetupInstallation: jest.fn() }));
jest.mock('../src/store', () => ({ getGuildConfigWithApiKey: jest.fn() }));
jest.mock('../src/qurl', () => ({ getIdentity: jest.fn() }));
const db = require('../src/store');
const config = require('../src/config');
const { getGuildSetupInstallation } = require('../src/discord-rest');
const { getIdentity } = require('../src/qurl');
const { prepareGuildOwnerBinding } = require('../src/guild-owner');

beforeEach(() => { jest.resetAllMocks(); config.ENABLE_CREDENTIAL_LIFECYCLE = false; });

test('first setup needs a verified owner, not an audit fingerprint', async () => {
  await expect(prepareGuildOwnerBinding('g', '')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
  await expect(prepareGuildOwnerBinding('g', '   ')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
  expect(db.getGuildConfigWithApiKey).not.toHaveBeenCalled();
  await expect(prepareGuildOwnerBinding('g', 'owner')).resolves.toEqual({ ownerId: 'owner', expectedApiKey: null });
});

test('same-owner rotation uses the durable owner without an identity network lookup', async () => {
  db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_owner_id: 'owner', qurl_api_key: 'saved' });
  await expect(prepareGuildOwnerBinding('g', 'owner')).resolves.toEqual({ ownerId: 'owner', expectedApiKey: 'saved' });
  expect(getIdentity).not.toHaveBeenCalled();
  await expect(prepareGuildOwnerBinding('g', 'another')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
});

test('legacy owner migration needs the saved credential to prove the same owner', async () => {
  db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_api_key: 'saved' });
  getIdentity.mockResolvedValue({ owner_id: 'owner' });
  await expect(prepareGuildOwnerBinding('g', 'owner')).resolves.toEqual({ ownerId: 'owner', expectedApiKey: 'saved' });
  expect(getIdentity).toHaveBeenCalledWith('saved', 'g');
  await expect(prepareGuildOwnerBinding('g', 'another')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
  getIdentity.mockResolvedValue({});
  await expect(prepareGuildOwnerBinding('g', 'owner')).rejects.toThrow('identity owner is missing');
  getIdentity.mockRejectedValue(new Error('unavailable'));
  await expect(prepareGuildOwnerBinding('g', 'owner')).rejects.toThrow('unavailable');
});


test('enabled lifecycle binds first setup to a currently verified installation', async () => {
  config.ENABLE_CREDENTIAL_LIFECYCLE = true;
  getGuildSetupInstallation.mockResolvedValue({ ok: true, joinedAt: 'current-installation' });
  await expect(prepareGuildOwnerBinding('g', 'owner', 'admin')).resolves.toEqual({
    ownerId: 'owner', expectedApiKey: null, installationJoinedAt: 'current-installation',
  });
  expect(getGuildSetupInstallation).toHaveBeenCalledWith('g', 'admin');
});

test('enabled lifecycle refuses unverified current admin or missing user before reading credentials', async () => {
  config.ENABLE_CREDENTIAL_LIFECYCLE = true;
  getGuildSetupInstallation.mockResolvedValue({ ok: false });
  await expect(prepareGuildOwnerBinding('g', 'owner', 'former-admin')).rejects.toMatchObject({ code: 'GUILD_INSTALLATION_UNVERIFIED' });
  await expect(prepareGuildOwnerBinding('g', 'owner')).rejects.toMatchObject({ code: 'GUILD_INSTALLATION_UNVERIFIED' });
  expect(db.getGuildConfigWithApiKey).not.toHaveBeenCalled();
  expect(getIdentity).not.toHaveBeenCalled();
});

test('a new installation with a current admin does not inherit the old billing owner', async () => {
  config.ENABLE_CREDENTIAL_LIFECYCLE = true;
  getGuildSetupInstallation.mockResolvedValue({ ok: true, joinedAt: 'new-installation' });
  db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_installation_joined_at: 'old-installation',
    qurl_owner_id: 'former-owner', qurl_api_key: 'old-key' });
  await expect(prepareGuildOwnerBinding('g', 'new-owner', 'current-admin')).resolves.toEqual({
    ownerId: 'new-owner', expectedApiKey: 'old-key', installationJoinedAt: 'new-installation',
    previousInstallationJoinedAt: 'old-installation',
  });
  expect(getIdentity).not.toHaveBeenCalled();
});

test('the same installation still requires its existing billing owner', async () => {
  config.ENABLE_CREDENTIAL_LIFECYCLE = true;
  getGuildSetupInstallation.mockResolvedValue({ ok: true, joinedAt: 'same-installation' });
  db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_installation_joined_at: 'same-installation',
    qurl_owner_id: 'existing-owner', qurl_api_key: 'saved' });
  await expect(prepareGuildOwnerBinding('g', 'new-owner', 'admin')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
});


test('a confirmed former admin receives a permission refusal before credential reads', async () => {
  config.ENABLE_CREDENTIAL_LIFECYCLE = true;
  getGuildSetupInstallation.mockResolvedValue({ ok: false, reason: 'not_admin' });
  await expect(prepareGuildOwnerBinding('g', 'owner', 'former-admin')).rejects.toMatchObject({ code: 'GUILD_SETUP_FORBIDDEN' });
  expect(db.getGuildConfigWithApiKey).not.toHaveBeenCalled();
});
