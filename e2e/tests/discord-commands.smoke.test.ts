import { loadEnv } from '../helpers/env';
import { api } from '../helpers/discord-api';

// Discord application-command option type for a subcommand.
// https://discord.com/developers/docs/interactions/application-commands#application-command-object-application-command-option-type
const SUBCOMMAND_OPTION_TYPE = 1;

// Discord application-command type for a chat-input (slash) command.
// https://discord.com/developers/docs/interactions/application-commands#application-command-object-application-command-types
const CHAT_INPUT_COMMAND_TYPE = 1;

interface ApplicationCommand {
  name: string;
  type?: number;
  options?: Array<{ type: number; name: string }>;
}

describe('Discord command registration (smoke)', () => {
  const env = loadEnv();
  // Shared across both assertions so we only hit the Discord API twice per
  // run instead of four times (halves rate-limit exposure).
  let registrations: ApplicationCommand[];

  beforeAll(async () => {
    // Union of globally-registered and guild-scoped `/qurl` commands.
    // The bot registers `/qurl` globally in multi-tenant mode and
    // guild-scoped in OpenNHP mode (see commands.js's registerCommands).
    // Fetching both scopes makes the assertion robust across deploy
    // configurations and catches ghost registrations in whichever scope
    // we're not actively using.
    //
    // The guild fetch is tolerated failing: if the bot isn't in the
    // configured GUILD_ID (403/404), treat it as "no guild-scoped
    // registrations" and fall through to global-only. Any other class
    // of failure (auth, network) still propagates out of beforeAll and
    // fails the suite loudly.
    const [globalCommands, guildCommands] = await Promise.all([
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/commands`) as Promise<ApplicationCommand[]>,
      (api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/guilds/${env.GUILD_ID}/commands`) as Promise<ApplicationCommand[]>)
        .catch((err: Error) => {
          if (/\b(403|404)\b/.test(err.message)) return [] as ApplicationCommand[];
          throw err;
        }),
    ]);
    registrations = [
      ...globalCommands.filter((c) => c.name === 'qurl'),
      ...guildCommands.filter((c) => c.name === 'qurl'),
    ];

    // Fail the whole suite loudly if Discord returned no /qurl registrations
    // at all. Without this guard the ghost-subcommand test below would
    // trivially pass on an empty array — green CI, false confidence.
    if (registrations.length === 0) {
      throw new Error(
        `No /qurl command registrations found in either global or guild scope for app ${env.BOT_CLIENT_ID}. ` +
        `Either the bot never called commands.set() on boot, or the Discord API returned an empty response. ` +
        `Check bot logs for a "Slash commands registered." line on the latest deploy.`,
      );
    }
  });

  it('exposes the expected /qurl subcommand set as a chat-input command', () => {
    for (const qurl of registrations) {
      // Guard against accidental re-registration as a user- or
      // message-context command. `type` is absent in Discord's older
      // response shape and defaults to CHAT_INPUT — accept either.
      expect(qurl.type ?? CHAT_INPUT_COMMAND_TYPE).toBe(CHAT_INPUT_COMMAND_TYPE);

      const subcommands = (qurl.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name)
        .sort();
      expect(subcommands).toEqual(['help', 'revoke', 'send', 'setup', 'status']);
    }
  });

  it('has no ghost subcommands from the Python-bot era', () => {
    // Intentional overlap with the exact-set assertion above: that test
    // is the correctness invariant, this one is living documentation
    // ("`list` and `clear` used to exist, must never return"). If the
    // exact-set check is ever loosened to allow additional subcommands,
    // this guard still catches the Kevin-report failure class.
    //
    // Both subcommands were the Python bot's catalog-era commands,
    // removed when the bot was rewritten in Node.js.
    const allSubcommandNames = registrations.flatMap((q) =>
      (q.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name),
    );
    expect(allSubcommandNames).not.toContain('list');
    expect(allSubcommandNames).not.toContain('clear');
  });
});
