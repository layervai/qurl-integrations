const { withFreshEnv } = require('./helpers/fresh-config');

describe('/qurl detect activation', () => {
  test.each([
    [undefined, false],
    ['true', true],
    ['false', false],
    ['', false],
    ['TRUE', false],
    ['1', false],
  ])('flag %p registers and routes detect with enabled=%p', async (flag, enabled) => {
    let commands;
    withFreshEnv({ DETECT_COMMAND_ENABLED: flag }, () => {
      expect(require('../src/config').DETECT_COMMAND_ENABLED).toBe(enabled);
      commands = require('../src/commands');
    });
    const rest = { get: jest.fn().mockResolvedValue([]), put: jest.fn() };
    await commands.registerCommands({ rest, appId: 'app-123' });
    const registered = rest.put.mock.calls.at(-1)[1].body.find(c => c.name === 'qurl');
    const detect = registered.options.find(option => option.name === 'detect');
    if (enabled) {
      expect(detect).toMatchObject({
        type: 1,
        options: [{ name: 'image', type: 11, required: true }],
      });
    } else {
      expect(detect).toBeUndefined();
    }

    // A cached submission must also respect the switch. When enabled, it
    // reaches the handler's attachment validation without making a request.
    const interaction = {
      guildId: '123456789012345678',
      user: { id: '223456789012345678' },
      options: {
        getSubcommand: () => 'detect',
        getAttachment: jest.fn(() => { throw new Error('Missing required image'); }),
      },
      reply: jest.fn(),
    };
    await commands.commands.find(c => c.data.name === 'qurl').execute(interaction);
    expect(interaction.options.getAttachment).toHaveBeenCalledTimes(enabled ? 1 : 0);
    expect(interaction.reply).toHaveBeenCalledWith({
      content: enabled
        ? '❌ The `image:` option is required. Re-run with an image attached.'
        : commands._test.QURL_DETECT_DISABLED_REPLY,
      ephemeral: true,
    });
  });
});
