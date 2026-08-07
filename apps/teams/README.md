# qURL for Microsoft Teams

Share internal resources from Microsoft Teams as secure, one-time links
without leaving the conversation. This Teams bot intentionally tracks the
Slack app's qURL workflows as closely as the platform allows; the main
differences are the Teams message surface, personal-chat delivery model, and
Bot Framework auth.

Examples below omit the leading bot mention for brevity. In a Teams channel
you will usually send them as `@qurl ...`, for example `@qurl get $docs`.
The bot also accepts optional `qurl` and `qurl-admin` prefixes in message text.

## Quickstart

1. **Install** the bot into the target Teams tenant and add it to the channel
   where you want to share qURLs.
2. **Open a personal chat** with the bot once. Teams uses that personal chat
   for `dm:true` delivery and qURL Connector bootstrap keys.
3. **Connect** qURL to the tenant: in a channel, send
   `@qurl setup you@company.com`. The first person to complete setup becomes
   the tenant **owner**.
4. **Protect a resource** (admins): run `@qurl protect-url ...` or
   `@qurl protect-connector ...` in the channel.
5. **Share a link**: anyone in that channel can run `@qurl get $id`
   to mint a one-time qURL.

Run `@qurl help` any time for the exact Teams syntax this deployment supports.

## Concepts

- **Resource**: something you can mint links for. Two kinds are supported
  today: an existing **URL resource** or a **qURL Connector** resource that
  fronts a service running in your environment.
- **`$id`**: a resource identifier. Pass it to `get`, `set-display-name`,
  `revoke`, and related commands.
- **Alias**: a channel-local name for a resource. Use an alias anywhere you
  would use a `$id`.
- **Channel scope**: resources are available per Teams channel. `list`,
  `aliases`, and `get` all reflect the current channel's allow-list.
- **Owner and admins**: the owner is whoever first completed `setup`. The
  owner and admins can run the admin-only commands.

## Commands

### Everyone

| Command | What it does |
|---------|--------------|
| `setup <email>` | Connect qURL to this Teams tenant. The first person to complete setup becomes the owner. |
| `setup <email> --rotate` | Owner-only: revoke the stored tenant key and replace it with a fresh one on the same qURL account. |
| `setup <email> --repoint` | Owner-only: move the tenant to a different qURL account. Cross-account repoints fail closed and require operator help. |
| `get <$id\|$alias>` | Mint a one-time qURL for a resource available in this channel. |
| `get <$id\|$alias> dm:true` | Mint the qURL and send it to your personal Teams chat instead of the channel thread. |
| `get <$id\|$alias> reason:"..."` | Mint the qURL and record a reason in the qURL audit trail. |
| `list` | List the resources available in this channel. |
| `aliases` | List the channel-local aliases in this channel. |
| `feedback <message>` | Send a bug report or feature request to the qURL team. |
| `uninstall` | Owner/admin-gated: disconnect qURL from this Teams tenant and purge local channel policy state. |
| `help` | Show the Teams command help. |

### Admins

| Command | What it does |
|---------|--------------|
| `protect-url url:https://internal.example.com as:$docs` | Protect an existing URL and bind it into the current channel. |
| `protect-url $resource-id as:$docs` | Reuse an existing tenant resource in the current channel under a channel-local alias. |
| `protect-url $docs` | Re-expose an existing URL resource in this channel when it already has a reusable alias or slug. |
| `protect-connector prod-dashboard [alias:$dash] [env:docker\|compose\|ecs-fargate\|kubernetes] [port:8080] [service:web]` | Protect a qURL Connector resource and DM the bootstrap key plus install steps to your personal chat. |
| `set-alias $alias $resource-id` | Point an alias at a resource in this channel. |
| `unset-alias $alias` | Remove an alias from this channel. |
| `set-display-name $resource-id Friendly name` | Set the friendly name shown by `list`. |
| `unset-display-name $resource-id` | Reset the resource display name. |
| `revoke $resource-id` | Revoke the resource and remove it from every channel policy in this tenant. |
| `add @user` | Promote a Teams user to admin for this tenant. |
| `remove @user` | Demote a Teams user from admin for this tenant. |
| `admins` | List the current owner and admins. |

## Protecting a resource

Teams currently supports the typed command path rather than Slack-style modals.
Run these commands in the channel where the resource should become visible.

**URL resource**: use `protect-url url:<target> as:$alias` to create or
find the URL resource and expose it in the current channel.

**qURL Connector**: use `protect-connector <slug> ...` to create or reuse a
connector resource, expose it in the current channel, and receive a short-lived
bootstrap key in your personal Teams chat. The bootstrap key currently expires
after 15 minutes; remove it from the runtime once the connector is online.

## Sharing a link

`get $id` mints a one-time qURL for any resource available in the current
channel. Add `dm:true` to send the link to your personal Teams chat, or
`reason:"..."` to annotate the mint in the audit log.

## Teams-specific behavior

- Channel-scoped commands must run in a Teams channel. Personal chat is used
  only for setup confirmation, `dm:true`, and connector bootstrap delivery.
- Teams does not have Slack-style ephemeral responses for slash commands. When
  you request private delivery, the bot sends the qURL to your personal chat.
- The bot stores one qURL API key per Teams tenant as `teams:<tenant-id>` in
  `workspace_state`.
- The bot stores a personal conversation reference the first time you message
  it in personal chat; without that reference, `dm:true` and
  `protect-connector` fail closed.

## FAQ

**"qURL isn't connected to this tenant yet."** Someone needs to run
`setup <email>` first. The first successful setup becomes the tenant owner.

**`dm:true` says private delivery is not ready.** Open a personal chat with the
bot once, send any message, then retry the command in the channel.

**Why did `setup --repoint` fail?** Teams refuses silent cross-account moves.
If the stored key belongs to a different qURL account, the bot stops and asks
for operator-assisted transfer rather than leaving multiple live keys behind.

**Why does `protect-url url:<target>` require `as:$alias`?** Teams has no Slack
modal to collect follow-up fields interactively, so the channel alias must be
present in the command.

## Self-hosting

Running the Secure Access Agent yourself? See [docs/operating.md](docs/operating.md)
for endpoints, environment variables, OAuth wiring, and local development.

## Docs

- Operating guide: [docs/operating.md](docs/operating.md)

## License

[MIT](../../LICENSE) — Copyright (c) 2025-present LayerV, Inc.
