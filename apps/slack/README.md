# qURL for Slack

Share internal resources from Slack as secure, one-time links — without
leaving the channel. Admins protect a resource once; anyone in the channel
mints a fresh, expiring link with a single slash command.

A **qURL™** is a one-time-use access link: it works for the first person who
opens it, then burns. Links also expire on their own, so nothing stays live
longer than it needs to.

## Quickstart

1. **Install** the qURL app into your workspace from the install link your
   qURL operator gave you.
2. **Connect** qURL to the workspace: run `/qurl setup you@company.com` in
   Slack. The first person to run this becomes the workspace **owner**.
3. **Protect a resource** (admins): run `/qurl-admin protect` and follow the
   guided form to expose a service or an existing URL in the current channel.
4. **Share a link**: anyone in that channel runs `/qurl get $id`
   to mint a one-time qURL.

Run `/qurl help` (or `/qurl-admin help`) any time for the exact commands your
workspace's Secure Access Agent supports.

## Concepts

- **Resource** — something you can mint links for. Two kinds: a **qURL
  Connector** (fronts a service running in your own environment) or a **URL
  resource** (an existing web URL). Admins create resources with the
  `/qurl-admin protect…` commands.
- **`$id`** — a resource's identifier. Pass it to `/qurl get` to mint a link.
- **Alias** — an alternate name for a resource within a channel. Several
  aliases can point at one resource. Use an alias anywhere you'd use a `$id`.
- **Channel scope** — resources are available per channel. A resource shows up
  only in the channels it's been protected in. `/qurl list`, `/qurl aliases`,
  and `/qurl get` all agree on what's available in a given channel — including
  for admins. For why this scoping is a confidentiality boundary and how it
  fails closed (e.g. in DMs), see [docs/list-disclosure.md](docs/list-disclosure.md).
- **Owner & admins** — the owner is whoever first connected qURL to the
  workspace. Owners and admins can run the `/qurl-admin` commands.

## Commands

### Everyone — `/qurl`

| Command | What it does |
|---------|--------------|
| `/qurl setup <email>` | Connect qURL to your workspace. The first person to run it becomes the owner and is the only one who can re-run it. |
| `/qurl setup <email> --rotate` | Owner-only: revoke the stored workspace key and replace it with a new one, on the same qURL account. Signing in with a *different* qURL account is detected and routed to an operator-assisted transfer (same as `--repoint`), not silently rotated. |
| `/qurl setup <email> --repoint` | Owner-only: move the workspace to a different qURL account. Signing in with the account that already holds the key behaves like `--rotate`; a genuine cross-account move is detected and routed to an operator-assisted transfer (qURL does not let one account take over another's workspace connection automatically). |
| `/qurl get <$id\|$alias>` | Mint a one-time qURL link for a resource in this channel. |
| `/qurl get <$id\|$alias> dm:true` | Mint the link and DM it to you instead of posting it in the channel. |
| `/qurl get <$id\|$alias> reason:"…"` | Mint the link and record a reason in the audit log. |
| `/qurl list` | List the resources available to you in this channel. |
| `/qurl aliases` | List this channel's aliases and the resource each one points to. |
| `/qurl uninstall` | Owner/admin-gated: disconnect qURL from this workspace's Slack commands. This does not revoke the upstream qURL API key. |
| `/qurl feedback` | Send a bug report or feature request to the qURL team. |
| `/qurl help` | Show the user command help. |

If qURL is disconnected and the recorded workspace owner is unavailable,
contact your qURL operator to recover or reassign ownership before reconnecting.

### Admins — `/qurl-admin`

Admin commands live under a separate `/qurl-admin` slash command. They're
enforced by the Secure Access Agent: only the owner and admins can run them.

Most setup runs through `/qurl-admin protect`, a guided picker. The typed
command variants below it are for power users and scripting.

**Protect resources**

| Command | What it does |
|---------|--------------|
| `/qurl-admin protect` | Guided picker — choose **qURL Connector** or **URL**, then fill in a short form. The recommended starting point. |
| `/qurl-admin protect-connector` | Guided setup for a qURL Connector; opens a form and returns copy-paste deploy steps. |
| `/qurl-admin protect-connector <id> [env:…] [port:8080] [alias:$alias]` | Typed connector setup for power users. |
| `/qurl-admin protect-url` | Guided setup to protect an existing URL resource. |
| `/qurl-admin protect-url $<alias> [as:$channel-alias]` | Typed: protect a URL resource that **already has an alias** in this channel. |
| `/qurl-admin protect-url url:<target-url> as:$channel-alias` | Typed: protect a URL that **has no alias yet** by its target URL. |

**Aliases**

| Command | What it does |
|---------|--------------|
| `/qurl-admin set-alias $<alias> $<id>` | Point an alias at a resource in this channel. |
| `/qurl-admin unset-alias $<alias>` | Remove an alias from this channel. |

**Manage resources**

| Command | What it does |
|---------|--------------|
| `/qurl-admin set-display-name $<id> <name>` | Set the friendly name shown in `/qurl list`. |
| `/qurl-admin unset-display-name $<id>` | Reset a resource's display name to the default. |
| `/qurl-admin revoke $<id>` | Revoke a protected resource and all of its qURLs. |

**Admins**

| Command | What it does |
|---------|--------------|
| `/qurl-admin add @user` | Promote a Slack user to admin. |
| `/qurl-admin remove @user` | Demote a Slack user from admin. |
| `/qurl-admin admins` | List the owner and the current admins. |
| `/qurl-admin help` | Show the admin command help. |

## Protecting a resource

`/qurl-admin protect` is the guided entry point. It asks whether you're
exposing a **qURL Connector** or an existing **URL**, then walks you through a
short form. Both make the resource available **in the current channel** — to
reach more channels, use the **Edit** button on the resource's `/qurl list`
row and pick additional channels.

**qURL Connector** — for a service that runs in your own environment. The
guided form asks for the connector ID, an optional channel alias, the local
port, and where you'll run it (Docker, Docker Compose, ECS/Fargate, or
Kubernetes). qURL replies with copy-paste deploy steps tailored to that
choice, plus a short-lived bootstrap key. Remove the bootstrap key from your
environment once the connector logs show it has connected. If you run the
paste block from a non-interactive shell, set `QURL_BOOTSTRAP_KEY` from your
secret manager instead of typing it at the prompt. If you generate setup more
than once, use the newest Slack message and discard older install blocks.

**URL resource** — for an existing web URL. Point an alias at it and it's
immediately available for `/qurl get` in the channel.

## Sharing a link

`/qurl get $id` mints a one-time qURL link for any resource available in the
current channel (pass a resource `$id` or a channel `$alias`). The reply
includes how long the link stays valid. Every link is single-use: it burns on
first open. Add `dm:true` to receive the link privately, or `reason:"…"` to
note why you minted it in the audit log.

## FAQ

**"qURL isn't connected to this workspace yet."** Someone needs to run
`/qurl setup <email>` first. The first person to do so becomes the owner.

**I can't re-run `/qurl setup`.** Only the owner — the person who first
connected qURL — can re-run setup. This stops the workspace from being
re-pointed at a different qURL account. Ask the owner, or use the
`/qurl-admin` commands for everyday admin tasks.

**How do I rotate the workspace key?** The owner runs
`/qurl setup <email> --rotate`, then completes the Auth0 prompt. Slack revokes
the old workspace key before storing the replacement, so a failed rotation does
not leave an extra live key behind.
If rotation fails after revoking the old key, retry with `--rotate` or
`--repoint`; plain setup will not mint around the stored old key identity.
If that old identity was deleted at qURL rather than revoked, Slack fails closed
until an operator verifies the stale identity or rotates the key from qURL
account/API-key management.
Workspaces connected before Slack stored qURL key identity may need to rotate
from the qURL dashboard first; Slack refuses to guess which key to revoke.

**How do I move the workspace to a different qURL account?** The owner runs
`/qurl setup <email> --repoint` and signs in with the destination qURL account.
If that account already holds the key, `--repoint` just rotates it. If a
*different* qURL account holds it, Slack detects the cross-account move and
routes you to an operator-assisted transfer instead of silently leaving the old
account's key live alongside a new one — qURL deliberately does not let one
account take over another's workspace connection
([issue #790](https://github.com/layervai/qurl-integrations/issues/790)).
Workspaces connected before Slack recorded the qURL account behind the key can't
verify a cross-account move: the current owner can refresh with `--rotate`
(which records the account for next time), or contact LayerV support for the
transfer.

**A resource I expected isn't in `/qurl list`.** Resources are channel-scoped.
Run the command in the channel where the resource was protected, or ask an
admin to add this channel via the **Edit** button on the resource's
`/qurl list` row.

**`/qurl-admin` says I'm not allowed.** Admin commands are limited to the
owner and admins. Ask the owner to add you with `/qurl-admin add @you`.

**Guided connector setup says it needs the latest app install.** Ask a
workspace admin to open the qURL install link your operator provided to
reinstall the app, then run `/qurl-admin protect-connector` again.

**Some commands aren't in `/qurl help`.** Help only lists what your
workspace's Secure Access Agent deployment has enabled. Run `/qurl help` or
`/qurl-admin help` for the authoritative list.

## Self-hosting

Running the Secure Access Agent yourself? See [docs/operating.md](docs/operating.md) for
endpoints, environment variables, Slack app configuration, and local
development.

## License

[MIT](../../LICENSE) — Copyright (c) 2025-present LayerV, Inc.
