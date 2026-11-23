// TODO(upstream-contract): /v1/me supplies canonical owner, key kind and scopes.
function qurlKeyOwner(identity) {
  const data = identity?.data;
  if (data?.auth_type !== 'api_key' || data.api_key?.kind !== 'api_key'
      || typeof data.owner_id !== 'string' || !data.owner_id.length
      || data.owner_id.length > 512
      || !Array.isArray(data.api_key.scopes)
      || !['qurl:read', 'qurl:write'].every(scope => data.api_key.scopes?.includes(scope))) {
    const error = new Error('qURL key must have an authenticated owner and read/write scopes');
    error.code = 'QURL_KEY_OWNER_INVALID';
    throw error;
  }
  return data.owner_id;
}

module.exports = { qurlKeyOwner };
