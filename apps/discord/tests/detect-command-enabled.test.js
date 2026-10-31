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


describe('qURL command data builder', () => {
  test.each([
    [false, false], [false, true], [true, false], [true, true],
  ])('uses explicit map=%s and detect=%s flags', (map, detect) => {
    withFreshEnv({ MAP_COMMAND_ENABLED: String(!map), DETECT_COMMAND_ENABLED: String(!detect) }, () => {
      const { _test } = require('../src/commands');
      const cfg = { MAP_COMMAND_ENABLED: map, DETECT_COMMAND_ENABLED: detect };
      const data = _test.buildQurlCommandData(cfg).toJSON();
      expect(data.name).toBe('qurl');
      expect(data.options.map(option => option.name)).toEqual([
        'send', ...(detect ? ['detect'] : []), ...(map ? ['map'] : []),
        'revoke', 'help', 'setup', 'status',
      ]);
      const send = data.options.find(option => option.name === 'send');
      expect(send.options.map(option => option.name))
        .toEqual(['attachment', 'recipients', 'expires-in', 'self-destruct', 'personal-message']);
      expect(send.options[0]).toMatchObject({ name: 'attachment', type: 11, required: true });
      if (map) {
        const location = data.options.find(option => option.name === 'map');
        expect(location.options.map(option => option.name))
          .toEqual(['location', 'recipients', 'location-name', 'expires-in', 'self-destruct', 'personal-message']);
        expect(location.options[0]).toMatchObject({ name: 'location', type: 3, required: true, autocomplete: true });
        const shared = ['recipients', 'expires-in', 'self-destruct', 'personal-message'];
        expect(location.options.filter(option => shared.includes(option.name)))
          .toEqual(send.options.filter(option => shared.includes(option.name)));
      }
      expect(_test.buildQurlCommandData(cfg).toJSON()).toEqual(data);
    });
  });
});
