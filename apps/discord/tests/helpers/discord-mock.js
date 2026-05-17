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

// Component-builder chainable — superset surface covering
// ButtonBuilder, StringSelectMenuBuilder, UserSelectMenuBuilder,
// ModalBuilder, TextInputBuilder. Each method returns `this` so the
// discord.js builder DSL works unchanged. Unused methods on the
// chainable are inert — tests can reuse this for any component
// without per-component shaping.
//
// commands-comprehensive.test.js and coverage-boost.test.js still
// inline narrower per-component shapes for historical reasons; they
// can migrate to this superset whenever a discord.js change
// surfaces a setMaxLength-style drift gap for components.
function makeComponentChainable(extra = {}) {
  return {
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setEmoji: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
    setTitle: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    addOptions: jest.fn().mockReturnThis(),
    setMinValues: jest.fn().mockReturnThis(),
    setMaxValues: jest.fn().mockReturnThis(),
    setDefaultValues: jest.fn().mockReturnThis(),
    addDefaultUsers: jest.fn().mockReturnThis(),
    addComponents: jest.fn().mockReturnThis(),
    setDisabled: jest.fn().mockReturnThis(),
    setValue: jest.fn().mockReturnThis(),
    setMaxLength: jest.fn().mockReturnThis(),
    setMinLength: jest.fn().mockReturnThis(),
    setRequired: jest.fn().mockReturnThis(),
    ...extra,
  };
}

module.exports = {
  makeOptionBuilder,
  makeComponentChainable,
};
