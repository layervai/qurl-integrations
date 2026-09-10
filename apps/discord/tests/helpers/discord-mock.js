
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
