jest.mock('../src/store', () => ({ getGuildConfigWithApiKey: jest.fn() }));
jest.mock('../src/qurl', () => ({ getIdentity: jest.fn() }));
const db = require('../src/store');
const { getIdentity } = require('../src/qurl');
const { prepareGuildOwnerBinding } = require('../src/guild-owner');

beforeEach(() => jest.resetAllMocks());

test('first setup needs a verified owner, not an audit fingerprint', async () => {
  await expect(prepareGuildOwnerBinding('g', '')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
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
  await expect(prepareGuildOwnerBinding('g', 'owner')).rejects.toMatchObject({ code: 'GUILD_OWNER_CONFLICT' });
  getIdentity.mockRejectedValue(new Error('unavailable'));
  await expect(prepareGuildOwnerBinding('g', 'owner')).rejects.toThrow('unavailable');
});
