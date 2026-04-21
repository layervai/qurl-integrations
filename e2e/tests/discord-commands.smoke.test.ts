import { loadEnv } from '../helpers/env';
import { api } from '../helpers/discord-api';

// Discord API option type for a subcommand.
// https://discord.com/developers/docs/interactions/application-commands#application-command-object-application-command-option-type
const SUBCOMMAND_OPTION_TYPE = 1;

describe('Discord command registration (smoke)', () => {
  const env = loadEnv();

  // Union of globally-registered and guild-scoped `/qurl` commands.
  // The bot registers `/qurl` globally in multi-tenant mode and
  // guild-scoped in OpenNHP mode (see commands.js's registerCommands).
  // Querying both scopes makes the assertion robust across deploy
  // configurations and also catches ghost guild-scoped registrations
  // left over from a prior mode.
  async function fetchAllQurlRegistrations(): Promise<Array<{ options?: Array<{ type: number; name: string }> }>> {
    const [globalCommands, guildCommands] = await Promise.all([
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/commands`),
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/guilds/${env.GUILD_ID}/commands`),
    ]);
    return [
      ...globalCommands.filter((c: { name: string }) => c.name === 'qurl'),
      ...guildCommands.filter((c: { name: string }) => c.name === 'qurl'),
    ];
  }

  it('exposes the expected /qurl subcommand set', async () => {
    const registrations = await fetchAllQurlRegistrations();

    expect(registrations.length).toBeGreaterThan(0);

    // A clean deploy has exactly one registration; if both scopes have
    // it (transient state during a mode flip), each must expose the
    // same subcommand set.
    for (const qurl of registrations) {
      const subcommands = (qurl.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name)
        .sort();
      expect(subcommands).toEqual(['help', 'revoke', 'send', 'setup', 'status'].sort());
    }
  });

  it('has no ghost subcommands from the pre-Node.js-migration era', async () => {
    // `list` and `clear` were the Python bot's catalog-style subcommands,
    // removed when the bot was rewritten (commit 2e336d6). If they come
    // back in either scope, command registration has regressed — same
    // failure class that produced "The application did not respond" on
    // qurl-test-bot-playground in the pre-flip state.
    const registrations = await fetchAllQurlRegistrations();
    const allSubcommandNames = registrations.flatMap((q) =>
      (q.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name),
    );
    expect(allSubcommandNames).not.toContain('list');
    expect(allSubcommandNames).not.toContain('clear');
  });
});
