const {
  isUnsupportedQurlContext,
  UNSUPPORTED_CONTEXT_MSG,
} = require('../src/interaction-context');

describe('Discord qURL interaction context boundary', () => {
  test.each([
    ['legacy guild payload', { guildId: 'guild-1' }, false],
    ['empty owners map', { guildId: 'guild-1', authorizingIntegrationOwners: {} }, false],
    ['guild install', { guildId: 'guild-1', authorizingIntegrationOwners: { 0: 'guild-1' } }, false],
    ['dual install', { guildId: 'guild-1', authorizingIntegrationOwners: { 0: 'guild-1', 1: 'user-1' } }, false],
    ['user install only', { guildId: 'guild-1', authorizingIntegrationOwners: { 1: 'user-1' } }, true],
    ['DM', { guildId: null }, true],
    ['guild-owned app DM sentinel', { guildId: null, authorizingIntegrationOwners: { 0: '0' } }, true],
    ['future numeric guild sentinel', { guildId: 'guild-1', authorizingIntegrationOwners: { 0: 0, 1: 'user-1' } }, false],
  ])('%s', (_name, interaction, expected) => {
    expect(isUnsupportedQurlContext(interaction)).toBe(expected);
  });

  test('uses wording that is actionable from both a DM and a guild', () => {
    expect(UNSUPPORTED_CONTEXT_MSG).toBe(
      'qURL only works inside a server where it is installed, not in DMs or from a user install.',
    );
  });
});
