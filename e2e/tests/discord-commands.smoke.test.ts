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

// Tag every fetched registration with the scope it came from. Guild-
// scoped ghost registrations (the Kevin-playground failure class) are
// the most interesting thing this test catches, and knowing WHICH scope
// leaked a stale subcommand starts failure triage at "guild scope has
// stale /qurl list" instead of "something, somewhere, has /qurl list".
type ScopedCommand = ApplicationCommand & { _scope: 'global' | 'guild' };

describe('Discord command registration (smoke)', () => {
  const env = loadEnv();
  // Shared across both assertions so we only hit the Discord API twice per
  // run instead of four times (halves rate-limit exposure). Initialized
  // to `[]` so the type doesn't lie during the pre-beforeAll window.
  let registrations: ScopedCommand[] = [];

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
    //
    // NOTE on propagation: per Discord's docs, GLOBAL application
    // commands can take up to ~1 hour to propagate after a fresh
    // `commands.set()` call. Guild-scoped ones are effectively instant.
    // If this test fails right after a deploy that ADDS/RENAMES a
    // subcommand in multi-tenant mode, it may be the propagation window
    // rather than a real regression — wait ~5–10 min and re-run before
    // debugging. A removal-only change doesn't have this issue because
    // Discord reflects the new set via this API immediately; only the
    // user-visible autocomplete cache is what takes up to an hour.
    const [globalCommands, guildCommands] = await Promise.all([
      api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/commands`) as Promise<ApplicationCommand[]>,
      (api(env.BOT_TOKEN, 'GET', `/applications/${env.BOT_CLIENT_ID}/guilds/${env.GUILD_ID}/commands`) as Promise<ApplicationCommand[]>)
        .catch((err: Error) => {
          // Match the exact error-message format from `helpers/discord-api.ts`:
          //   `Discord API GET /path: ${status} ${body}`
          // Matching `: <status> ` (with space-separator on both sides) avoids
          // false-matching 403/404 that appear inside the response `${body}`
          // as an unrelated snowflake ID or Discord error code.
          if (/:\s(403|404)\s/.test(err.message)) return [] as ApplicationCommand[];
          throw err;
        }),
    ]);
    registrations = [
      ...globalCommands
        .filter((c) => c.name === 'qurl')
        .map((c) => ({ ...c, _scope: 'global' as const })),
      ...guildCommands
        .filter((c) => c.name === 'qurl')
        .map((c) => ({ ...c, _scope: 'guild' as const })),
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

  it('every /qurl registration exposes the expected chat-input subcommand set', () => {
    for (const qurl of registrations) {
      // Guard against accidental re-registration as a user- or
      // message-context command. `type` is absent in Discord's older
      // response shape and defaults to CHAT_INPUT — accept either.
      expect(qurl.type ?? CHAT_INPUT_COMMAND_TYPE).toBe(CHAT_INPUT_COMMAND_TYPE);

      const subcommands = (qurl.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name)
        .sort();
      // Prepend scope to the expectation so a mismatch surfaces which
      // scope regressed in the Jest failure header.
      expect({ scope: qurl._scope, subcommands })
        .toEqual({ scope: qurl._scope, subcommands: ['help', 'revoke', 'send', 'setup', 'status'] });
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
    for (const qurl of registrations) {
      const subcommandNames = (qurl.options ?? [])
        .filter((o) => o.type === SUBCOMMAND_OPTION_TYPE)
        .map((o) => o.name);
      // Assert each ghost name individually. `expect.not.arrayContaining(subset)`
      // is the negation of "contains ALL of subset", so a single-ghost
      // leak (only `list` or only `clear`) would slip past a combined
      // `.not.arrayContaining(['list', 'clear'])`. Individual `.not.toContain`
      // is unambiguous per-name. Scope context is preserved by the
      // positive-set test's failure header, which fires first on any
      // regression that also changes the full set.
      expect(subcommandNames).not.toContain('list');
      expect(subcommandNames).not.toContain('clear');
    }
  });
});
