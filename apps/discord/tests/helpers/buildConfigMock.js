// Test-side reconstitution of the mode derivation that lives in
// src/config.js. Each test file that does `jest.mock('../src/config', ...)`
// previously had to hard-code the derived fields in lockstep — if any
// one drifted, tests passed for the wrong reason.
//
// This helper takes the single input (guildId) and produces the same
// shape the real config module exports. A new derived field added to
// config.js would be added here once, and every test suite that uses
// this helper picks it up automatically.
//
// Usage:
//   const { buildConfigMock } = require('./helpers/buildConfigMock');
//   jest.mock('../src/config', () => ({
//     ...jest.requireActual('./helpers/buildConfigMock').buildConfigMock({
//       guildId: 'guild-1',
//     }),
//     // test-specific overrides
//     BASE_URL: 'https://bot.example.com',
//     ...
//   }));

function buildConfigMock({ guildId = null } = {}) {
  const normalizedGuildId = guildId || null;
  const isMultiTenant = !normalizedGuildId;

  return {
    GUILD_ID: normalizedGuildId,
    isMultiTenant,
  };
}

module.exports = { buildConfigMock };
