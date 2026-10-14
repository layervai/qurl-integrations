# qURL plugin for Claude Code

Share an app running on your machine privately, straight from Claude Code, with
qURL™: no deploy, no account, no API key, no signup. Install the plugin with two
commands in a Claude Code session:

```text
/plugin marketplace add layervai/qurl-integrations
/plugin install qurl@layerv
```

Then ask Claude to share what you're working on ("share my app with
ana@example.com"), or use the commands below. The first time it shares, Claude
installs the `qurl` CLI for you.

## What it adds

| Command | What it does |
|---|---|
| `qurl` skill (`/qurl:qurl`) | The qURL agent skill. Claude uses it on its own whenever you want to share, publish or open something with qURL. |
| `/qurl:share [who]` | Share the app you're working on privately. Asks who it's for. |
| `/qurl:approve <code> [CRID]` | Let in a person who asked for access, by the six-digit code they gave you. Never by name. |

The two commands load only when you type them, so they add almost nothing to a
session's context.

## Updates

Claude Code installs a new copy only when the plugin's version changes. Third-party
marketplaces don't update on their own by default: run
`/plugin marketplace update layerv`, or turn on auto-update for `layerv` under
**Marketplaces** in `/plugin`.

## Keeping the bundled skill current (maintainers)

`skills/qurl/SKILL.md` is a copy of the skill served at
<https://layerv.ai/skills/qurl/SKILL.md>, byte for byte. The served file is the
source of truth: never edit the copy here. The `claude-code-plugin: Bundled
Skill` workflow compares the two every day and fails while they differ. To
update it:

1. Copy the served file over the bundled one:

   ```bash
   curl -fsSL https://layerv.ai/skills/qurl/SKILL.md -o plugins/claude-code/qurl/skills/qurl/SKILL.md
   go run ./apps/cli/scripts/check-served-skill -bundled plugins/claude-code/qurl/skills/qurl/SKILL.md
   ```

2. Raise `version` in `.claude-plugin/plugin.json` in the same PR, or people who
   installed the plugin keep the old copy. Use a patch version for a new copy of
   the skill or a wording change, a minor version for a new command, and a major
   version when a command is removed or renamed. The version lives only in
   `plugin.json`, never in the marketplace entry.
3. If the skill's rules changed, check that `/qurl:share` and `/qurl:approve`
   (in `skills/share` and `skills/approve`) still agree with them.
4. Run `claude plugin validate --strict .` from the repository root and
   `go test ./internal/ciworkflows/...`.

Any other change to the plugin's files also needs a new version to reach people.
