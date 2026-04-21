import { loadEnv } from '../helpers/env';
import { api } from '../helpers/discord-api';

// Discord application-command option type for a subcommand.
// https://discord.com/developers/docs/interactions/application-commands#application-command-object-application-command-option-type
const SUBCOMMAND_OPTION_TYPE = 1;

// Discord application-command type for a chat-input (slash) command.
// https://discord.com/developers/docs/interactions/application-commands#application-command-object-application-command-types
const CHAT_INPUT_COMMAND_TYPE = 1;

interface QurlRegistration {
  type?: number;
  options?: Array<{ type: number; name: string }>;
}

describe('Discord command registration (smoke)', () => {
  const env = loadEnv();
  // Shared across both assertions so we only hit the Discord API twice per
  // run instead of four times (halves rate-limit exposure).
  let registrations: QurlRegistration[];

  beforeAll(async () => {
    // Union of globally-registered and guild-scoped `/qurl` commands.
    // The bot registers `/qurl` globally in multi-tenant mode and
    // guild-scoped in OpenNHP mode (see commands.js's registerCommands).
    // Fetching both scopes makes the assertion robust across deploy
    // configurations and catches ghost registrations in whichever scope
    // we're not actively using.
    const [globalCommands, guildCommands] = await Promise.all([
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/commands`),
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/guilds/${env.GUILD_ID}/commands`),
    ]);
    registrations = [
      ...globalCommands.filter((c: { name: string }) => c.name === 'qurl'),
      ...guildCommands.filter((c: { name: string }) => c.name === 'qurl'),
    ];
  });

  it('exposes the expected /qurl subcommand set as a chat-input command', () => {
    expect(registrations.length).toBeGreaterThan(0);

    for (const qurl of registrations) {
      // Guard against accidental re-registration as a user- or
      // message-context command. `type` is absent in Discord's older
      // response shape and defaults to CHAT_INPUT — accept either.
      expect(qurl.type ?? CHAT_INPUT_COMMAND_TYPE).toBe(CHAT_INPUT_COMMAND_TYPE);

      const subcommands = (qurl.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name)
        .sort();
      expect(subcommands).toEqual(['help', 'revoke', 'send', 'setup', 'status'].sort());
    }
  });

  it('has no ghost subcommands from the pre-Node.js-migration era', () => {
    // Intentional overlap with the exact-set assertion above: that test
    // is the correctness invariant, this one is living documentation
    // ("`list` and `clear` used to exist, must never return"). If the
    // exact-set check is ever loosened to allow additional subcommands,
    // this guard still catches the Kevin-report failure class.
    //
    // Both subcommands were the Python bot's catalog-era commands,
    // removed in commit 2e336d6 when the bot was rewritten in Node.js.
    const allSubcommandNames = registrations.flatMap((q) =>
      (q.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name),
    );
    expect(allSubcommandNames).not.toContain('list');
    expect(allSubcommandNames).not.toContain('clear');
  });
});
