/**
 * Shared discord.js mock factory helpers for jest tests.
 *
 * The SlashCommandBuilder + option-builder chainables are the dominant
 * mock surface that grows whenever a new chained method appears at the
 * discord.js layer (setMaxLength, addChoices, setAutocomplete, etc.).
 * Without a shared factory, each test file that mocks discord.js had
 * to be touched in lockstep — which silently breaks at module-load
 * (e.g. commands-comprehensive + coverage-boost both regressed when
 * PR #301 added setMaxLength).
 *
 * Tests opt in by importing these factories inside their own
 * `jest.mock('discord.js', () => ({ ... }))` factory function.
 */

// Option-builder chainable used by addStringOption / addAttachmentOption
// / addUserOption / addIntegerOption. Every chainable method returns
// itself so the discord.js builder DSL works unchanged.
function makeOptionBuilder() {
  return {
    setName: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(),
    setRequired: jest.fn().mockReturnThis(),
    setMaxLength: jest.fn().mockReturnThis(),
    setMinLength: jest.fn().mockReturnThis(),
    addChoices: jest.fn().mockReturnThis(),
    setAutocomplete: jest.fn().mockReturnThis(),
  };
}

module.exports = {
  makeOptionBuilder,
};
