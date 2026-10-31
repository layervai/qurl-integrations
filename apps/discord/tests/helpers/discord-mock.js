
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

function buildDiscordMock(embedInstances = []) {
  const makeEmbed = () => {
    const embed = {
      setColor: jest.fn().mockReturnThis(),
      setTitle: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      setAuthor: jest.fn().mockReturnThis(),
      addFields: jest.fn().mockReturnThis(),
      setFooter: jest.fn().mockReturnThis(),
      setTimestamp: jest.fn().mockReturnThis(),
      setThumbnail: jest.fn().mockReturnThis(),
      setURL: jest.fn().mockReturnThis(),
      _fields: [],
    };
    embed.addFields.mockImplementation(function (...args) {
      embed._fields.push(...args);
      return embed;
    });
    embedInstances.push(embed);
    return embed;
  };
  return {
    SlashCommandBuilder: jest.fn().mockImplementation(() => {
      const subBuilder = () => ({
        setName: jest.fn().mockReturnThis(),
        setDescription: jest.fn().mockReturnThis(),
        addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
      });
      const builder = {
        setName: jest.fn(function (n) { builder.name = n; return builder; }),
        setDescription: jest.fn().mockReturnThis(),
        addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
        addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
        addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
        addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
        addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
        setDefaultMemberPermissions: jest.fn().mockReturnThis(),
        toJSON: jest.fn().mockReturnValue({}),
      };
      return builder;
    }),
    EmbedBuilder: jest.fn().mockImplementation(makeEmbed),
    PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n },
    ActionRowBuilder: jest.fn().mockImplementation(() => {
      const row = { components: [], addComponents: jest.fn(function (...args) {
        row.components.push(...args.flat());
        return row;
      }) };
      return row;
    }),
    ButtonBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
      setEmoji: jest.fn().mockReturnThis(),
      setStyle: jest.fn().mockReturnThis(),
      setURL: jest.fn().mockReturnThis(),
    })),
    ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
    ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
    ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
    StringSelectMenuBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
      addOptions: jest.fn().mockReturnThis(),
    })),
    UserSelectMenuBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
      setMinValues: jest.fn().mockReturnThis(),
      setMaxValues: jest.fn().mockReturnThis(),
      setDefaultValues: jest.fn().mockReturnThis(),
      addDefaultUsers: jest.fn().mockReturnThis(),
    })),
    MentionableSelectMenuBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
    ModalBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setTitle: jest.fn().mockReturnThis(),
      addComponents: jest.fn().mockReturnThis(),
    })),
    TextInputBuilder: jest.fn().mockImplementation(() => ({
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
      setStyle: jest.fn().mockReturnThis(),
      setMaxLength: jest.fn().mockReturnThis(),
      setMinLength: jest.fn().mockReturnThis(),
      setRequired: jest.fn().mockReturnThis(),
    })),
    TextInputStyle: { Short: 1, Paragraph: 2 },
  };
}

function makeInteraction(overrides = {}) {
  const base = {
    user: { id: 'user-1', username: 'TestUser' },
    options: {
      getSubcommand: jest.fn(() => 'send'),
      getString: jest.fn(() => null),
      getUser: jest.fn(() => null),
      getAttachment: jest.fn(() => null),
      getInteger: jest.fn(() => null),
      getFocused: jest.fn(() => ({ name: 'location', value: '' })),
    },
    reply: jest.fn().mockResolvedValue({
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      createMessageComponentCollector: jest.fn(() => ({ on: jest.fn() })),
    }),
    editReply: jest.fn().mockResolvedValue(undefined),
    deferReply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    channel: {
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      members: new Map(),
    },
    channelId: 'ch-1',
    guild: {
      members: { fetch: jest.fn().mockResolvedValue(undefined) },
      voiceStates: { cache: new Map() },
    },
    replied: false,
    deferred: false,
    isChatInputCommand: jest.fn(() => true),
    isAutocomplete: jest.fn(() => false),
    commandName: 'qurl',
    respond: jest.fn().mockResolvedValue(undefined),
  };
  return { ...base, ...overrides };
}

module.exports = {
  buildDiscordMock,
  makeInteraction,
  makeOptionBuilder,
  makeComponentChainable,
};
