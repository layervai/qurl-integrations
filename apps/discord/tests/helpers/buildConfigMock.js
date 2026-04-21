// Test-side reconstitution of the three-mode derivation that lives in
// src/config.js. Each test file that does `jest.mock('../src/config', ...)`
// previously had to hard-code all three derived fields
// (`isMultiTenant`, `ENABLE_OPENNHP_FEATURES`, `isOpenNHPActive`) in
// lockstep — if any one drifted, tests passed for the wrong reason.
//
// This helper takes the two inputs (guildId, enableOpenNHP) and
// produces the same shape the real config module exports. A 4th
// derived field added to config.js would be added here once, and
// every test suite that uses this helper picks up the new field
// automatically.
//
// Usage:
//   const { buildConfigMock } = require('./helpers/buildConfigMock');
//   jest.mock('../src/config', () => ({
//     ...jest.requireActual('./helpers/buildConfigMock').buildConfigMock({
//       guildId: 'guild-1',
//       enableOpenNHP: true,
//     }),
//     // test-specific overrides (role-name strings, thresholds, etc.)
//     CONTRIBUTOR_ROLE_NAME: 'Contributor',
//     ...
//   }));

function buildConfigMock({ guildId = null, enableOpenNHP = false } = {}) {
  const normalizedGuildId = guildId || null;
  const isMultiTenant = !normalizedGuildId;
  const isOpenNHPActive = !isMultiTenant && enableOpenNHP === true;

  return {
    GUILD_ID: normalizedGuildId,
    isMultiTenant,
    ENABLE_OPENNHP_FEATURES: enableOpenNHP === true,
    isOpenNHPActive,
  };
}

module.exports = { buildConfigMock };
