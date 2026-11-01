const { withFreshEnv } = require('./helpers/fresh-config');

function loadCommands(guildId) {
  let loaded;
  withFreshEnv({ GUILD_ID: guildId, PREVIOUS_COMMAND_SCOPE: undefined }, () => {
    loaded = require('../src/commands');
  });
  return loaded;
}

test.each([undefined, '123456789012345678'])('registration separates admin actions in scope %s', async guildId => {
  const loaded = loadCommands(guildId);
  const rest = { get: jest.fn().mockResolvedValue([]), put: jest.fn().mockResolvedValue(undefined) };
  await loaded.registerCommands({ rest, appId: 'app-123' });
  const data = rest.put.mock.calls[0][1].body;
  expect(data.map(command => command.name).sort()).toEqual(['qurl', 'qurl-admin']);
  const user = data.find(command => command.name === 'qurl');
  expect(user.options.map(option => option.name)).not.toContain('setup');
  expect(user.options.map(option => option.name)).not.toContain('status');
  const admin = data.find(command => command.name === 'qurl-admin');
  expect(admin.default_member_permissions).toBe('32');
  if (guildId) {
    expect(admin.integration_types).toBeUndefined();
    expect(admin.contexts).toBeUndefined();
  } else {
    expect(admin).toMatchObject({ integration_types: [0], contexts: [0] });
  }
  expect(admin.options.map(option => option.name)).toEqual(['setup', 'status']);
  expect(user.default_member_permissions ?? null).toBeNull();
});

test.each(['setup', 'status'])('admin %s retains runtime permission checks, including cached aliases', async sub => {
  const loaded = loadCommands();
  for (const name of ['qurl-admin', 'qurl']) {
    const command = loaded.commands.find(item => item.data.name === name);
    expect(command).toBeDefined();
    const interaction = {
      commandName: name,
      guildId: '123456789012345678',
      options: { getSubcommand: () => sub },
      memberPermissions: { has: () => false },
      reply: jest.fn().mockResolvedValue(undefined),
    };
    await command.execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Only server administrators/), flags: 64,
    }));
  }
});

test('unknown admin actions never dispatch a user command', async () => {
  const loaded = loadCommands();
  const userCommand = loaded.commands.find(command => command.data.name === 'qurl');
  const dispatch = jest.spyOn(userCommand, 'execute');
  try {
    const interaction = {
      options: { getSubcommand: () => 'send' },
      reply: jest.fn().mockResolvedValue(undefined),
    };
    await loaded.commands.find(command => command.data.name === 'qurl-admin').execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith({ content: 'Unknown qURL admin command.', flags: 64 });
    expect(dispatch).not.toHaveBeenCalled();
  } finally {
    dispatch.mockRestore();
  }
});
