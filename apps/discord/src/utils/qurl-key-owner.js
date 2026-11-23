// Only qurl-service's authenticated identity response can establish a guild owner.
function qurlKeyOwner(identity) {
  const data = identity?.data;
  if (data?.auth_type !== 'api_key' || data.api_key?.kind !== 'api_key'
      || typeof data.owner_id !== 'string' || !data.owner_id.length
      || data.owner_id.length > 512
      || !Array.isArray(data.api_key.scopes)
      || !['qurl:read', 'qurl:write'].every(scope => data.api_key.scopes?.includes(scope))) {
    throw new Error('qURL key must have an authenticated owner and read/write scopes');
  }
  return data.owner_id;
}

module.exports = { qurlKeyOwner };
